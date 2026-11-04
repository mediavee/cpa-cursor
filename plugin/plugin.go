// Package plugin is the CPA v8 plugin that serves Cursor as a provider: it answers the JSON RPC
// methods of the plugin ABI (registration, auth, models, executor, quota) on top of internal/cursor
// and internal/translate. It knows nothing of cgo: cmd/plugin moves the bytes.
package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/mediavee/cpa-cursor/internal/cursor"
)

// ID is the plugin id (the basename of the .so) and the provider key of the credentials.
const ID = "cursor"

// Version is stamped at build time with -ldflags "-X github.com/mediavee/cpa-cursor/plugin.Version=...".
var Version = "dev"

const (
	repository     = "https://github.com/mediavee/cpa-cursor"
	shutdownWait   = 5 * time.Second
	hostLogTimeout = 5 * time.Second
)

// HostCall calls a method of CPA (plugin to host). It is safe from any goroutine.
type HostCall func(method string, payload any) (json.RawMessage, error)

// Plugin answers the ABI methods. Create it with New; it does nothing, not even start a goroutine,
// until CPA calls it, because the library is also loaded by CPA's preflight child.
type Plugin struct {
	host HostCall
	// newClient builds the upstream client; tests replace it to trust a fake server.
	newClient func(cursor.Config) (*cursor.Client, error)
	now       func() time.Time
	// probeEvery is how often an idle stream checks that CPA still reads it.
	probeEvery time.Duration

	// rt is an immutable snapshot swapped whole by plugin.reconfigure.
	rt       atomic.Pointer[runtime]
	catalogs catalogStore

	wg      sync.WaitGroup
	closing atomic.Bool
}

// New returns a plugin that calls CPA through host.
func New(host HostCall) *Plugin {
	return &Plugin{
		host:       host,
		newClient:  func(c cursor.Config) (*cursor.Client, error) { return cursor.NewClient(c) },
		now:        time.Now,
		probeEvery: defaultProbeEvery,
	}
}

// runtime is what one configuration yields: the upstream client and the runs parked per credential.
type runtime struct {
	cfg    Config
	client *cursor.Client

	mu   sync.Mutex
	regs map[string]*cursor.Registry
}

func (rt *runtime) registry(authID string) *cursor.Registry {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	r, ok := rt.regs[authID]
	if !ok {
		r = cursor.NewRegistry(cursor.RegistryConfig{TTL: rt.cfg.ParkTTL, MaxParked: rt.cfg.MaxParked})
		rt.regs[authID] = r
	}
	return r
}

// dropParked closes the runs, whichever credential holds them, that wait for these tool calls: a
// request that replays a conversation supersedes them.
func (rt *runtime) dropParked(toolCallIDs []string) {
	rt.mu.Lock()
	all := make([]*cursor.Registry, 0, len(rt.regs))
	for _, r := range rt.regs {
		all = append(all, r)
	}
	rt.mu.Unlock()
	for _, r := range all {
		r.Drop(toolCallIDs)
	}
}

func (rt *runtime) close() {
	rt.mu.Lock()
	regs := rt.regs
	rt.regs = map[string]*cursor.Registry{}
	rt.mu.Unlock()
	for _, r := range regs {
		r.Close()
	}
	rt.client.CloseIdleConnections()
}

func (p *Plugin) runtime() (*runtime, error) {
	rt := p.rt.Load()
	if rt == nil {
		return nil, newError("not_configured", 503, "the cursor plugin is not registered yet")
	}
	return rt, nil
}

// Call answers one ABI method with the JSON envelope CPA expects. It never panics.
func (p *Plugin) Call(method string, request []byte) (out []byte) {
	defer func() {
		if v := recover(); v != nil {
			p.logf("error", "panic in %s: %v\n%s", method, v, debug.Stack())
			out = ErrorEnvelope(pluginabi.NewError("plugin_panic", fmt.Sprintf("cursor plugin panic in %s: %v", method, v), 500))
		}
	}()
	result, err := p.handle(method, request)
	if err != nil {
		return ErrorEnvelope(asABIError(err))
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return ErrorEnvelope(pluginabi.NewError("plugin_error", err.Error(), 500))
	}
	if result == nil {
		raw = []byte("{}")
	}
	env, err := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	if err != nil {
		return ErrorEnvelope(pluginabi.NewError("plugin_error", err.Error(), 500))
	}
	return env
}

