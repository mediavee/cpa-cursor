package plugin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func authFile(fields map[string]any) []byte {
	fields["type"] = "cursor"
	return mustJSON(fields)
}

func (h *harness) parse(name string, raw []byte) (pluginapi.AuthParseResponse, *pluginabi.Error) {
	h.t.Helper()
	res, e := h.call(pluginabi.MethodAuthParse, mustJSON(pluginapi.AuthParseRequest{Provider: "cursor", FileName: name, RawJSON: raw}))
	var out pluginapi.AuthParseResponse
	if e == nil {
		if err := json.Unmarshal(res, &out); err != nil {
			h.t.Fatal(err)
		}
	}
	return out, e
}

func TestParseDeclaresTheRefreshCPAReads(t *testing.T) {
	h := newHarness(t)
	token := h.token(60*24*time.Hour, "sub-1")
	out, e := h.parse("work.json", authFile(map[string]any{"access_token": token, "api_key": "key-secret", "prefix": "cur"}))
	if e != nil || !out.Handled {
		t.Fatalf("parse = %+v, %v", out, e)
	}
	a := out.Auth
	if a.Provider != "cursor" || a.FileName != "work.json" || a.Label != "work" {
		t.Errorf("auth = %+v", a)
	}
	// CPA never refreshes an unknown provider without these (research/cpa-plugin-abi.md §2.4).
	if a.Metadata["refresh_interval_seconds"] != float64(300) {
		t.Errorf("refresh_interval_seconds = %v, want 300", a.Metadata["refresh_interval_seconds"])
	}
	if a.Metadata["refresh_token"] != "key-secret" {
		t.Errorf("refresh_token must carry the API key so that a 401 triggers a refresh")
	}
	if a.Metadata["access_token"] != token || a.Metadata["prefix"] != "cur" {
		t.Errorf("metadata lost the file content: %v", a.Metadata)
	}
	// The models interval (1 h) comes before the token's expiry − 300 s: the credential is refreshed
	// then, which lists its models again.
	if want := h.clock().Add(time.Hour); !a.NextRefreshAfter.Equal(want) {
		t.Errorf("next refresh = %v, want %v", a.NextRefreshAfter, want)
	}
	var storage map[string]any
	if err := json.Unmarshal(a.StorageJSON, &storage); err != nil || storage["access_token"] != token {
		t.Errorf("storage json = %s", a.StorageJSON)
	}
}

func TestParseSchedulesRefreshFiveMinutesBeforeExpiry(t *testing.T) {
	h := newHarness(t)
	exp := h.clock().Add(20 * time.Minute)
	out, e := h.parse("a.json", authFile(map[string]any{"access_token": makeJWT(exp, "s"), "api_key": "k"}))
	if e != nil {
		t.Fatal(e)
	}
	if want := exp.Add(-300 * time.Second); !out.Auth.NextRefreshAfter.Equal(want) {
		t.Errorf("next refresh = %v, want exp − 300 s = %v", out.Auth.NextRefreshAfter, want)
	}
}

func TestParseAPIKeyOnlyFileIsDueForRefreshAtOnce(t *testing.T) {
	h := newHarness(t)
	out, e := h.parse("k.json", authFile(map[string]any{"api_key": "k"}))
	if e != nil || !out.Handled {
		t.Fatalf("parse = %+v, %v", out, e)
	}
	if out.Auth.NextRefreshAfter.After(h.clock()) {
		t.Errorf("next refresh = %v: a file with only a key must be exchanged now", out.Auth.NextRefreshAfter)
	}
	if h.up.count("/auth/exchange_user_api_key") != 0 {
		t.Error("parse must not touch the network")
	}
}

func TestParseLeavesOtherProvidersAlone(t *testing.T) {
	h := newHarness(t)
	out, e := h.parse("x.json", []byte(`{"type":"claude","access_token":"t"}`))
	if e != nil || out.Handled {
		t.Errorf("parse of a claude file = %+v, %v: must not be handled", out, e)
	}
}

func TestParseRejectsAFileWithoutCredentials(t *testing.T) {
	h := newHarness(t)
	if _, e := h.parse("x.json", authFile(map[string]any{"label": "empty"})); e == nil || !strings.Contains(e.Message, "neither access_token nor api_key") {
		t.Errorf("error = %+v", e)
	}
}

