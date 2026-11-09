package relay

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestUnaryCallIsForwardedWithTheAccountsToken(t *testing.T) {
	type seen struct {
		auth, custom, ctype, body, proto, ghost, xff string
	}
	got := make(chan seen, 1)
	e := newEnv(t, envOpts{api: func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{r.Header.Get("Authorization"), r.Header.Get("X-Cursor-Custom"), r.Header.Get("Content-Type"),
			string(b), r.Proto, r.Header.Get("X-Ghost-Mode"), r.Header.Get("X-Forwarded-For")}
		w.Header().Set("X-Reply", "yes")
		w.Header().Set("Trailer", "X-End")
		w.Header().Set("Content-Type", "application/proto")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "reply-body")
		w.Header().Set("X-End", "done")
	}})
	token := e.token()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, e.srv.URL+"/aiserver.v1.AiService/Something", strings.NewReader("request-body"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Cursor-Custom", "custom-value")
	req.Header.Set("X-Ghost-Mode", "true")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("Content-Type", "application/proto")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	s := <-got
	if want := "Bearer " + e.upstreamToken(); s.auth != want {
		t.Errorf("upstream saw Authorization %q, want the account's token", s.auth)
	}
	if s.custom != "custom-value" || s.ctype != "application/proto" || s.body != "request-body" || s.ghost != "true" {
		t.Errorf("client headers or body changed on the way: %+v", s)
	}
	if s.xff != "" {
		t.Errorf("X-Forwarded-For %q reached Cursor", s.xff)
	}
	if resp.StatusCode != http.StatusAccepted || string(body) != "reply-body" || resp.Header.Get("X-Reply") != "yes" {
		t.Errorf("response changed: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if resp.Trailer.Get("X-End") != "done" {
		t.Errorf("trailer lost: %v", resp.Trailer)
	}
}

func TestBidirectionalStreamFlowsBothWaysAtOnce(t *testing.T) {
	type seen struct{ auth, proto string }
	got := make(chan seen, 1)
	e := newEnv(t, envOpts{agent: func(w http.ResponseWriter, r *http.Request) {
		got <- seen{r.Header.Get("Authorization"), r.Proto}
		echoStream(w, r)
	}})
	token := e.token()

	pr, pw := io.Pipe()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, e.srv.URL+"/agent.v1.AgentService/Run", pr)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/connect+proto")
	// Do returns when the response headers arrive, while the request body is still open and empty.
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Errorf("client side is %s, want HTTP/2", resp.Proto)
	}
	rd := bufio.NewReader(resp.Body)
	for _, msg := range []string{"one", "two", "three"} {
		if _, err := io.WriteString(pw, msg+"\n"); err != nil {
			t.Fatal(err)
		}
		line, err := rd.ReadString('\n')
		if err != nil || line != "echo:"+msg+"\n" {
			t.Fatalf("read %q, %v; want the echo of %q before the next message", line, err, msg)
		}
	}
	_ = pw.Close()
	if rest, _ := io.ReadAll(rd); len(rest) != 0 {
		t.Errorf("unexpected tail %q", rest)
	}
	s := <-got
	if s.auth != "Bearer "+e.upstreamToken() || s.proto != "HTTP/2.0" {
		t.Errorf("upstream saw %+v, want the account's token over HTTP/2", s)
	}
}

func TestClientCancelCancelsTheUpstreamStream(t *testing.T) {
	ended := make(chan struct{})
	started := make(chan struct{})
	e := newEnv(t, envOpts{agent: func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // the request is complete: only the response is still flowing
		w.WriteHeader(http.StatusOK)
		_ = http.NewResponseController(w).Flush()
		close(started)
		<-r.Context().Done()
		close(ended)
	}})
	token := e.token()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.srv.URL+"/agent.v1.AgentService/Run", strings.NewReader("frame"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/connect+proto")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-started
	cancel()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream stream stayed open after the client cancelled")
	}
}

