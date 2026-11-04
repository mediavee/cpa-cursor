package plugin

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/mediavee/cpa-cursor/internal/cursor"
)

// refreshLead is how long before its expiry a session token is exchanged again (the CLI's own rule).
const refreshLead = 300 * time.Second

// credential is the content of a cursor auth file:
//
//	{"type": "cursor", "access_token": "<session JWT>", "api_key": "<optional user API key>", ...}
//
// Keys other than the ones the plugin manages are kept as they are.
type credential struct {
	fields map[string]any
}

// parseCredential reads the auth JSON CPA hands over: the storage JSON first, the metadata over it,
// the way CPA merges them when it saves the file.
func parseCredential(storage []byte, meta map[string]any) (credential, error) {
	fields := map[string]any{}
	if len(storage) > 0 {
		if err := json.Unmarshal(storage, &fields); err != nil {
			return credential{}, fmt.Errorf("cursor auth file is not a JSON object: %w", err)
		}
	}
	maps.Copy(fields, meta)
	return credential{fields: fields}, nil
}

func (c credential) str(key string) string {
	s, _ := c.fields[key].(string)
	return strings.TrimSpace(s)
}

func (c credential) accessToken() string { return c.str("access_token") }
func (c credential) apiKey() string      { return c.str("api_key") }

// expiry is the session token's exp claim, or the expires_at field when the token is not a JWT.
func (c credential) expiry() (time.Time, bool) {
	if tok := c.accessToken(); tok != "" {
		if exp, err := cursor.TokenExpiry(tok); err == nil {
			return exp, true
		}
	}
	if s := c.str("expires_at"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// usable reports whether the session token can still be sent to Cursor.
func (c credential) usable(now time.Time) bool {
	if c.accessToken() == "" {
		return false
	}
	exp, ok := c.expiry()
	return !ok || now.Before(exp)
}

// needsExchange reports whether the session token is missing or inside the refresh lead.
func (c credential) needsExchange(now time.Time) bool {
	if c.accessToken() == "" {
		return true
	}
	exp, ok := c.expiry()
	return ok && exp.Sub(now) <= refreshLead
}

func (c *credential) setAccessToken(token string) {
	c.fields["access_token"] = token
	delete(c.fields, "expires_at")
	if exp, ok := c.expiry(); ok {
		c.fields["expires_at"] = exp.UTC().Format(time.RFC3339)
	}
}

// nextRefresh is the earliest time CPA should call auth.refresh: the exchange lead before expiry or
// the models interval, whichever comes first, so that the credential's models are listed again.
func (c credential) nextRefresh(now time.Time, modelsEvery time.Duration) time.Time {
	if c.accessToken() == "" {
		return now
	}
	next := now.Add(modelsEvery)
	if exp, ok := c.expiry(); ok {
		if due := exp.Add(-refreshLead); due.Before(next) {
			next = due
		}
	}
	return next
}

// authData is the record CPA persists and later hands back to the plugin. Metadata is the file
// content; refresh_interval_seconds and refresh_token are what make CPA schedule a refresh and
// retry after a 401, which it does for no other unknown provider.
func (c credential) authData(fileName, label string, now time.Time, modelsEvery time.Duration) (pluginapi.AuthData, error) {
	meta := maps.Clone(c.fields)
	meta["type"] = ID
	meta["refresh_interval_seconds"] = int(refreshLead / time.Second)
	if key := c.apiKey(); key != "" {
		meta["refresh_token"] = key
	} else {
		delete(meta, "refresh_token")
	}
	storage, err := json.Marshal(meta)
	if err != nil {
		return pluginapi.AuthData{}, err
	}
	return pluginapi.AuthData{
		Provider:         ID,
		FileName:         fileName,
		Label:            label,
		Prefix:           c.str("prefix"),
		StorageJSON:      storage,
		Metadata:         meta,
		NextRefreshAfter: c.nextRefresh(now, modelsEvery),
	}, nil
}

// accountFileName names the file of a logged-in account after its token subject, so that logging in
// again replaces the file instead of adding a second credential.
func accountFileName(token string) string {
	sub := tokenSubject(token)
	if sub == "" {
		sum := sha256.Sum256([]byte(token))
		sub = hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256([]byte(sub))
	return "cursor-" + hex.EncodeToString(sum[:6]) + ".json"
}

// tokenSubject reads the sub claim of a JWT without verifying it.
func tokenSubject(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Sub
}