func TestLoginWithABrowserLink(t *testing.T) {
	h := newHarness(t)
	var start pluginapi.AuthLoginStartResponse
	h.mustCall(pluginabi.MethodAuthLoginStart, pluginapi.AuthLoginStartRequest{Provider: "cursor", BaseURL: "http://127.0.0.1:8317/v0/management/oauth-callback"}, &start)

	u, err := url.Parse(start.URL)
	if err != nil || !strings.HasSuffix(u.Path, "/loginDeepControl") {
		t.Fatalf("login url = %q", start.URL)
	}
	q := u.Query()
	if q.Get("uuid") != start.State || q.Get("challenge") == "" || q.Get("mode") != "login" {
		t.Errorf("login url query = %v, state = %q", q, start.State)
	}
	if start.Metadata["verifier"] == "" || strings.Contains(start.URL, start.Metadata["verifier"].(string)) {
		t.Error("the verifier must be kept for the poll and never put in the link")
	}
	if !start.ExpiresAt.After(h.clock()) {
		t.Errorf("expires at %v", start.ExpiresAt)
	}

	var polls atomic.Int32
	issued := h.token(60*24*time.Hour, "sub-login")
	h.up.handle("/auth/poll", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("uuid") != start.State || r.URL.Query().Get("verifier") != start.Metadata["verifier"] {
			t.Errorf("poll query = %v", r.URL.Query())
		}
		if polls.Add(1) < 2 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"accessToken":"` + issued + `","refreshToken":"` + issued + `"}`))
	})

	poll := func() pluginapi.AuthLoginPollResponse {
		var out pluginapi.AuthLoginPollResponse
		h.mustCall(pluginabi.MethodAuthLoginPoll, pluginapi.AuthLoginPollRequest{Provider: "cursor", State: start.State, Metadata: start.Metadata}, &out)
		return out
	}
	// The first calls see 404 (not approved yet): pending, not an error.
	first := poll()
	for first.Status == pluginapi.AuthLoginStatusPending && polls.Load() < 2 {
		first = poll()
	}
	done := first
	for done.Status == pluginapi.AuthLoginStatusPending {
		done = poll()
	}
	if done.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("login = %+v", done)
	}
	a := done.Auth
	if a.Metadata["access_token"] != issued || a.Metadata["api_key"] != nil || a.Metadata["refresh_token"] != nil {
		t.Errorf("a browser login has a session token only: %v", a.Metadata)
	}
	if !strings.HasPrefix(a.FileName, "cursor-") || !strings.HasSuffix(a.FileName, ".json") || strings.Contains(a.FileName, "sub-login") {
		t.Errorf("file name = %q: stable per account, no account data in it", a.FileName)
	}
	again, _ := h.parse(a.FileName, a.StorageJSON)
	if !again.Handled {
		t.Error("the file a login saves must parse back")
	}
}

func TestLoginWithAnAPIKey(t *testing.T) {
	h := newHarness(t)
	var start pluginapi.AuthLoginStartResponse
	h.mustCall(pluginabi.MethodAuthLoginStart, pluginapi.AuthLoginStartRequest{Provider: "cursor", Metadata: map[string]any{"api_key": " key-1 "}}, &start)
	if start.State == "" || strings.ContainsAny(start.State, " /") {
		t.Errorf("state = %q", start.State)
	}

	issued := h.token(60*24*time.Hour, "sub-key")
	h.up.handle("/auth/exchange_user_api_key", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key-1" {
			t.Errorf("exchange used %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"accessToken":"` + issued + `","refreshToken":"` + issued + `"}`))
	})
	var out pluginapi.AuthLoginPollResponse
	h.mustCall(pluginabi.MethodAuthLoginPoll, pluginapi.AuthLoginPollRequest{Provider: "cursor", State: start.State, Metadata: start.Metadata}, &out)
	if out.Status != pluginapi.AuthLoginStatusSuccess || out.Auth.Metadata["api_key"] != "key-1" || out.Auth.Metadata["refresh_token"] != "key-1" || out.Auth.Metadata["access_token"] != issued {
		t.Errorf("login = %+v", out)
	}
}

func TestLoginErrorsAreReportedToTheOperator(t *testing.T) {
	h := newHarness(t)
	h.up.replyStatus("/auth/exchange_user_api_key", http.StatusForbidden, `{"error":"bad key"}`)
	var out pluginapi.AuthLoginPollResponse
	h.mustCall(pluginabi.MethodAuthLoginPoll, pluginapi.AuthLoginPollRequest{Provider: "cursor", State: "key-1", Metadata: map[string]any{"mode": "api_key", "api_key": "wrong"}}, &out)
	if out.Status != pluginapi.AuthLoginStatusError || out.Message == "" {
		t.Errorf("login = %+v, want an error with a message", out)
	}

	h.mustCall(pluginabi.MethodAuthLoginPoll, pluginapi.AuthLoginPollRequest{Provider: "cursor", State: "lost", Metadata: map[string]any{}}, &out)
	if out.Status != pluginapi.AuthLoginStatusError {
		t.Errorf("a poll without login state = %+v", out)
	}
}

func (h *harness) refresh(storage map[string]any) (pluginapi.AuthRefreshResponse, *pluginabi.Error) {
	h.t.Helper()
	res, e := h.call(pluginabi.MethodAuthRefresh, mustJSON(pluginapi.AuthRefreshRequest{AuthID: "auth-1", AuthProvider: "cursor", StorageJSON: authFile(storage), Metadata: storage}))
	var out pluginapi.AuthRefreshResponse
	if e == nil {
		if err := json.Unmarshal(res, &out); err != nil {
			h.t.Fatal(err)
		}
	}
	return out, e
}

