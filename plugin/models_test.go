package plugin

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func (h *harness) models(authID string, storage map[string]any) pluginapi.ModelResponse {
	h.t.Helper()
	var out pluginapi.ModelResponse
	h.mustCall(pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{AuthID: authID, AuthProvider: "cursor", StorageJSON: authFile(storage)}, &out)
	return out
}

func TestModelsForAuthListsTheAccountSlugs(t *testing.T) {
	h := newHarness(t)
	out := h.models("auth-1", map[string]any{"access_token": h.token(time.Hour, "s")})
	if out.Provider != "cursor" {
		t.Errorf("provider = %q", out.Provider)
	}
	byID := map[string]pluginapi.ModelInfo{}
	var ids []string
	for _, m := range out.Models {
		byID[m.ID] = m
		ids = append(ids, m.ID)
	}
	if want := []string{"default", "composer-x", "composer-x-fast", "claude-t-low", "claude-t-high"}; !slices.Equal(ids, want) {
		t.Errorf("model ids = %v, want the GetUsableModels slugs %v (D10)", ids, want)
	}
	if m := byID["composer-x"]; m.ContextLength != 200000 || m.DisplayName != "Composer X" || m.OwnedBy != "cursor" || m.Object != "model" {
		t.Errorf("composer-x = %+v", m)
	}
	high := byID["claude-t-high"]
	if high.ContextLength != 1000000 {
		t.Errorf("a max-mode slug takes the max-mode context: %d", high.ContextLength)
	}
	if !slices.Contains(high.SupportedInputModalities, "image") || !slices.Contains(high.SupportedParameters, "reasoning_effort") {
		t.Errorf("claude-t-high = %+v", high)
	}
	if slices.Contains(byID["composer-x"].SupportedInputModalities, "image") || slices.Contains(byID["composer-x"].SupportedParameters, "reasoning_effort") {
		t.Errorf("composer-x must not claim images or effort: %+v", byID["composer-x"])
	}
}

func TestCatalogHoldsWhatTranslateNeeds(t *testing.T) {
	h := newHarness(t)
	h.models("auth-1", map[string]any{"access_token": h.token(time.Hour, "s")})
	cat := h.p.catalogs.get("auth-1", h.clock(), 0)
	if cat == nil {
		t.Fatal("the catalog was not kept for the executor")
	}

	composer, ok := cat.Lookup("composer-x")
	if !ok || composer.ID != "composer-x" || len(composer.Parameters) != 1 || composer.Parameters[0].ID != "fast" || composer.Parameters[0].Value != "false" {
		t.Errorf("composer-x = %+v: the slug of a variant is sent as base id + its parameters", composer)
	}
	fast, _ := cat.Lookup("composer-x-fast")
	if fast.ID != "composer-x" || fast.Parameters[0].Value != "true" || len(fast.Variants) != 2 {
		t.Errorf("composer-x-fast = %+v", fast)
	}
	claude, _ := cat.Lookup("claude-t-high")
	if claude.ID != "claude-t" || !claude.MaxMode || claude.Effort == nil || claude.Effort.ID != "effort" || !slices.Equal(claude.Effort.Values, []string{"low", "high"}) {
		t.Errorf("claude-t-high = %+v", claude)
	}
	auto, ok := cat.Lookup("default")
	if !ok || auto.ID != "default" || auto.Slug != "default" || len(auto.Parameters) != 0 {
		t.Errorf("default = %+v: a slug AvailableModels does not describe is sent as it is", auto)
	}
	if _, ok := cat.Lookup("gpt-nope"); ok {
		t.Error("an unlisted slug resolved")
	}
}

func TestModelsAreListedWithoutParametersWhenAvailableModelsFails(t *testing.T) {
	h := newHarness(t)
	h.up.replyStatus("/aiserver.v1.AiService/AvailableModels", http.StatusInternalServerError, `{"code":"internal","message":"x"}`)
	out := h.models("auth-1", map[string]any{"access_token": h.token(time.Hour, "s")})
	if len(out.Models) != 5 || !h.host.logged("AvailableModels failed") {
		t.Errorf("models = %d, logged = %v", len(out.Models), h.host.logged("AvailableModels failed"))
	}
}

func TestModelsAreCachedBetweenAuthChanges(t *testing.T) {
	h := newHarness(t)
	storage := map[string]any{"access_token": h.token(time.Hour, "s")}
	h.models("auth-1", storage)
	h.models("auth-1", storage)
	if n := h.up.count("/aiserver.v1.AiService/AvailableModels"); n != 1 {
		t.Errorf("AvailableModels fetched %d times for two calls within five minutes, want 1", n)
	}
	h.models("auth-2", storage)
	if n := h.up.count("/aiserver.v1.AiService/AvailableModels"); n != 2 {
		t.Errorf("another credential must have its own list: fetched %d times", n)
	}
	h.advance(6 * time.Minute)
	h.models("auth-1", map[string]any{"access_token": h.token(time.Hour, "s")})
	if n := h.up.count("/aiserver.v1.AiService/AvailableModels"); n != 3 {
		t.Errorf("a stale list must be fetched again: fetched %d times", n)
	}
}

func TestModelsKeepThePreviousListWhenCursorFails(t *testing.T) {
	h := newHarness(t)
	storage := map[string]any{"access_token": h.token(time.Hour, "s")}
	h.models("auth-1", storage)
	h.up.replyStatus("/agent.v1.AgentService/GetUsableModels", http.StatusServiceUnavailable, `{"code":"unavailable","message":"down"}`)
	h.advance(10 * time.Minute)
	out := h.models("auth-1", map[string]any{"access_token": h.token(time.Hour, "s")})
	if len(out.Models) != 5 || !h.host.logged("keeping the previous list") {
		t.Errorf("models = %d, want the 5 previous ones kept and a warning", len(out.Models))
	}

	if _, e := h.call(pluginabi.MethodModelForAuth, mustJSON(pluginapi.AuthModelRequest{AuthID: "never-listed", StorageJSON: authFile(storage)})); e == nil {
		t.Error("a credential never listed must report the failure")
	}
}

func TestACredentialWithoutAUsableTokenListsNoModels(t *testing.T) {
	h := newHarness(t)
	for name, storage := range map[string]map[string]any{
		"key only":      {"api_key": "k"},
		"expired token": {"access_token": h.token(-time.Minute, "s")},
	} {
		out := h.models("auth-"+name, storage)
		if len(out.Models) != 0 {
			t.Errorf("%s: models = %v, want none until the refresh gave a token", name, out.Models)
		}
	}
	if n := h.up.count("/agent.v1.AgentService/GetUsableModels"); n != 0 {
		t.Errorf("Cursor was called %d times with no usable token", n)
	}
}
