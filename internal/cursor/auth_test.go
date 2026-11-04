package cursor

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoginChallengeFollowsTheCLI(t *testing.T) {
	ch, err := NewLoginChallenge("https://cursor.example.com/")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(ch.URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme+"://"+u.Host+u.Path != "https://cursor.example.com/loginDeepControl" {
		t.Errorf("url = %s", ch.URL)
	}
	q := u.Query()
	if q.Get("mode") != "login" || q.Get("redirectTarget") != "cli" || q.Get("uuid") != ch.UUID {
		t.Errorf("query = %v", q)
	}
	raw, err := base64.RawURLEncoding.DecodeString(ch.Verifier)
	if err != nil || len(raw) != 32 {
		t.Errorf("verifier must be 32 random bytes in base64url, got %d bytes (%v)", len(raw), err)
	}
	sum := sha256.Sum256([]byte(ch.Verifier))
	if q.Get("challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Error("challenge is not base64url(sha256(verifier))")
	}
	other, _ := NewLoginChallenge("https://cursor.example.com")
	if other.Verifier == ch.Verifier || other.UUID == ch.UUID {
		t.Error("challenges repeat")
	}
}

// autoClock fires every timer as soon as it is created and records the delays.
type autoClock struct {
	SystemClock
	mu     sync.Mutex
	delays []time.Duration
}

func (a *autoClock) NewTimer(d time.Duration) Timer {
	a.mu.Lock()
	a.delays = append(a.delays, d)
	a.mu.Unlock()
	return SystemClock{}.NewTimer(0)
}

func pollServer(t *testing.T, statuses []int, body string) (*Client, *autoClock, *int) {
	t.Helper()
	n := new(int)
	var mu sync.Mutex
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		i := *n
		*n++
		mu.Unlock()
		if r.URL.Path != "/auth/poll" || r.Header.Get("Authorization") != "" || r.URL.Query().Get("uuid") == "" || r.URL.Query().Get("verifier") == "" {
			t.Errorf("bad poll request: %s auth=%q", r.URL, r.Header.Get("Authorization"))
		}
		st := statuses[min(i, len(statuses)-1)]
		w.WriteHeader(st)
		switch st {
		case http.StatusOK:
			_, _ = io.WriteString(w, body)
		case http.StatusForbidden:
			_, _ = io.WriteString(w, `{"error":"account_closed"}`)
		}
	}))
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	cfg := DefaultConfig()
	cfg.APIURL, cfg.SiteURL = srv.URL, srv.URL
	clk := &autoClock{}
	c, err := NewClient(cfg, WithClock(clk), WithTLSConfig(&tls.Config{RootCAs: pool}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.CloseIdleConnections)
	return c, clk, n
}

func TestPollLoginWaitsOn404ThenReturnsTokens(t *testing.T) {
	c, clk, n := pollServer(t, []int{404, 404, 404, 200}, `{"accessToken":"at","refreshToken":"rt"}`)
	ch, _ := c.NewLoginChallenge()
	tok, err := c.PollLogin(context.Background(), ch)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "at" || tok.RefreshToken != "rt" || *n != 4 {
		t.Errorf("tokens=%+v polls=%d", tok, *n)
	}
	// min(1000·1.2^n, 10000) ms
	want := []time.Duration{1000 * time.Millisecond, 1200 * time.Millisecond, 1440 * time.Millisecond}
	if len(clk.delays) != len(want) {
		t.Fatalf("delays = %v", clk.delays)
	}
	for i, d := range want {
		if diff := clk.delays[i] - d; diff < -time.Millisecond || diff > time.Millisecond {
			t.Errorf("delay %d = %v, want %v", i, clk.delays[i], d)
		}
	}
}

