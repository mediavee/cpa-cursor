// Package cursor speaks the backend protocol of the official cursor-agent CLI:
// the agent.v1 Run bidi stream over HTTP/2 and the Connect JSON unary RPCs.
//
// The package knows nothing about CPA, the keychain or the file system: the
// caller provides the configuration and a bearer token on every call.
package cursor

import (
	"errors"
	"fmt"
	"net/url"
)

// Defaults match the official CLI. DefaultClientVersion is the latest release
// known when this code was written; callers should override it (see D3).
const (
	DefaultAPIURL        = "https://api2.cursor.sh"
	DefaultAgentURL      = "https://agentn.global.api5.cursor.sh"
	DefaultSiteURL       = "https://cursor.com"
	DefaultClientVersion = "cli-2026.10.22-27dcf5f"
)

// Config is what the caller decides about the upstream.
type Config struct {
	// APIURL hosts the unary RPCs and the auth endpoints.
	APIURL string
	// AgentURL hosts agent.v1.AgentService/Run until GetServerConfig overrides it.
	AgentURL string
	// SiteURL hosts the browser login page.
	SiteURL string
	// ClientVersion is sent as x-cursor-client-version, including the "cli-" prefix.
	ClientVersion string
	// GhostMode is sent as x-ghost-mode. The CLI defaults to true.
	GhostMode bool
}

// DefaultConfig returns the CLI's own settings.
func DefaultConfig() Config {
	return Config{
		APIURL:        DefaultAPIURL,
		AgentURL:      DefaultAgentURL,
		SiteURL:       DefaultSiteURL,
		ClientVersion: DefaultClientVersion,
		GhostMode:     true,
	}
}

func (c Config) validate() error {
	var errs []error
	for name, raw := range map[string]string{"APIURL": c.APIURL, "AgentURL": c.AgentURL, "SiteURL": c.SiteURL} {
		u, err := url.Parse(raw)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		case u.Scheme != "https" || u.Host == "":
			errs = append(errs, fmt.Errorf("%s: %q is not an https URL", name, raw))
		}
	}
	if c.ClientVersion == "" {
		errs = append(errs, errors.New("ClientVersion is empty"))
	}
	return errors.Join(errs...)
}
