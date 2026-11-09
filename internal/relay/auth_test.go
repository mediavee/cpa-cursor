package relay

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestExchangeKeepsTheUpstreamClaimsAndSealsTheClientKey(t *testing.T) {
	e := newEnv(t, envOpts{})
	token := e.token()

	up, got := strings.Split(e.upstreamToken(), "."), strings.Split(token, ".")
	if len(got) != 3 || got[0] != up[0] || got[1] != up[1] {
		t.Fatalf("token %q does not keep the upstream header and claims", token)
	}
	if got[2] == up[2] || strings.Contains(token, testClientKey) || strings.Contains(token, base64.RawURLEncoding.EncodeToString([]byte(testClientKey))) {
		t.Error("the signature must be the sealed key, neither Cursor's signature nor the key in clear")
	}
	var claims struct {
		Sub string `json:"sub"`
		Exp int64  `json:"exp"`
	}
	raw, _ := base64.RawURLEncoding.DecodeString(got[1])
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Sub != "user_test" || claims.Exp != e.clock.Now().Add(time.Hour).Unix() {
		t.Errorf("claims %s: %v", raw, err)
	}
	// Minted twice, the tokens differ (fresh nonce) and both work.
	if again := e.token(); again == token {
		t.Error("two exchanges returned the same sealed signature")
	}
}

func TestExchangeRefusesAKeyTheGatewayRefuses(t *testing.T) {
	e := newEnv(t, envOpts{})
	before := e.api.calls.Load()
	if _, status := e.exchange("gw_unknown_key"); status != http.StatusUnauthorized {
		t.Errorf("unknown key: status %d, want 401", status)
	}
	if _, status := e.exchange(""); status != http.StatusUnauthorized {
		t.Errorf("no key: status %d, want 401", status)
	}
	if e.api.calls.Load() != before {
		t.Error("a refused exchange reached Cursor")
	}
	if resp, _ := e.do(http.MethodGet, exchangePath, testClientKey, "", ""); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET exchange: status %d, want 405", resp.StatusCode)
	}
}

func TestCallsNeedARelayTokenTheGatewayStillAccepts(t *testing.T) {
	e := newEnv(t, envOpts{api: func(http.ResponseWriter, *http.Request) {}, agent: func(http.ResponseWriter, *http.Request) {}})
	token := e.token()
	other := newEnv(t, envOpts{mutate: func(c *Config) { c.SealKey = "another-seal-key-0123456789abcdefghij" }})
	other.clock.Advance(time.Minute) // other claims than e's, so a signature moved between them shows
	alien := other.token()
	up := e.api.calls.Load()

	for name, bearer := range map[string]string{
		"no credential":       "",
		"the client key":      testClientKey,
		"a token of another":  alien,
		"a token tampered":    token[:len(token)-4] + "AAAA",
		"the upstream token":  e.upstreamToken(),
		"claims of another":   strings.Split(alien, ".")[0] + "." + strings.Split(alien, ".")[1] + "." + strings.Split(token, ".")[2],
		"a malformed bearer":  "not.a.token",
		"a single-part token": "abc",
	} {
		for _, path := range []string{"/aiserver.v1.AiService/Something", "/agent.v1.AgentService/Run"} {
			if resp, _ := e.do(http.MethodPost, path, bearer, "application/proto", "x"); resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s on %s: status %d, want 401", name, path, resp.StatusCode)
			}
		}
	}
	if e.api.calls.Load() != up || e.agent.calls.Load() != 0 {
		t.Error("a refused call reached Cursor")
	}

	// The key is revoked: the token it sealed stops working once the cached answer expires, not before.
	e.setKeyValid(testClientKey, false)
	if resp, _ := e.do(http.MethodPost, "/aiserver.v1.AiService/Something", token, "application/proto", "x"); resp.StatusCode != http.StatusOK {
		t.Errorf("within the cache time: status %d, want 200", resp.StatusCode)
	}
	e.clock.Advance(2 * time.Minute)
	up = e.api.calls.Load()
	if resp, _ := e.do(http.MethodPost, "/aiserver.v1.AiService/Something", token, "application/proto", "x"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("after the revocation: status %d, want 401", resp.StatusCode)
	}
	if e.api.calls.Load() != up {
		t.Error("a revoked key reached Cursor")
	}
}