// ErrorEnvelope serialises a failed RPC envelope.
func ErrorEnvelope(e *pluginabi.Error) []byte {
	raw, err := json.Marshal(pluginabi.Envelope{OK: false, Error: e})
	if err != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"unserialisable error"}}`)
	}
	return raw
}

func (p *Plugin) handle(method string, req []byte) (any, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return p.configure(req)
	case pluginabi.MethodPluginQuiesce:
		return struct{}{}, nil
	case pluginabi.MethodPluginShutdown:
		p.Shutdown()
		return struct{}{}, nil

	case pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier, pluginabi.MethodQuotaIdentifier:
		return map[string]string{"identifier": ID}, nil

	case pluginabi.MethodAuthParse:
		return p.authParse(req)
	case pluginabi.MethodAuthLoginStart:
		return p.authLoginStart(req)
	case pluginabi.MethodAuthLoginPoll:
		return p.authLoginPoll(req)
	case pluginabi.MethodAuthRefresh:
		return p.authRefresh(req)

	case pluginabi.MethodModelStatic:
		// Models are per credential: a static list would be arbitrated against native ids.
		return pluginapi.ModelResponse{Provider: ID}, nil
	case pluginabi.MethodModelForAuth:
		return p.modelsForAuth(req)

	case pluginabi.MethodExecutorExecute:
		return p.execute(req)
	case pluginabi.MethodExecutorExecuteStream:
		return p.executeStream(req)
	case pluginabi.MethodExecutorCountTokens:
		return p.countTokens(req)
	case pluginabi.MethodExecutorHTTPRequest:
		return nil, newError("unsupported", 501, "the cursor executor has no passthrough HTTP request")

	case pluginabi.MethodQuotaDescribe:
		return pluginapi.QuotaDescribeResponse{SupportedProviders: []string{ID}, DisplayName: "Cursor"}, nil
	case pluginabi.MethodQuotaFetch:
		return p.quotaFetch(req)
	case pluginabi.MethodQuotaReset:
		return pluginapi.QuotaResetResponse{Message: "Cursor usage cannot be reset from here"}, nil
	}
	return nil, newError("unknown_method", 0, "the cursor plugin does not implement %s", method)
}

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

type capabilities struct {
	AuthProvider          bool                         `json:"auth_provider"`
	ModelProvider         bool                         `json:"model_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats"`
	QuotaProvider         bool                         `json:"quota_provider"`
}

// configure answers plugin.register and plugin.reconfigure: it applies the config and describes the
// plugin. An unchanged config keeps the parked runs.
func (p *Plugin) configure(raw []byte) (any, error) {
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, newError("bad_request", 400, "plugin.register: %v", err)
	}
	cfg, err := parseConfig(req.ConfigYAML)
	if err != nil {
		return nil, newError("bad_config", 400, "%v", err)
	}
	if cur := p.rt.Load(); cur == nil || cur.cfg != cfg {
		client, err := p.newClient(cfg.Upstream)
		if err != nil {
			return nil, newError("bad_config", 400, "plugins.configs.cursor: %v", err)
		}
		if old := p.rt.Swap(&runtime{cfg: cfg, client: client, regs: map[string]*cursor.Registry{}}); old != nil {
			old.close()
		}
	}
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name: "cursor", Version: Version, Author: "mediavee", GitHubRepository: repository,
			ConfigFields: configFields(),
		},
		Capabilities: capabilities{
			AuthProvider: true, ModelProvider: true, Executor: true, QuotaProvider: true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats:  []string{"openai"},
			ExecutorOutputFormats: []string{"openai"},
		},
	}, nil
}

func configFields() []pluginapi.ConfigField {
	d := cursor.DefaultConfig()
	return []pluginapi.ConfigField{
		{Name: "api_url", Type: pluginapi.ConfigFieldTypeString, Default: d.APIURL, Description: "Host of the unary RPCs and the auth endpoints."},
		{Name: "agent_url", Type: pluginapi.ConfigFieldTypeString, Default: d.AgentURL, Description: "Host of the agent Run stream, used until Cursor announces its own."},
		{Name: "site_url", Type: pluginapi.ConfigFieldTypeString, Default: d.SiteURL, Description: "Host of the browser login page."},
		{Name: "client_version", Type: pluginapi.ConfigFieldTypeString, Default: d.ClientVersion, Description: "x-cursor-client-version sent to Cursor, with its cli- prefix."},
		{Name: "ghost_mode", Type: pluginapi.ConfigFieldTypeBoolean, Default: d.GhostMode, Description: "x-ghost-mode sent to Cursor."},
		{Name: "park_ttl", Type: pluginapi.ConfigFieldTypeString, Default: cursor.DefaultParkTTL.String(), Description: "How long a run parked on client tool calls waits for their results."},
		{Name: "max_parked", Type: pluginapi.ConfigFieldTypeInteger, Default: cursor.DefaultMaxParked, Description: "Runs parked per credential; one more closes the oldest."},
		{Name: "models_refresh_interval", Type: pluginapi.ConfigFieldTypeString, Default: defaultModelsRefreshInterval.String(), Description: "How often a credential is refreshed, which lists its models again."},
		{Name: "request_timeout", Type: pluginapi.ConfigFieldTypeString, Default: defaultRequestTimeout.String(), Description: "Upper bound of one request."},
	}
}

// Shutdown closes the parked runs and waits briefly for the plugin's goroutines.
func (p *Plugin) Shutdown() {
	if !p.closing.CompareAndSwap(false, true) {
		return
	}
	if rt := p.rt.Swap(nil); rt != nil {
		rt.close()
	}
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownWait):
	}
}

// goSafe runs fn on a tracked goroutine. A panic on a goroutine the plugin started would kill CPA, so
// it is logged and dropped.
func (p *Plugin) goSafe(name string, fn func()) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() {
			if v := recover(); v != nil {
				p.logf("error", "panic in %s: %v\n%s", name, v, debug.Stack())
			}
		}()
		fn()
	}()
}

// logf writes to CPA's log. Callers never pass a token, an API key or a response body.
func (p *Plugin) logf(level, format string, args ...any) {
	if p.host == nil {
		return
	}
	_, _ = p.host("host.log", map[string]any{"level": level, "message": "cursor: " + fmt.Sprintf(format, args...)})
}

// newError builds an ABI error. A zero status lets CPA answer 500.
func newError(code string, status int, format string, args ...any) *pluginabi.Error {
	return pluginabi.NewError(code, fmt.Sprintf(format, args...), status)
}

// asABIError keeps an ABI error as it is and wraps anything else.
func asABIError(err error) *pluginabi.Error {
	var abiErr *pluginabi.Error
	if errors.As(err, &abiErr) {
		return abiErr
	}
	return pluginabi.NewError("plugin_error", err.Error(), 500)
}
