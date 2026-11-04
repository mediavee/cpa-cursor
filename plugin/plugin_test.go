package plugin

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/mediavee/cpa-cursor/internal/cursor"
)

func TestRegisterDeclaresACompleteProvider(t *testing.T) {
	h := newHarness(t)
	var reg registration
	h.mustCall(pluginabi.MethodPluginRegister, map[string]any{"config_yaml": h.up.configYAML(), "schema_version": 10}, &reg)

	m := reg.Metadata
	if m.Name == "" || m.Version == "" || m.Author == "" || m.GitHubRepository == "" {
		t.Errorf("CPA drops a plugin with an empty name, version, author or repository: %+v", m)
	}
	c := reg.Capabilities
	if !c.AuthProvider || !c.ModelProvider || !c.Executor || !c.QuotaProvider {
		t.Errorf("capabilities = %+v, want auth, model, executor and quota", c)
	}
	if c.ExecutorModelScope != pluginapi.ExecutorModelScopeOAuth {
		t.Errorf("model scope = %q: models are per credential only", c.ExecutorModelScope)
	}
	if len(c.ExecutorInputFormats) != 1 || c.ExecutorInputFormats[0] != "openai" ||
		len(c.ExecutorOutputFormats) != 1 || c.ExecutorOutputFormats[0] != "openai" {
		t.Errorf("formats = %v / %v, want openai both ways (D8)", c.ExecutorInputFormats, c.ExecutorOutputFormats)
	}
	if reg.SchemaVersion == 0 || reg.SchemaVersion > pluginabi.SchemaVersion {
		t.Errorf("schema_version = %d, host speaks %d", reg.SchemaVersion, pluginabi.SchemaVersion)
	}
}

func TestIdentifiersAreTheProviderKey(t *testing.T) {
	h := newHarness(t)
	for _, method := range []string{pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier, pluginabi.MethodQuotaIdentifier} {
		var out struct{ Identifier string }
		h.mustCall(method, struct{}{}, &out)
		if out.Identifier != "cursor" {
			t.Errorf("%s = %q, want cursor", method, out.Identifier)
		}
	}
}

func TestNoStaticModels(t *testing.T) {
	h := newHarness(t)
	var out pluginapi.ModelResponse
	h.mustCall(pluginabi.MethodModelStatic, struct{}{}, &out)
	if len(out.Models) != 0 || len(out.Registrations) != 0 {
		t.Errorf("static models = %+v: they would be arbitrated against native ids and could unregister the executor", out)
	}
}

func TestConfigParsing(t *testing.T) {
	cfg, err := parseConfig([]byte("enabled: true\npriority: 3\nghost_mode: false\npark_ttl: 2m\nmax_parked: 7\nclient_version: cli-9\nmodels_refresh_interval: 30m\nrequest_timeout: 3m\napi_url: https://api.test\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := cursor.DefaultConfig()
	want.GhostMode, want.ClientVersion, want.APIURL = false, "cli-9", "https://api.test"
	if cfg.Upstream != want || cfg.ParkTTL.Minutes() != 2 || cfg.MaxParked != 7 || cfg.ModelsRefreshInterval.Minutes() != 30 || cfg.RequestTimeout.Minutes() != 3 {
		t.Errorf("config = %+v", cfg)
	}
	def, err := parseConfig(nil)
	if err != nil || def.Upstream != cursor.DefaultConfig() || def.ParkTTL != cursor.DefaultParkTTL || def.MaxParked != cursor.DefaultMaxParked {
		t.Errorf("empty config = %+v, %v: want the CLI's defaults", def, err)
	}
	for _, bad := range []string{"park_ttl: soon", "park_ttl: -1m", "max_parked: -2", "request_timeout: 0s", "api_url: ["} {
		if _, err := parseConfig([]byte(bad)); err == nil {
			t.Errorf("config %q was accepted", bad)
		}
	}
}

func TestReconfigureAppliesWithoutRestart(t *testing.T) {
	h := newHarness(t, "client_version: cli-first")
	token := h.token(time.Hour, "sub-1")

	run := func() string {
		h.up.setScript(nil)
		resp, perr := h.execute("auth-1", token, "composer-x", chatBody("composer-x", userMsg("hi")))
		if perr != nil {
			t.Fatalf("execute: %v", perr)
		}
		_ = resp
		r := <-h.up.runSeen
		return r.header.Get("X-Cursor-Client-Version")
	}
	if got := run(); got != "cli-first" {
		t.Fatalf("version before = %q", got)
	}
	h.register("client_version: cli-second")
	if got := run(); got != "cli-second" {
		t.Errorf("version after plugin.reconfigure = %q, want cli-second without restarting", got)
	}

	if _, e := h.call(pluginabi.MethodPluginReconfigure, mustJSON(map[string]any{"config_yaml": []byte("park_ttl: never")})); e == nil || e.HTTPStatus != 400 {
		t.Errorf("an invalid config must be refused with 400, got %+v", e)
	}
	if got := run(); got != "cli-second" {
		t.Errorf("a refused config changed the running one: version = %q", got)
	}
}

func TestPanicBecomesAnErrorEnvelope(t *testing.T) {
	h := newHarness(t)
	h.p.newClient = func(cursor.Config) (*cursor.Client, error) { panic("boom") }
	_, e := h.call(pluginabi.MethodPluginReconfigure, mustJSON(map[string]any{"config_yaml": []byte("client_version: other")}))
	if e == nil || e.Code != "plugin_panic" || e.HTTPStatus != 500 || !strings.Contains(e.Message, "boom") {
		t.Errorf("a panic must come back as an error envelope, got %+v", e)
	}
}

func TestUnknownMethod(t *testing.T) {
	h := newHarness(t)
	if _, e := h.call("no.such.method", nil); e == nil || e.Code != "unknown_method" {
		t.Errorf("error = %+v", e)
	}
}

func TestCallBeforeRegisterIsAnErrorNotACrash(t *testing.T) {
	p := New(newFakeHost().call)
	var env pluginabi.Envelope
	if err := json.Unmarshal(p.Call(pluginabi.MethodModelForAuth, []byte(`{"AuthID":"a"}`)), &env); err != nil {
		t.Fatal(err)
	}
	if env.OK {
		t.Error("model.for_auth succeeded before plugin.register")
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