func TestRefreshExchangesTheAPIKeyInTheLastFiveMinutes(t *testing.T) {
	h := newHarness(t)
	fresh := h.token(60*24*time.Hour, "sub-1")
	var exchanges atomic.Int32
	h.up.handle("/auth/exchange_user_api_key", func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		if r.Header.Get("Authorization") != "Bearer key-1" {
			t.Errorf("exchange used %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"accessToken":"` + fresh + `"}`))
	})

	// 10 minutes left: nothing to exchange yet, only the schedule moves.
	soon := h.token(10*time.Minute, "sub-1")
	out, e := h.refresh(map[string]any{"access_token": soon, "api_key": "key-1"})
	if e != nil || exchanges.Load() != 0 || out.Auth.Metadata["access_token"] != soon {
		t.Fatalf("refresh with 10 min left: exchanges=%d err=%v", exchanges.Load(), e)
	}
	if want := h.clock().Add(5 * time.Minute); !out.NextRefreshAfter.Equal(want) {
		t.Errorf("next refresh = %v, want exp − 300 s = %v", out.NextRefreshAfter, want)
	}

	// 4 minutes left: the key is exchanged.
	out, e = h.refresh(map[string]any{"access_token": h.token(4*time.Minute, "sub-1"), "api_key": "key-1"})
	if e != nil || exchanges.Load() != 1 {
		t.Fatalf("refresh with 4 min left: exchanges=%d err=%v", exchanges.Load(), e)
	}
	if out.Auth.Metadata["access_token"] != fresh || out.Auth.Metadata["api_key"] != "key-1" {
		t.Errorf("metadata = %v", out.Auth.Metadata)
	}
	if out.Auth.Metadata["expires_at"] == nil || !out.NextRefreshAfter.After(h.clock()) {
		t.Errorf("the new expiry and schedule are missing: %v, next %v", out.Auth.Metadata, out.NextRefreshAfter)
	}

	// A key with no session token yet (file dropped by hand) is exchanged too.
	if _, e = h.refresh(map[string]any{"api_key": "key-1"}); e != nil || exchanges.Load() != 2 {
		t.Errorf("refresh without a token: exchanges=%d err=%v", exchanges.Load(), e)
	}
}

func TestRefreshWithoutAKeyAsksToReconnectOnExpiry(t *testing.T) {
	h := newHarness(t)
	_, e := h.refresh(map[string]any{"access_token": h.token(-time.Minute, "sub-1")})
	if e == nil || e.HTTPStatus != 401 || !strings.Contains(e.Message, "reconnection required") {
		t.Errorf("error = %+v, want a clear reconnection-required 401", e)
	}
	// Still valid: nothing to say yet.
	if _, e := h.refresh(map[string]any{"access_token": h.token(3*time.Minute, "sub-1")}); e != nil {
		t.Errorf("a valid token without a key must not fail: %v", e)
	}
}

func TestRefreshWithARejectedKeyIsALoginError(t *testing.T) {
	h := newHarness(t)
	h.up.replyStatus("/auth/exchange_user_api_key", http.StatusForbidden, `{"code":"permission_denied","message":"revoked"}`)
	_, e := h.refresh(map[string]any{"access_token": h.token(time.Minute, "sub-1"), "api_key": "revoked"})
	if e == nil || e.HTTPStatus != 401 || !strings.Contains(e.Message, "reconnection required") {
		t.Fatalf("error = %+v, want a 401 asking to reconnect", e)
	}
	if strings.Contains(e.Message, "Bearer") || strings.Contains(e.Message, "revoked\"") {
		t.Errorf("the message must not carry the key: %q", e.Message)
	}
}

func TestSecretsNeverReachTheLog(t *testing.T) {
	h := newHarness(t)
	token := h.token(time.Hour, "sub-log")
	h.up.replyStatus("/agent.v1.AgentService/GetUsableModels", http.StatusInternalServerError, `{"code":"internal","message":"boom"}`)
	h.up.replyStatus("/auth/exchange_user_api_key", http.StatusForbidden, `{"code":"permission_denied","message":"nope"}`)
	_, _ = h.refresh(map[string]any{"access_token": h.token(time.Minute, "x"), "api_key": "key-very-secret"})
	_, _ = h.execute("auth-1", token, "composer-x", chatBody("composer-x", userMsg("hi")))
	_, _ = h.call(pluginabi.MethodModelForAuth, mustJSON(pluginapi.AuthModelRequest{AuthID: "auth-1", StorageJSON: authFile(map[string]any{"access_token": token})}))

	h.host.mu.Lock()
	defer h.host.mu.Unlock()
	for _, l := range h.host.logs {
		if strings.Contains(l, token) || strings.Contains(l, "key-very-secret") {
			t.Errorf("log line leaks a secret: %q", l)
		}
	}
}

func TestParseKeepsThePrefixOfTheFile(t *testing.T) {
	h := newHarness(t)
	raw := authFile(map[string]any{"type": "cursor", "access_token": h.token(time.Hour, "s"), "prefix": "cur"})
	var out pluginapi.AuthParseResponse
	h.mustCall(pluginabi.MethodAuthParse, pluginapi.AuthParseRequest{FileName: "a.json", RawJSON: raw}, &out)
	if !out.Handled || out.Auth.Prefix != "cur" {
		t.Errorf("prefix = %q: CPA namespaces the models of a credential by it", out.Auth.Prefix)
	}
}