func TestPublicAPIGoesUpstreamWithTheUserAPIKey(t *testing.T) {
	type seen struct{ auth, path, query string }
	got := make(chan seen, 4)
	e := newEnv(t, envOpts{public: func(w http.ResponseWriter, r *http.Request) {
		got <- seen{r.Header.Get("Authorization"), r.URL.Path, r.URL.RawQuery}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}})

	resp, body := e.do(http.MethodGet, "/v1/models?limit=3", testClientKey, "", "")
	if resp.StatusCode != http.StatusOK || body != `{"ok":true}` {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	s := <-got
	if s.auth != "Bearer "+testUserKey || s.path != "/v1/models" || s.query != "limit=3" {
		t.Errorf("public upstream saw %+v", s)
	}
	if n := e.exchanges.Load(); n != 0 {
		t.Errorf("/v1 needs no session token, but %d exchanges happened", n)
	}

	// A key the gateway refuses never reaches the public API.
	before := e.public.calls.Load()
	if resp, _ := e.do(http.MethodGet, "/v1/me", "gw_unknown_key", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unknown key: status %d, want 401", resp.StatusCode)
	}
	// A relay token is not the client key: /v1 takes the raw key only.
	if resp, _ := e.do(http.MethodGet, "/v1/me", e.token(), "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("relay token on /v1: status %d, want 401", resp.StatusCode)
	}
	if e.public.calls.Load() != before {
		t.Error("a refused call reached the public API")
	}
}

func TestUpstream401RenewsTheTokenOnceAndRetriesUnaryCalls(t *testing.T) {
	var rec list
	e := newEnv(t, envOpts{})
	e.setAPI(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if rec.add(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "ok:"+string(b))
	})
	token := e.token()
	first := e.upstreamToken()
	e.clock.Advance(time.Minute) // beyond the minimum gap between two renewals

	resp, body := e.do(http.MethodPost, "/aiserver.v1.AiService/Something", token, "application/proto", "payload")
	if resp.StatusCode != http.StatusOK || body != "ok:payload" {
		t.Fatalf("got %d %q, want the retried call's answer with its body replayed", resp.StatusCode, body)
	}
	tokens := rec.all()
	if len(tokens) != 2 || tokens[0] != first || tokens[1] != e.upstreamToken() || tokens[0] == tokens[1] {
		t.Errorf("upstream saw tokens %v, want the stale one and then a fresh one", tokens)
	}
	if n := e.exchanges.Load(); n != 2 {
		t.Errorf("%d exchanges, want 2 (first token, one renewal)", n)
	}
}

func TestUpstream401AfterTheRetryPassesThrough(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.setAPI(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	token := e.token()
	e.clock.Advance(time.Minute)

	resp, _ := e.do(http.MethodPost, "/aiserver.v1.AiService/Something", token, "application/proto", "payload")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status %d, want the upstream 401", resp.StatusCode)
	}
	if n := e.exchanges.Load(); n != 2 {
		t.Errorf("%d exchanges, want 2: one renewal, not a loop", n)
	}
	if n := e.api.calls.Load(); n != 4 {
		t.Errorf("api saw %d calls, want 4 (2 exchanges, the call and its single retry)", n)
	}
}

func TestAPersistent401DoesNotTriggerAnExchangePerRequest(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.setAPI(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	token := e.token()
	for range 3 {
		if resp, _ := e.do(http.MethodPost, "/aiserver.v1.AiService/Something", token, "application/proto", "x"); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status %d, want the upstream 401", resp.StatusCode)
		}
	}
	if n := e.exchanges.Load(); n != 1 {
		t.Errorf("%d exchanges, want 1: the token was minted moments ago", n)
	}
}

