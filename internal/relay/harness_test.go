package relay

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Invented values: nothing here is, or looks like, a real credential.
const (
	testUserKey   = "crsr_user_key_for_tests_0001"
	testSealKey   = "seal-key-for-tests-0123456789abcdef-xyz"
	testClientKey = "gw_client_key_for_tests_0001"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// upstream is a fake Cursor host: an HTTP/2 TLS server that counts its calls.
type upstream struct {
	*httptest.Server
	calls atomic.Int64
}

func newUpstream(t *testing.T, h func(http.ResponseWriter, *http.Request)) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.calls.Add(1)
		h(w, r)
	}))
	u.EnableHTTP2 = true
	u.StartTLS()
	t.Cleanup(u.Close)
	return u
}

func notFound(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) }

// env is a relay in front of fake upstreams and a fake verifier.
type env struct {
	t      *testing.T
	clock  *fakeClock
	logs   *syncBuffer
	relay  *Relay
	srv    *httptest.Server
	client *http.Client
	pool   *x509.CertPool

	api, agent, public *upstream
	verifier           *httptest.Server
	verifierCalls      atomic.Int64
	verifierStatus     atomic.Int64 // 0: answer by key
	exchanges          atomic.Int64
	failExchange       atomic.Bool

	mu        sync.Mutex
	validKeys map[string]bool
	upToken   string // the last token the fake exchange issued
	apiFn     func(http.ResponseWriter, *http.Request)
}

type envOpts struct {
	api, agent, public func(http.ResponseWriter, *http.Request)
	mutate             func(*Config)
}

func newEnv(t *testing.T, o envOpts) *env {
	t.Helper()
	e := &env{
		t:         t,
		clock:     &fakeClock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)},
		logs:      &syncBuffer{},
		validKeys: map[string]bool{testClientKey: true},
	}
	pick := func(f func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
		if f == nil {
			return notFound
		}
		return f
	}
	e.apiFn = pick(o.api)
	e.api = newUpstream(t, e.apiHandler)
	e.agent = newUpstream(t, pick(o.agent))
	e.public = newUpstream(t, pick(o.public))
	e.verifier = httptest.NewServer(http.HandlerFunc(e.verify))
	t.Cleanup(e.verifier.Close)

	e.pool = x509.NewCertPool()
	e.pool.AddCert(e.api.Certificate())

	cfg := Config{
		UserAPIKey:   testUserKey,
		SealKey:      testSealKey,
		VerifierURL:  e.verifier.URL,
		APIURL:       e.api.URL,
		AgentURL:     e.agent.URL,
		PublicAPIURL: e.public.URL,
		UpstreamTLS:  &tls.Config{RootCAs: e.pool},
		Now:          e.clock.Now,
	}
	e.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { e.relay.ServeHTTP(w, r) }))
	e.srv.EnableHTTP2 = true
	e.srv.StartTLS()
	t.Cleanup(e.srv.Close)
	cfg.PublicURL = e.srv.URL
	if o.mutate != nil {
		o.mutate(&cfg)
	}
	var err error
	e.relay, err = New(cfg, slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	e.client = &http.Client{Transport: &http.Transport{
		TLSClientConfig:    &tls.Config{RootCAs: e.pool},
		ForceAttemptHTTP2:  true,
		DisableCompression: true,
	}}
	t.Cleanup(e.client.CloseIdleConnections)
	return e
}

// apiHandler is the fake api2: it exchanges the user API key and hands anything else to apiFn.
func (e *env) apiHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != exchangePath {
		e.mu.Lock()
		f := e.apiFn
		e.mu.Unlock()
		f(w, r)
		return
	}
	if e.failExchange.Load() || r.Header.Get("Authorization") != "Bearer "+testUserKey {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	n := e.exchanges.Add(1)
	tok := upstreamJWT(e.clock.Now().Add(time.Hour), int(n))
	e.mu.Lock()
	e.upToken = tok
	e.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": tok, "refreshToken": tok})
}

func (e *env) setAPI(f func(http.ResponseWriter, *http.Request)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.apiFn = f
}

func (e *env) upstreamToken() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.upToken
}

func (e *env) setKeyValid(key string, valid bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.validKeys[key] = valid
}

func (e *env) verify(w http.ResponseWriter, r *http.Request) {
	e.verifierCalls.Add(1)
	if s := e.verifierStatus.Load(); s != 0 {
		w.WriteHeader(int(s))
		return
	}
	e.mu.Lock()
	ok := e.validKeys[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	e.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
	}
}

func upstreamJWT(exp time.Time, n int) string {
	enc := base64.RawURLEncoding.EncodeToString
	return fmt.Sprintf("%s.%s.upstream-signature-%d",
		enc([]byte(`{"alg":"HS256","typ":"JWT"}`)),
		enc(fmt.Appendf(nil, `{"sub":"user_test","jti":"%d","exp":%d}`, n, exp.Unix())),
		n)
}

// exchange trades key for a relay token the way a Cursor client does.
func (e *env) exchange(key string) (token string, status int) {
	e.t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, e.srv.URL+exchangePath, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode
	}
	var tokens struct{ AccessToken, RefreshToken string }
	if err := json.NewDecoder(resp.Body).Decode(&tokens); err != nil {
		e.t.Fatal(err)
	}
	if tokens.AccessToken == "" || tokens.AccessToken != tokens.RefreshToken {
		e.t.Fatalf("exchange answered %+v, want the same non-empty token twice", tokens)
	}
	return tokens.AccessToken, resp.StatusCode
}

func (e *env) token() string {
	e.t.Helper()
	tok, status := e.exchange(testClientKey)
	if status != http.StatusOK {
		e.t.Fatalf("exchange: status %d", status)
	}
	return tok
}

// reply is what a test keeps of a response whose body it has read.
type reply struct {
	StatusCode int
	Header     http.Header
}

// do sends a request with the given bearer and returns the reply and its body.
func (e *env) do(method, path, bearer, contentType, body string) (reply, string) {
	e.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, e.srv.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return reply{resp.StatusCode, resp.Header}, string(out)
}

// list is a goroutine-safe record of what a fake upstream saw.
type list struct {
	mu sync.Mutex
	v  []string
}

func (l *list) add(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.v = append(l.v, s)
	return len(l.v)
}

func (l *list) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.v...)
}
