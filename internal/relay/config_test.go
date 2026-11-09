package relay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestConfigFromEnv(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "user-key")
	if err := os.WriteFile(keyFile, []byte(testUserKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := map[string]string{
		"CURSOR_RELAY_USER_API_KEY_FILE": keyFile,
		"CURSOR_RELAY_SEAL_KEY":          testSealKey,
		"CURSOR_RELAY_VERIFIER_URL":      "https://gateway.example.test/v1/models",
		"CURSOR_RELAY_PUBLIC_URL":        "https://relay.example.test/",
	}
	cfg, err := ConfigFromEnv(envMap(base))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UserAPIKey != testUserKey || cfg.PublicURL != "https://relay.example.test" || cfg.ListenAddr != ":8080" ||
		cfg.APIURL != "https://api2.cursor.sh" || cfg.PublicAPIURL != "https://api.cursor.com" ||
		cfg.VerifyTTL != time.Minute || cfg.VerifyNegativeTTL != 10*time.Second || cfg.DrainTimeout != 30*time.Second {
		t.Errorf("unexpected defaults: %+v", cfg)
	}

	bad := map[string]map[string]string{
		"no user key":      {"CURSOR_RELAY_USER_API_KEY_FILE": ""},
		"missing file":     {"CURSOR_RELAY_USER_API_KEY_FILE": filepath.Join(dir, "absent")},
		"short seal key":   {"CURSOR_RELAY_SEAL_KEY": "short"},
		"no verifier":      {"CURSOR_RELAY_VERIFIER_URL": ""},
		"no public url":    {"CURSOR_RELAY_PUBLIC_URL": ""},
		"cleartext api":    {"CURSOR_RELAY_API_URL": "http://api2.example.test"},
		"bad duration":     {"CURSOR_RELAY_VERIFY_TTL": "soon"},
		"certificate only": {"CURSOR_RELAY_TLS_CERT_FILE": "/etc/relay/cert.pem"},
	}
	for name, override := range bad {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for k, v := range override {
			env[k] = v
		}
		if _, err := ConfigFromEnv(envMap(env)); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), testSealKey) || strings.Contains(err.Error(), testUserKey) {
			t.Errorf("%s: the error carries a secret: %v", name, err)
		}
	}
}