func TestPollLoginBackoffIsCapped(t *testing.T) {
	c, clk, _ := pollServer(t, []int{404}, "")
	ch, _ := c.NewLoginChallenge()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			clk.mu.Lock()
			n := len(clk.delays)
			clk.mu.Unlock()
			if n >= 30 {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	_, _ = c.PollLogin(ctx, ch)
	clk.mu.Lock()
	defer clk.mu.Unlock()
	for i, d := range clk.delays {
		if d > pollMaxDelay {
			t.Fatalf("delay %d = %v exceeds the 10s cap", i, d)
		}
	}
	if clk.delays[29] != pollMaxDelay {
		t.Errorf("late delay = %v, want the cap", clk.delays[29])
	}
}

func TestPollLoginStopsOnForbidden(t *testing.T) {
	c, _, n := pollServer(t, []int{404, 403}, "")
	ch, _ := c.NewLoginChallenge()
	_, err := c.PollLogin(context.Background(), ch)
	var ce *Error
	if !errors.As(err, &ce) || ce.HTTPStatus != 403 || *n != 2 {
		t.Fatalf("err=%v polls=%d", err, *n)
	}
}

func TestPollLoginGivesUpAfterThreeConsecutiveErrors(t *testing.T) {
	c, _, n := pollServer(t, []int{500}, "")
	ch, _ := c.NewLoginChallenge()
	if _, err := c.PollLogin(context.Background(), ch); err == nil || *n != 3 {
		t.Fatalf("err=%v polls=%d, want a failure after 3", err, *n)
	}
	// A pending answer in between resets the count.
	c, _, n = pollServer(t, []int{500, 500, 404, 500, 500, 200}, `{"accessToken":"at"}`)
	ch, _ = c.NewLoginChallenge()
	if tok, err := c.PollLogin(context.Background(), ch); err != nil || tok.AccessToken != "at" || *n != 6 {
		t.Fatalf("tok=%v err=%v polls=%d", tok, err, *n)
	}
}

func TestPollLoginHonoursContext(t *testing.T) {
	c, _, _ := pollServer(t, []int{404}, "")
	c.clock = SystemClock{}
	ch, _ := c.NewLoginChallenge()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.PollLogin(ctx, ch); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestExchangeAPIKey(t *testing.T) {
	var gotAuth, gotBody, gotPath string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if r.Header.Get("Authorization") == "Bearer bad" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":"bad_api_key"}`)
			return
		}
		_, _ = io.WriteString(w, `{"accessToken":"fresh","refreshToken":"fresh"}`)
	}))
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	cfg := DefaultConfig()
	cfg.APIURL = srv.URL
	c, _ := NewClient(cfg, WithTLSConfig(&tls.Config{RootCAs: pool}))

	tok, err := c.ExchangeAPIKey(context.Background(), "key_123")
	if err != nil || tok.AccessToken != "fresh" {
		t.Fatalf("tok=%v err=%v", tok, err)
	}
	if gotPath != "/auth/exchange_user_api_key" || gotAuth != "Bearer key_123" || gotBody != "{}" {
		t.Errorf("request: %s auth=%q body=%q", gotPath, gotAuth, gotBody)
	}
	var ce *Error
	if _, err := c.ExchangeAPIKey(context.Background(), "bad"); !errors.As(err, &ce) || ce.HTTPStatus != 403 {
		t.Fatalf("err = %v", err)
	}
}

func fakeJWT(payload string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc([]byte(payload)) + "." + enc([]byte("sig"))
}

func TestTokenExpiryReadsExpWithoutVerifying(t *testing.T) {
	exp, err := TokenExpiry(fakeJWT(`{"sub":"user_fake","exp":1794489320,"time":"1789305320"}`))
	if err != nil || exp.Unix() != 1794489320 {
		t.Fatalf("exp=%v err=%v", exp, err)
	}
	for name, tok := range map[string]string{
		"not a jwt": "abc",
		"no exp":    fakeJWT(`{"sub":"x"}`),
		"bad json":  fakeJWT(`nope`),
		"bad b64":   "a.!!!.c",
	} {
		if _, err := TokenExpiry(tok); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if strings.Contains(fakeJWT(`{}`), "=") {
		t.Fatal("test helper must produce unpadded base64url")
	}
}
