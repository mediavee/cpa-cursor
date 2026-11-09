// Package relay is an HTTP/2 server that lets Cursor's own clients use one account they hold no credential
// for. A client presents a key of the operator's gateway; the relay forwards every call to Cursor as the client
// sent it, except the credential, which it replaces with the account's.
package relay

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mediavee/cpa-cursor/internal/cursor"
)

// DefaultPublicAPIURL hosts the public API (/v1/*), which accepts the user API key only.
const DefaultPublicAPIURL = "https://api.cursor.com"

// Config is what the operator decides.
type Config struct {
	// UserAPIKey is the Cursor user API key of the account.
	UserAPIKey string
	// SealKey is the relay's own secret: it seals the client key inside the tokens the relay hands out.
	SealKey string
	// VerifierURL is asked with one GET and the client key as bearer; 200 means the key is valid.
	VerifierURL string
	// PublicURL is how clients reach the relay; it replaces Cursor's agent hosts in GetServerConfig.
	PublicURL string

	// APIURL hosts the aiserver.v1 RPCs and the auth endpoints.
	APIURL string
	// AgentURL hosts agent.v1 until GetServerConfig announces another.
	AgentURL string
	// PublicAPIURL hosts /v1/*.
	PublicAPIURL string

	// VerifyTTL and VerifyNegativeTTL bound how long the verifier's answer is kept.
	VerifyTTL, VerifyNegativeTTL time.Duration
	// DrainTimeout bounds how long open streams may finish after a shutdown request.
	DrainTimeout time.Duration

	// TLSCertFile and TLSKeyFile make the relay serve TLS; without them it serves cleartext HTTP/2 (h2c)
	// for a reverse proxy.
	TLSCertFile, TLSKeyFile string
	// ListenAddr is the address the command listens on.
	ListenAddr string

	// UpstreamTLS customises the TLS configuration towards Cursor, for tests.
	UpstreamTLS *tls.Config
	// Now replaces the system clock, for tests.
	Now func() time.Time
}

func (c *Config) setDefaults() {
	if c.APIURL == "" {
		c.APIURL = cursor.DefaultAPIURL
	}
	if c.AgentURL == "" {
		c.AgentURL = cursor.DefaultAgentURL
	}
	if c.PublicAPIURL == "" {
		c.PublicAPIURL = DefaultPublicAPIURL
	}
	if c.VerifyTTL == 0 {
		c.VerifyTTL = time.Minute
	}
	if c.VerifyNegativeTTL == 0 {
		c.VerifyNegativeTTL = 10 * time.Second
	}
	if c.DrainTimeout == 0 {
		c.DrainTimeout = 30 * time.Second
	}
	if c.ListenAddr == "" {
		c.ListenAddr = ":8080"
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

const minSealKeyLen = 32

func (c *Config) validate() error {
	var errs []error
	if c.UserAPIKey == "" {
		errs = append(errs, errors.New("the user API key is empty"))
	}
	if len(c.SealKey) < minSealKeyLen {
		errs = append(errs, fmt.Errorf("the seal key must hold at least %d characters", minSealKeyLen))
	}
	for name, raw := range map[string]string{
		"VerifierURL": c.VerifierURL, "PublicURL": c.PublicURL,
		"APIURL": c.APIURL, "AgentURL": c.AgentURL, "PublicAPIURL": c.PublicAPIURL,
	} {
		if err := checkURL(raw, name == "VerifierURL" || name == "PublicURL"); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		errs = append(errs, errors.New("TLS needs both a certificate and a key file"))
	}
	return errors.Join(errs...)
}

// checkURL requires an absolute URL; upstreams must be https so the account's credential never travels in
// clear, the verifier and the public URL may be http.
func checkURL(raw string, httpOK bool) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return err
	case u.Host == "":
		return fmt.Errorf("%q is not an absolute URL", raw)
	case u.Scheme == "https", u.Scheme == "http" && httpOK:
		return nil
	}
	return fmt.Errorf("%q has an unsupported scheme", raw)
}

// Environment variable names. A secret can also be read from the file named by the variable with a _FILE suffix.
const (
	envListen         = "CURSOR_RELAY_LISTEN"
	envPublicURL      = "CURSOR_RELAY_PUBLIC_URL"
	envVerifierURL    = "CURSOR_RELAY_VERIFIER_URL"
	envUserAPIKey     = "CURSOR_RELAY_USER_API_KEY"
	envSealKey        = "CURSOR_RELAY_SEAL_KEY"
	envVerifyTTL      = "CURSOR_RELAY_VERIFY_TTL"
	envVerifyNegTTL   = "CURSOR_RELAY_VERIFY_NEGATIVE_TTL"
	envDrainTimeout   = "CURSOR_RELAY_DRAIN_TIMEOUT"
	envTLSCert        = "CURSOR_RELAY_TLS_CERT_FILE"
	envTLSKey         = "CURSOR_RELAY_TLS_KEY_FILE"
	envAPIURL         = "CURSOR_RELAY_API_URL"
	envAgentURL       = "CURSOR_RELAY_AGENT_URL"
	envPublicAPIURL   = "CURSOR_RELAY_PUBLIC_API_URL"
	fileSuffix        = "_FILE"
	defaultListenAddr = ":8080"
)

// ConfigFromEnv reads the configuration from getenv (os.Getenv in production) and validates it.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	var errs []error
	secret := func(name string) string {
		if path := getenv(name + fileSuffix); path != "" {
			raw, err := os.ReadFile(path)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s%s: %w", name, fileSuffix, err))
				return ""
			}
			return strings.TrimSpace(string(raw))
		}
		return strings.TrimSpace(getenv(name))
	}
	duration := func(name string) time.Duration {
		raw := getenv(name)
		if raw == "" {
			return 0
		}
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("%s: %q is not a positive duration", name, raw))
		}
		return d
	}
	cfg := Config{
		UserAPIKey:        secret(envUserAPIKey),
		SealKey:           secret(envSealKey),
		VerifierURL:       getenv(envVerifierURL),
		PublicURL:         strings.TrimRight(getenv(envPublicURL), "/"),
		APIURL:            strings.TrimRight(getenv(envAPIURL), "/"),
		AgentURL:          strings.TrimRight(getenv(envAgentURL), "/"),
		PublicAPIURL:      strings.TrimRight(getenv(envPublicAPIURL), "/"),
		VerifyTTL:         duration(envVerifyTTL),
		VerifyNegativeTTL: duration(envVerifyNegTTL),
		DrainTimeout:      duration(envDrainTimeout),
		TLSCertFile:       getenv(envTLSCert),
		TLSKeyFile:        getenv(envTLSKey),
		ListenAddr:        getenv(envListen),
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = defaultListenAddr
	}
	cfg.setDefaults()
	if err := errors.Join(append(errs, cfg.validate())...); err != nil {
		return Config{}, fmt.Errorf("relay config: %w", err)
	}
	return cfg, nil
}
