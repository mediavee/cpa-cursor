package relay

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
)

const (
	serverConfigPath = "/aiserver.v1.ServerConfigService/GetServerConfig"
	// maxServerConfigBytes bounds the response the relay buffers to rewrite it.
	maxServerConfigBytes = 8 << 20

	// agentUrlConfigField is GetServerConfigResponse.agent_url_config; AgentUrlConfig holds agent_url (1) and
	// agentn_url (2). The generated types do not cover aiserver.v1.ServerConfigService, so the message is
	// rewritten on the wire.
	agentUrlConfigField = 27
	agentURLField       = 1
	agentnURLField      = 2
)

// agentHosts are the run hosts Cursor announced, kept to route agent.v1 calls.
type agentHosts struct{ agent, agentn string }

// rewriteServerConfig replaces every URL of the response's agent URL config with publicURL and returns the
// hosts it replaced. body is the decoded response; json tells which encoding it has.
func rewriteServerConfig(body []byte, isJSON bool, publicURL string) (out []byte, hosts agentHosts, err error) {
	if isJSON {
		return rewriteServerConfigJSON(body, publicURL)
	}
	return rewriteServerConfigProto(body, publicURL)
}

func rewriteServerConfigProto(body []byte, publicURL string) ([]byte, agentHosts, error) {
	var hosts agentHosts
	out := make([]byte, 0, len(body))
	for rest := body; len(rest) > 0; {
		num, typ, n := protowire.ConsumeTag(rest)
		if n < 0 {
			return nil, hosts, protowire.ParseError(n)
		}
		m := protowire.ConsumeFieldValue(num, typ, rest[n:])
		if m < 0 {
			return nil, hosts, protowire.ParseError(m)
		}
		if num != agentUrlConfigField || typ != protowire.BytesType {
			out = append(out, rest[:n+m]...)
			rest = rest[n+m:]
			continue
		}
		inner, _ := protowire.ConsumeBytes(rest[n:])
		rewritten, h, err := rewriteAgentURLConfig(inner, publicURL)
		if err != nil {
			return nil, hosts, err
		}
		hosts = h
		out = protowire.AppendTag(out, num, typ)
		out = protowire.AppendBytes(out, rewritten)
		rest = rest[n+m:]
	}
	return out, hosts, nil
}

// rewriteAgentURLConfig replaces every string field holding an http(s) URL, so a field a newer client adds
// cannot send a run to Cursor directly.
func rewriteAgentURLConfig(msg []byte, publicURL string) ([]byte, agentHosts, error) {
	var hosts agentHosts
	out := make([]byte, 0, len(msg)+len(publicURL))
	for rest := msg; len(rest) > 0; {
		num, typ, n := protowire.ConsumeTag(rest)
		if n < 0 {
			return nil, hosts, protowire.ParseError(n)
		}
		m := protowire.ConsumeFieldValue(num, typ, rest[n:])
		if m < 0 {
			return nil, hosts, protowire.ParseError(m)
		}
		if typ == protowire.BytesType {
			v, _ := protowire.ConsumeBytes(rest[n:])
			if isHTTPURL(string(v)) {
				switch num {
				case agentURLField:
					hosts.agent = string(v)
				case agentnURLField:
					hosts.agentn = string(v)
				}
				out = protowire.AppendTag(out, num, typ)
				out = protowire.AppendString(out, publicURL)
				rest = rest[n+m:]
				continue
			}
		}
		out = append(out, rest[:n+m]...)
		rest = rest[n+m:]
	}
	return out, hosts, nil
}

func rewriteServerConfigJSON(body []byte, publicURL string) ([]byte, agentHosts, error) {
	var hosts agentHosts
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, hosts, err
	}
	for _, key := range []string{"agentUrlConfig", "agent_url_config"} {
		raw, ok := top[key]
		if !ok {
			continue
		}
		var cfg map[string]json.RawMessage
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, hosts, err
		}
		for name, v := range cfg {
			var s string
			if json.Unmarshal(v, &s) != nil || !isHTTPURL(s) {
				continue
			}
			switch name {
			case "agentUrl", "agent_url":
				hosts.agent = s
			case "agentnUrl", "agentn_url":
				hosts.agentn = s
			}
			cfg[name], _ = json.Marshal(publicURL)
		}
		top[key], _ = json.Marshal(cfg)
	}
	out, err := json.Marshal(top)
	return out, hosts, err
}

func isHTTPURL(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

// rewriteServerConfigResponse is the ReverseProxy hook for GetServerConfig: it decodes the body, replaces
// the agent hosts and answers uncompressed.
func (r *Relay) rewriteServerConfigResponse(resp *http.Response) error {
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxServerConfigBytes+1))
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("read the server config: %w", err)
	}
	if len(raw) > maxServerConfigBytes {
		return errors.New("the server config is too large")
	}
	switch enc := strings.ToLower(resp.Header.Get("Content-Encoding")); enc {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return fmt.Errorf("decode the server config: %w", err)
		}
		if raw, err = io.ReadAll(io.LimitReader(zr, maxServerConfigBytes)); err != nil {
			return fmt.Errorf("decode the server config: %w", err)
		}
	default:
		return fmt.Errorf("the server config uses the %q encoding", enc)
	}
	isJSON := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "json")
	body, hosts, err := rewriteServerConfig(raw, isJSON, r.cfg.PublicURL)
	if err != nil {
		return fmt.Errorf("rewrite the server config: %w", err)
	}
	r.hosts.remember(hosts)
	resp.Header.Del("Content-Encoding")
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.ContentLength = int64(len(body))
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return nil
}
