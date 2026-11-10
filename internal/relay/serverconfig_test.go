package relay

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

// serverConfigProto is a GetServerConfigResponse as the protocol defines it: field 27 is the agent URL config
// (agent_url 1, agentn_url 2, and a third URL a newer client knows), around fields that must not change.
func serverConfigProto(agent, agentn, third string) []byte {
	var cfg []byte
	for i, u := range []string{agent, agentn, third} {
		cfg = protowire.AppendTag(cfg, protowire.Number(i+1), protowire.BytesType)
		cfg = protowire.AppendString(cfg, u)
	}
	cfg = protowire.AppendTag(cfg, 9, protowire.VarintType)
	cfg = protowire.AppendVarint(cfg, 7)

	var m []byte
	m = protowire.AppendTag(m, 1, protowire.BytesType)
	m = protowire.AppendString(m, "bug-config")
	m = protowire.AppendTag(m, 6, protowire.BytesType)
	m = protowire.AppendString(m, "config-version-1")
	m = protowire.AppendTag(m, 27, protowire.BytesType)
	m = protowire.AppendBytes(m, cfg)
	m = protowire.AppendTag(m, 28, protowire.VarintType)
	m = protowire.AppendVarint(m, 1)
	// A URL elsewhere in the message is none of the relay's business.
	m = protowire.AppendTag(m, 36, protowire.BytesType)
	m = protowire.AppendString(m, "https://retrieval.example.test")
	return m
}

func serverConfigJSON(agent, agentn, third string) []byte {
	b, _ := json.Marshal(map[string]any{
		"agentUrlConfig": map[string]any{"agentUrl": agent, "agentnUrl": agentn, "localLoopInferenceUrl": third},
		"configVersion":  "config-version-1",
		"retrievalUrl":   "https://retrieval.example.test",
	})
	return b
}

func gz(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Bytes()
}

func TestGetServerConfigPointsEveryAgentHostAtTheRelay(t *testing.T) {
	const (
		agentn = "https://agentn.example.test"
		third  = "https://loop.example.test"
	)
	var e *env
	cases := []struct {
		name, contentType string
		gzip, json        bool
	}{
		{"proto", "application/proto", false, false},
		{"gzip proto", "application/proto", true, false},
		{"json", "application/json", false, true},
		{"gzip json", "application/json", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e = newEnv(t, envOpts{api: func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Accept-Encoding"); got != "gzip" {
					t.Errorf("upstream was asked for %q, want gzip only", got)
				}
				build := serverConfigProto
				if tc.json {
					build = serverConfigJSON
				}
				body := build(e.agent.URL, agentn, third)
				w.Header().Set("Content-Type", tc.contentType)
				if tc.gzip {
					w.Header().Set("Content-Encoding", "gzip")
					body = gz(body)
				}
				_, _ = w.Write(body)
			}})
			token := e.token()
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, e.srv.URL+serverConfigPath, bytes.NewReader([]byte("{}")))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", tc.contentType)
			req.Header.Set("Accept-Encoding", "br")
			resp, err := e.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)

			if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "" || resp.ContentLength != int64(len(body)) {
				t.Fatalf("status %d, encoding %q, length %d for %d bytes", resp.StatusCode, resp.Header.Get("Content-Encoding"), resp.ContentLength, len(body))
			}
			url := e.srv.URL
			if tc.json {
				var got, want map[string]any
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatalf("body is not JSON: %v", err)
				}
				_ = json.Unmarshal(serverConfigJSON(url, url, url), &want)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("got %s", body)
				}
				return
			}
			if want := serverConfigProto(url, url, url); !bytes.Equal(body, want) {
				t.Errorf("got %x\nwant %x", body, want)
			}
		})
	}
}

func TestRunsFollowTheHostsCursorAnnounced(t *testing.T) {
	var e *env
	e = newEnv(t, envOpts{
		api: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/proto")
			// The public upstream stands in for the second run host.
			_, _ = w.Write(serverConfigProto(e.agent.URL, e.public.URL, ""))
		},
		agent:  func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "agent") },
		public: func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "agentn") },
		mutate: func(c *Config) { c.AgentURL = "https://127.0.0.1:1" }, // nothing listens there
	})
	token := e.token()
	e.do(http.MethodPost, serverConfigPath, token, "application/proto", "{}")

	run := func(ghost string) string {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, e.srv.URL+"/agent.v1.AgentService/Run", bytes.NewReader([]byte("x")))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/connect+proto")
		req.Header.Set("X-Ghost-Mode", ghost)
		resp, err := e.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if got := run("true"); got != "agent" {
		t.Errorf("ghost mode ran on %q, want the announced agentUrl", got)
	}
	if got := run("false"); got != "agentn" {
		t.Errorf("normal mode ran on %q, want the announced agentnUrl", got)
	}
}

func TestAServerConfigTheRelayCannotReadIsNotPassedOn(t *testing.T) {
	for name, body := range map[string][]byte{"proto": {0xff, 0xff, 0xff}, "json": []byte(`{"leak-marker":`)} {
		e := newEnv(t, envOpts{api: func(w http.ResponseWriter, _ *http.Request) {
			if name == "json" {
				w.Header().Set("Content-Type", "application/json")
			}
			_, _ = w.Write(body)
		}})
		resp, got := e.do(http.MethodPost, serverConfigPath, e.token(), "application/proto", "{}")
		if resp.StatusCode != http.StatusBadGateway || bytes.Contains([]byte(got), body) {
			t.Errorf("%s: status %d body %q, want 502 without the upstream body", name, resp.StatusCode, got)
		}
	}
}

func TestAGzipServerConfigOverTheCapIsNotPassedOn(t *testing.T) {
	// Padding that ends exactly at the cap, then the agent hosts: a cut at the cap still parses, without them.
	var m []byte
	record := protowire.AppendTag(nil, 999, protowire.BytesType)
	record = protowire.AppendBytes(record, make([]byte, 1024-len(record)-2))
	for len(m) < maxServerConfigBytes {
		m = append(m, record...)
	}
	m = append(m[:maxServerConfigBytes], serverConfigProto("https://agent.example.test", "https://agentn.example.test", "https://third.example.test")...)
	body := gz(m)
	e := newEnv(t, envOpts{api: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(body)
	}})
	resp, _ := e.do(http.MethodPost, serverConfigPath, e.token(), "application/proto", "{}")
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status %d, want 502", resp.StatusCode)
	}
}
