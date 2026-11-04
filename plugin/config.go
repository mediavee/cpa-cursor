package plugin

import (
	"errors"
	"fmt"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/mediavee/cpa-cursor/internal/cursor"
)

// Defaults of the settings that are not Cursor's own (those are cursor.DefaultConfig).
const (
	defaultModelsRefreshInterval = time.Hour
	defaultRequestTimeout        = 15 * time.Minute
)

// Config is plugins.configs.cursor of CPA's config.yaml. Every key is optional.
type Config struct {
	Upstream cursor.Config
	// ParkTTL and MaxParked bound the runs parked on client tool calls (per credential).
	ParkTTL   time.Duration
	MaxParked int
	// ModelsRefreshInterval is how often CPA is asked to refresh a credential, which re-lists its models.
	ModelsRefreshInterval time.Duration
	// RequestTimeout bounds one request, from the first byte sent to Cursor to the last one received.
	RequestTimeout time.Duration
}

// rawConfig is the YAML shape; durations are strings such as "10m".
type rawConfig struct {
	APIURL                string `yaml:"api_url"`
	AgentURL              string `yaml:"agent_url"`
	SiteURL               string `yaml:"site_url"`
	ClientVersion         string `yaml:"client_version"`
	GhostMode             *bool  `yaml:"ghost_mode"`
	ParkTTL               string `yaml:"park_ttl"`
	MaxParked             int    `yaml:"max_parked"`
	ModelsRefreshInterval string `yaml:"models_refresh_interval"`
	RequestTimeout        string `yaml:"request_timeout"`
}

// parseConfig reads the YAML CPA sends with plugin.register and plugin.reconfigure. Keys the plugin
// does not know (enabled, priority, ...) are ignored.
func parseConfig(raw []byte) (Config, error) {
	var rc rawConfig
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &rc); err != nil {
			return Config{}, fmt.Errorf("plugins.configs.cursor: %w", err)
		}
	}
	cfg := Config{
		Upstream:              cursor.DefaultConfig(),
		ParkTTL:               cursor.DefaultParkTTL,
		MaxParked:             cursor.DefaultMaxParked,
		ModelsRefreshInterval: defaultModelsRefreshInterval,
		RequestTimeout:        defaultRequestTimeout,
	}
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&cfg.Upstream.APIURL, rc.APIURL)
	set(&cfg.Upstream.AgentURL, rc.AgentURL)
	set(&cfg.Upstream.SiteURL, rc.SiteURL)
	set(&cfg.Upstream.ClientVersion, rc.ClientVersion)
	if rc.GhostMode != nil {
		cfg.Upstream.GhostMode = *rc.GhostMode
	}

	var errs []error
	duration := func(key, v string, dst *time.Duration) {
		if v == "" {
			return
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("%s: %q is not a positive duration such as \"10m\"", key, v))
			return
		}
		*dst = d
	}
	duration("park_ttl", rc.ParkTTL, &cfg.ParkTTL)
	duration("models_refresh_interval", rc.ModelsRefreshInterval, &cfg.ModelsRefreshInterval)
	duration("request_timeout", rc.RequestTimeout, &cfg.RequestTimeout)
	if rc.MaxParked < 0 {
		errs = append(errs, errors.New("max_parked: must not be negative"))
	} else if rc.MaxParked > 0 {
		cfg.MaxParked = rc.MaxParked
	}
	if len(errs) > 0 {
		return Config{}, fmt.Errorf("plugins.configs.cursor: %w", errors.Join(errs...))
	}
	return cfg, nil
}
