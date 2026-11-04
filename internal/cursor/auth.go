package cursor

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Polling parameters of the CLI's browser login (research/cursor-protocol.md §1.1).
const (
	pollMaxAttempts  = 150
	pollMaxErrors    = 3
	pollBaseDelay    = time.Second
	pollMaxDelay     = 10 * time.Second
	pollDelayGrowth  = 1.2
	pollResponseSize = 1 << 20
)

// Tokens are what the login and the API-key exchange return. For a browser
// login the refresh token is the same string as the access token, and the
// CLI has no way to use it: only the API-key exchange renews a session.
type Tokens struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

// LoginChallenge is one PKCE browser login: open URL in a browser, then call
// Client.PollLogin.
type LoginChallenge struct {
	UUID     string
	Verifier string
	URL      string
}

// NewLoginChallenge builds the loginDeepControl link the way the CLI does.
func NewLoginChallenge(siteURL string) (*LoginChallenge, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("cursor: login verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	id := uuid.NewString()

	q := url.Values{}
	q.Set("challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	q.Set("uuid", id)
	q.Set("mode", "login")
	q.Set("redirectTarget", "cli")
	return &LoginChallenge{
		UUID:     id,
		Verifier: verifier,
		URL:      strings.TrimRight(siteURL, "/") + "/loginDeepControl?" + q.Encode(),
	}, nil
}

// NewLoginChallenge builds a challenge for the configured site.
func (c *Client) NewLoginChallenge() (*LoginChallenge, error) {
	return NewLoginChallenge(c.cfg.SiteURL)
}

// PollLogin waits for the user to finish the browser login. It backs off like
// the CLI (1 s growing by 20% up to 10 s, 150 attempts) and gives up after
// three consecutive failures.
func (c *Client) PollLogin(ctx context.Context, ch *LoginChallenge) (*Tokens, error) {
	endpoint := c.cfg.APIURL + "/auth/poll?" + url.Values{"uuid": {ch.UUID}, "verifier": {ch.Verifier}}.Encode()
	var lastErr error
	failures := 0
	for attempt := range pollMaxAttempts {
		tokens, pending, err := c.pollOnce(ctx, endpoint)
		switch {
		case err == nil && !pending:
			return tokens, nil
		case err == nil:
			failures = 0
		case isHardFailure(err):
			return nil, err
		default:
			failures++
			lastErr = err
			if failures >= pollMaxErrors {
				return nil, fmt.Errorf("cursor: login poll failed %d times in a row: %w", failures, lastErr)
			}
		}
		delay := min(time.Duration(float64(pollBaseDelay)*math.Pow(pollDelayGrowth, float64(attempt))), pollMaxDelay)
		t := c.clock.NewTimer(delay)
		select {
		case <-t.C():
		case <-ctx.Done():
			t.Stop()
			return nil, context.Cause(ctx)
		}
	}
	return nil, errors.New("cursor: login timed out: the link was not opened or approved")
}

func isHardFailure(err error) bool {
	var ce *Error
	return errors.As(err, &ce) && ce.HTTPStatus == http.StatusForbidden
}

// pollOnce returns pending=true while the login is not approved yet.
func (c *Client) pollOnce(ctx context.Context, endpoint string) (tokens *Tokens, pending bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.unary.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, pollResponseSize))
	if err != nil {
		return nil, false, err
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return nil, true, nil
	case http.StatusOK:
		var t Tokens
		if err := json.Unmarshal(body, &t); err != nil || t.AccessToken == "" {
			return nil, false, errors.New("cursor: login poll returned no access token")
		}
		return &t, false, nil
	default:
		return nil, false, parseHTTPError(resp.StatusCode, body)
	}
}

// ExchangeAPIKey trades a user API key for a session token, which is also how
// the CLI renews it.
func (c *Client) ExchangeAPIKey(ctx context.Context, apiKey string) (*Tokens, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.APIURL+"/auth/exchange_user_api_key", bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.unary.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cursor: exchange API key: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, pollResponseSize))
	if err != nil {
		return nil, fmt.Errorf("cursor: exchange API key: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseHTTPError(resp.StatusCode, body)
	}
	var t Tokens
	if err := json.Unmarshal(body, &t); err != nil || t.AccessToken == "" {
		return nil, errors.New("cursor: exchange API key returned no access token")
	}
	return &t, nil
}

// TokenExpiry reads the exp claim of a JWT without verifying its signature.
func TokenExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("cursor: token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, fmt.Errorf("cursor: token payload: %w", err)
	}
	var claims struct {
		Exp *float64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("cursor: token payload: %w", err)
	}
	if claims.Exp == nil {
		return time.Time{}, errors.New("cursor: token has no exp claim")
	}
	return time.Unix(int64(*claims.Exp), 0), nil
}
