package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/mediavee/cpa-cursor/internal/cursor"
)

const (
	// loginPollWindow is how long one auth.login.poll call watches the login: the host polls again by
	// itself, so a call returns "pending" soon rather than holding the management request.
	loginPollWindow = 1500 * time.Millisecond
	loginLifetime   = 15 * time.Minute
	exchangeTimeout = 30 * time.Second

	modePKCE   = "pkce"
	modeAPIKey = "api_key"
)

// modelsEvery is the models refresh interval of the current config.
func (p *Plugin) modelsEvery() time.Duration {
	if rt := p.rt.Load(); rt != nil {
		return rt.cfg.ModelsRefreshInterval
	}
	return defaultModelsRefreshInterval
}

// authParse turns a cursor auth file into a credential. It does no I/O: a file that holds only an API
// key gets a refresh due at once, which exchanges the key for a session token.
func (p *Plugin) authParse(raw []byte) (any, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, newError("bad_request", 400, "auth.parse: %v", err)
	}
	var probe struct {
		Type  string `json:"type"`
		Label string `json:"label"`
	}
	// A file that is not JSON, or not of type cursor, is another provider's: not handled, not an error.
	if json.Unmarshal(req.RawJSON, &probe) != nil || !strings.EqualFold(probe.Type, ID) {
		return pluginapi.AuthParseResponse{}, nil //nolint:nilerr
	}
	c, err := parseCredential(req.RawJSON, nil)
	if err != nil {
		return nil, newError("bad_auth_file", 400, "%v", err)
	}
	if c.accessToken() == "" && c.apiKey() == "" {
		return nil, newError("bad_auth_file", 400, "cursor auth file %q has neither access_token nor api_key", req.FileName)
	}
	label := probe.Label
	if label == "" {
		label = strings.TrimSuffix(req.FileName, ".json")
	}
	data, err := c.authData(req.FileName, label, p.now(), p.modelsEvery())
	if err != nil {
		return nil, err
	}
	return pluginapi.AuthParseResponse{Handled: true, Auth: data}, nil
}

// authLoginStart begins a login through CPA's management API. Without parameters it is the CLI's
// browser login: the link is opened on any machine. With an api_key parameter the key is exchanged
// on the first poll.
func (p *Plugin) authLoginStart(raw []byte) (any, error) {
	var req pluginapi.AuthLoginStartRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, newError("bad_request", 400, "auth.login.start: %v", err)
	}
	rt, err := p.runtime()
	if err != nil {
		return nil, err
	}
	if key, _ := req.Metadata["api_key"].(string); strings.TrimSpace(key) != "" {
		return pluginapi.AuthLoginStartResponse{
			Provider:  ID,
			State:     newLoginID(),
			ExpiresAt: p.now().Add(loginLifetime),
			Metadata:  map[string]any{"mode": modeAPIKey, "api_key": strings.TrimSpace(key)},
		}, nil
	}
	ch, err := rt.client.NewLoginChallenge()
	if err != nil {
		return nil, err
	}
	return pluginapi.AuthLoginStartResponse{
		Provider:  ID,
		URL:       ch.URL,
		State:     ch.UUID,
		ExpiresAt: p.now().Add(loginLifetime),
		Metadata:  map[string]any{"mode": modePKCE, "uuid": ch.UUID, "verifier": ch.Verifier},
	}, nil
}

// authLoginPoll checks the login once.
func (p *Plugin) authLoginPoll(raw []byte) (any, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, newError("bad_request", 400, "auth.login.poll: %v", err)
	}
	rt, err := p.runtime()
	if err != nil {
		return nil, err
	}
	failed := func(err error) (any, error) {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: p.exchangeError(context.Background(), rt, err).Message}, nil
	}

	var tokens *cursor.Tokens
	var apiKey string
	switch mode, _ := req.Metadata["mode"].(string); mode {
	case modeAPIKey:
		apiKey, _ = req.Metadata["api_key"].(string)
		ctx, cancel := context.WithTimeout(context.Background(), exchangeTimeout)
		defer cancel()
		if tokens, err = rt.client.ExchangeAPIKey(ctx, apiKey); err != nil {
			return failed(err)
		}
	case modePKCE:
		id, _ := req.Metadata["uuid"].(string)
		verifier, _ := req.Metadata["verifier"].(string)
		ctx, cancel := context.WithTimeout(context.Background(), loginPollWindow)
		defer cancel()
		tokens, err = rt.client.PollLogin(ctx, &cursor.LoginChallenge{UUID: id, Verifier: verifier})
		if errors.Is(err, context.DeadlineExceeded) {
			return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: "waiting for the login link to be opened and approved"}, nil
		}
		if err != nil {
			return failed(err)
		}
	default:
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "unknown login state: start the login again"}, nil
	}

	c := credential{fields: map[string]any{}}
	c.setAccessToken(tokens.AccessToken)
	if apiKey != "" {
		c.fields["api_key"] = apiKey
	}
	name := accountFileName(tokens.AccessToken)
	data, err := c.authData(name, strings.TrimSuffix(name, ".json"), p.now(), rt.cfg.ModelsRefreshInterval)
	if err != nil {
		return nil, err
	}
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Message: "logged in", Auth: data}, nil
}

// authRefresh renews a credential. A session token inside its last five minutes (or missing) is
// exchanged again from the API key; without a key an expired token is an error that says to log in
// again. Called earlier than that, it only reschedules, which lists the credential's models again.
func (p *Plugin) authRefresh(raw []byte) (any, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, newError("bad_request", 400, "auth.refresh: %v", err)
	}
	rt, err := p.runtime()
	if err != nil {
		return nil, err
	}
	c, err := parseCredential(req.StorageJSON, req.Metadata)
	if err != nil {
		return nil, newError("bad_auth_file", 400, "%v", err)
	}
	now := p.now()
	if c.needsExchange(now) {
		switch key := c.apiKey(); {
		case key != "":
			ctx, cancel := context.WithTimeout(context.Background(), exchangeTimeout)
			defer cancel()
			tokens, err := rt.client.ExchangeAPIKey(ctx, key)
			if err != nil {
				return nil, p.exchangeError(ctx, rt, err)
			}
			c.setAccessToken(tokens.AccessToken)
		case !c.usable(now):
			return nil, newError("login_required", 401, "the Cursor session token has expired and the credential has no API key: reconnection required (log in again)")
		}
	}
	data, err := c.authData("", "", now, rt.cfg.ModelsRefreshInterval)
	if err != nil {
		return nil, err
	}
	return pluginapi.AuthRefreshResponse{Auth: data, NextRefreshAfter: data.NextRefreshAfter}, nil
}

func newLoginID() string { return "key-" + uuid.NewString() }

// exchangeError maps a failed login or API-key exchange: Cursor refusing the key means the operator
// must supply a new one, whatever the code Cursor gave.
func (p *Plugin) exchangeError(ctx context.Context, rt *runtime, err error) *pluginabi.Error {
	var ce *cursor.Error
	if errors.As(err, &ce) && (ce.Class == cursor.ClassLogin || ce.Class == cursor.ClassFatal || ce.Class == cursor.ClassBlocked) {
		return newError("login_required", 401, "Cursor rejected the API key or the login, reconnection required (provide a new key or log in again): %v", ce)
	}
	return p.upstreamError(ctx, rt, "", err)
}