func TestStreamsAreNotRetriedButRenewTheTokenForTheNextCall(t *testing.T) {
	e := newEnv(t, envOpts{agent: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }})
	token := e.token()
	e.clock.Advance(time.Minute)

	resp, _ := e.do(http.MethodPost, "/agent.v1.AgentService/Run", token, "application/connect+proto", "frame")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status %d, want the upstream 401", resp.StatusCode)
	}
	if n := e.agent.calls.Load(); n != 1 {
		t.Errorf("agent saw %d calls, want 1: a stream is not replayed", n)
	}
	if n := e.exchanges.Load(); n != 2 {
		t.Errorf("%d exchanges, want 2: the next call needs a fresh token", n)
	}
}

func TestRenewalHappensBeforeTheTokenExpires(t *testing.T) {
	var rec list
	e := newEnv(t, envOpts{})
	e.setAPI(func(w http.ResponseWriter, r *http.Request) { rec.add(r.Header.Get("Authorization")) })
	token := e.token()
	first := e.upstreamToken()

	e.clock.Advance(30 * time.Minute)
	e.do(http.MethodPost, "/aiserver.v1.AiService/A", token, "application/proto", "x")
	e.clock.Advance(26 * time.Minute) // 4 minutes before expiry: inside the renewal margin
	e.do(http.MethodPost, "/aiserver.v1.AiService/B", token, "application/proto", "x")

	auths := rec.all()
	if len(auths) != 2 || auths[0] != "Bearer "+first || auths[1] != "Bearer "+e.upstreamToken() || auths[1] == auths[0] {
		t.Errorf("tokens used %v, want the first then a renewed one", auths)
	}
	// The client's own token expires with the first upstream token: it must exchange again.
	e.clock.Advance(10 * time.Minute)
	if resp, _ := e.do(http.MethodPost, "/aiserver.v1.AiService/C", token, "application/proto", "x"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expired relay token: status %d, want 401", resp.StatusCode)
	}
}

func TestAFailedRenewalKeepsServingTheTokenUntilItExpires(t *testing.T) {
	e := newEnv(t, envOpts{api: func(w http.ResponseWriter, _ *http.Request) {}})
	token := e.token()
	first := e.upstreamToken()
	e.failExchange.Store(true)
	e.clock.Advance(56 * time.Minute)

	if resp, _ := e.do(http.MethodPost, "/aiserver.v1.AiService/A", token, "application/proto", "x"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want the call served with the token still valid", resp.StatusCode)
	}
	if e.upstreamToken() != first {
		t.Error("a token appeared although the exchange fails")
	}
	e.clock.Advance(10 * time.Minute)
	if e.relay.tokens.held() {
		t.Error("the expired token is still reported as held")
	}
	if resp, _ := e.do(http.MethodGet, "/healthz", "", "", ""); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("healthz status %d once the token expired, want 503", resp.StatusCode)
	}
	// With no valid token left and no way to get one, a new client cannot be served.
	if _, status := e.exchange(testClientKey); status != http.StatusServiceUnavailable {
		t.Errorf("exchange with an expired upstream token: status %d, want 503", status)
	}
}

func TestHealthzReportsTheUpstreamToken(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.failExchange.Store(true)
	resp, body := e.do(http.MethodGet, "/healthz", "", "", "")
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, `"upstreamToken":false`) {
		t.Errorf("without a token: %d %q", resp.StatusCode, body)
	}
	e.failExchange.Store(false)
	if err := e.relay.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	resp, body = e.do(http.MethodGet, "/healthz", "", "", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"upstreamToken":true`) {
		t.Errorf("with a token: %d %q", resp.StatusCode, body)
	}
	if e.verifierCalls.Load() != 0 || e.api.calls.Load() != 1 {
		t.Error("healthz must not need a client key")
	}
}

// echoStream answers its headers at once, then echoes each line it receives.
func echoStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/connect+proto")
	w.WriteHeader(http.StatusOK)
	_ = http.NewResponseController(w).Flush()
	sc := bufio.NewScanner(r.Body)
	for sc.Scan() {
		_, _ = io.WriteString(w, "echo:"+sc.Text()+"\n")
		_ = http.NewResponseController(w).Flush()
	}
}