func TestVerifierAnswersAreCachedAndAnOutageFailsClosed(t *testing.T) {
	e := newEnv(t, envOpts{api: func(http.ResponseWriter, *http.Request) {}})
	token := e.token()
	for range 3 {
		e.do(http.MethodPost, "/aiserver.v1.AiService/Something", token, "application/proto", "x")
	}
	if n := e.verifierCalls.Load(); n != 1 {
		t.Errorf("verifier asked %d times, want 1 (exchange and calls share the cache)", n)
	}

	// Negative answers are cached too, for the shorter negative time.
	for range 3 {
		e.exchange("gw_unknown_key")
	}
	if n := e.verifierCalls.Load(); n != 2 {
		t.Errorf("verifier asked %d times after three bad exchanges, want 2", n)
	}
	e.clock.Advance(11 * time.Second)
	e.exchange("gw_unknown_key")
	if n := e.verifierCalls.Load(); n != 3 {
		t.Errorf("verifier asked %d times once the negative answer expired, want 3", n)
	}

	// A 5xx is not an answer: 503, nothing cached, nothing forwarded.
	e.clock.Advance(2 * time.Minute)
	e.verifierStatus.Store(http.StatusInternalServerError)
	before := e.api.calls.Load()
	if resp, _ := e.do(http.MethodPost, "/aiserver.v1.AiService/Something", token, "application/proto", "x"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("verifier down: status %d, want 503", resp.StatusCode)
	}
	if e.api.calls.Load() != before {
		t.Error("a call went upstream while the verifier was down")
	}
	e.verifierStatus.Store(0)
	if resp, _ := e.do(http.MethodPost, "/aiserver.v1.AiService/Something", token, "application/proto", "x"); resp.StatusCode != http.StatusOK {
		t.Errorf("verifier back: status %d, want 200 (the outage was not cached)", resp.StatusCode)
	}
}

func TestNoLogLineCarriesACredential(t *testing.T) {
	e := newEnv(t, envOpts{
		api:    func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
		public: func(http.ResponseWriter, *http.Request) {},
	})
	token := e.token()
	e.clock.Advance(time.Minute)
	e.do(http.MethodPost, "/aiserver.v1.AiService/Something", token, "application/proto", "x")
	e.do(http.MethodGet, "/v1/me", testClientKey, "", "")
	e.do(http.MethodGet, "/v1/me", "gw_unknown_key", "", "")
	e.do(http.MethodPost, "/aiserver.v1.AiService/Something", "not.a.token", "application/proto", "x")
	e.verifierStatus.Store(http.StatusBadGateway)
	e.do(http.MethodGet, "/v1/me", "gw_another_key", "", "")
	e.verifierStatus.Store(0)
	e.failExchange.Store(true)
	e.clock.Advance(2 * time.Hour)
	e.do(http.MethodPost, "/aiserver.v1.AiService/Something", token, "application/proto", "x")

	logs := e.logs.String()
	if !strings.Contains(logs, "path=/aiserver.v1.AiService/Something") || !strings.Contains(logs, "client="+keyTag(testClientKey)) {
		t.Fatalf("the log lacks the path or the key's hash:\n%s", logs)
	}
	for name, secret := range map[string]string{
		"client key": testClientKey, "unknown key": "gw_unknown_key", "user API key": testUserKey, "seal key": testSealKey,
		"relay token": token, "relay signature": strings.Split(token, ".")[2],
		"upstream token": e.upstreamToken(), "upstream signature": strings.Split(e.upstreamToken(), ".")[2],
	} {
		if strings.Contains(logs, secret) {
			t.Errorf("the log contains the %s:\n%s", name, logs)
		}
	}
}
