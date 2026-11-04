package cursor

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/mediavee/cpa-cursor/internal/gen/agent/v1"
)

// D3: the headers of the official CLI, plus the Connect framing headers that
// connect-es adds to every streaming call.
func TestRunHeadersAreExactlyTheCLIs(t *testing.T) {
	up := newFakeUpstream(t)
	cfg := up.config()
	cfg.ClientVersion = "cli-2099.01.02-abcdef0"
	cfg.GhostMode = true
	c, err := NewClient(cfg, WithTLSConfig(clientTLS(up)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.CloseIdleConnections)

	s, err := c.Run(context.Background(), "secret-token", RunParams{Request: runRequest("hi"), RequestID: "11111111-1111-4111-8111-111111111111"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fs := up.accept()
	h := fs.req.Header

	want := map[string]string{
		"authorization":            "Bearer secret-token",
		"x-ghost-mode":             "true",
		"x-cursor-client-version":  "cli-2099.01.02-abcdef0",
		"x-cursor-client-type":     "cli",
		"x-request-id":             "11111111-1111-4111-8111-111111111111",
		"x-original-request-id":    "11111111-1111-4111-8111-111111111111",
		"user-agent":               "connect-es/1.6.1",
		"content-type":             "application/connect+proto",
		"connect-protocol-version": "1",
		"connect-content-encoding": "gzip",
		"connect-accept-encoding":  "gzip",
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if key := h.Get("x-blob-encryption-key"); !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(key) {
		t.Errorf("x-blob-encryption-key = %q, want 64 hex characters", key)
	}

	wantKeys := []string{"x-blob-encryption-key"}
	for k := range want {
		wantKeys = append(wantKeys, k)
	}
	slices.Sort(wantKeys)
	if got := lowerKeys(h); !slices.Equal(got, wantKeys) {
		t.Errorf("header set = %v\nwant         %v", got, wantKeys)
	}
	for _, forbidden := range []string{"x-session-id", "x-client-key", "x-cursor-checksum", "x-cursor-agent-allowed-tools", "accept-encoding"} {
		if h.Get(forbidden) != "" {
			t.Errorf("forbidden header %s sent", forbidden)
		}
	}
}

func TestOriginalRequestIDSurvivesRetries(t *testing.T) {
	up := newFakeUpstream(t)
	c := up.client(t, SystemClock{})
	s, err := c.Run(context.Background(), "t", RunParams{Request: runRequest("hi"), OriginalRequestID: "first-attempt"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := up.accept().req.Header
	if h.Get("x-original-request-id") != "first-attempt" || h.Get("x-request-id") != s.RequestID() || s.RequestID() == "first-attempt" {
		t.Errorf("request ids: %q / %q (session %q)", h.Get("x-request-id"), h.Get("x-original-request-id"), s.RequestID())
	}
}

func TestBlobKeyIsPerConversation(t *testing.T) {
	up := newFakeUpstream(t)
	c := up.client(t, SystemClock{})
	keys := map[string]bool{}
	for range 3 {
		s, err := c.Run(context.Background(), "t", RunParams{Request: runRequest("hi")})
		if err != nil {
			t.Fatal(err)
		}
		keys[up.accept().req.Header.Get("x-blob-encryption-key")] = true
		s.Close()
	}
	if len(keys) != 3 {
		t.Errorf("blob encryption keys repeat: %v", keys)
	}
	explicit := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if _, err := hex.DecodeString(explicit); err != nil {
		t.Fatal(err)
	}
	s, _ := c.Run(context.Background(), "t", RunParams{Request: runRequest("hi"), BlobEncryptionKey: explicit})
	defer s.Close()
	if got := up.accept().req.Header.Get("x-blob-encryption-key"); got != explicit {
		t.Errorf("explicit key not used: %q", got)
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	c, err := NewClient(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(context.Background(), "", RunParams{Request: runRequest("x")}); err == nil {
		t.Error("empty token accepted")
	}
	if _, err := c.Run(context.Background(), "t", RunParams{}); err == nil {
		t.Error("missing run request accepted")
	}
}

// The server refuses or ignores these on ordinary accounts (spec, spike findings).
func TestRunRefusesForbiddenPromptFields(t *testing.T) {
	c, err := NewClient(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*agentv1.AgentRunRequest){
		"custom_system_prompt":      func(r *agentv1.AgentRunRequest) { r.CustomSystemPrompt = proto.String("x") },
		"exclude_workspace_context": func(r *agentv1.AgentRunRequest) { r.ExcludeWorkspaceContext = proto.Bool(true) },
		"system_prompt_spec":        func(r *agentv1.AgentRunRequest) { r.SystemPromptSpec = &agentv1.SystemPromptSpec{} },
	} {
		req := runRequest("x")
		mutate(req)
		if _, err := c.Run(context.Background(), "t", RunParams{Request: req}); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	bad := DefaultConfig()
	bad.APIURL = "http://api.example.com"
	bad.ClientVersion = ""
	if _, err := NewClient(bad); err == nil {
		t.Fatal("invalid config accepted")
	}
	if _, err := NewClient(DefaultConfig()); err != nil {
		t.Fatalf("default config rejected: %v", err)
	}
}

func TestRunGoesThroughTheEnvironmentProxy(t *testing.T) {
	up := newFakeUpstream(t)
	proxy := newConnectProxy(t)
	c := up.client(t, SystemClock{})
	c.proxy = func(*http.Request) (*url.URL, error) { return &url.URL{Scheme: "http", Host: proxy.addr}, nil }

	s, err := c.Run(context.Background(), "t", RunParams{Request: runRequest("hi")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fs := up.accept()
	fs.recv()
	fs.send(textMsg("via proxy"), false)
	if ev, ok := nextEvent(t, s).(UpdateEvent); !ok || ev.Update.GetTextDelta().GetText() != "via proxy" {
		t.Fatalf("event = %v", ev)
	}
	if got := proxy.targets(); !slices.Equal(got, []string{up.srv.Listener.Addr().String()}) {
		t.Errorf("CONNECT targets = %v", got)
	}
}

func TestProxyRefusalIsAnError(t *testing.T) {
	up := newFakeUpstream(t)
	c := up.client(t, SystemClock{})
	deny := newConnectProxy(t)
	deny.status = http.StatusForbidden
	c.proxy = func(*http.Request) (*url.URL, error) { return &url.URL{Scheme: "http", Host: deny.addr}, nil }
	s, err := c.Run(context.Background(), "t", RunParams{Request: runRequest("hi")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := nextErr(t, s); !strings.Contains(err.Error(), "proxy CONNECT") || errors.Is(err, io.EOF) {
		t.Fatalf("err = %v", err)
	}
}

// connectProxy is a minimal HTTP CONNECT proxy.
type connectProxy struct {
	addr   string
	status int
	mu     sync.Mutex
	seen   []string
}

func newConnectProxy(t *testing.T) *connectProxy {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &connectProxy{addr: ln.Addr().String(), status: http.StatusOK}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go p.handle(conn)
		}
	}()
	return p
}

func (p *connectProxy) targets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.seen)
}

func (p *connectProxy) handle(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil || req.Method != http.MethodConnect {
		return
	}
	p.mu.Lock()
	p.seen = append(p.seen, req.Host)
	p.mu.Unlock()
	if p.status != http.StatusOK {
		_, _ = io.WriteString(conn, "HTTP/1.1 "+http.StatusText(p.status)+"\r\n\r\n")
		return
	}
	upstream, err := (&net.Dialer{}).DialContext(req.Context(), "tcp", req.Host)
	if err != nil {
		return
	}
	defer upstream.Close()
	_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, br); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, upstream); done <- struct{}{} }()
	<-done
}
