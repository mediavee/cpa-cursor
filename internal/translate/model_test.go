package translate_test

import (
	"encoding/json"
	"errors"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/mediavee/cpa-cursor/internal/translate"
)

// Catalog entries shaped like AiService/AvailableModels rows (47 base models, observed 2026-11-04).
var modelCatalog = catalog{
	"composer-2.5": {
		ID: "composer-2.5", Slug: "composer-2.5",
		Parameters: []translate.Parameter{{ID: "fast", Value: "false"}},
	},
	"default": {ID: "default", Slug: "default", MaxMode: false},
	"claude-opus-5-5-medium": {
		ID: "claude-opus-5-5", Slug: "claude-opus-5-5-medium",
		Parameters: []translate.Parameter{{ID: "context", Value: "300k"}, {ID: "effort", Value: "medium"}, {ID: "fast", Value: "false"}},
		Effort:     &translate.EffortParameter{ID: "effort", Values: []string{"low", "medium", "high", "xhigh", "max"}},
		Variants: []translate.Variant{
			{Slug: "claude-opus-5-5-low", Parameters: []translate.Parameter{{ID: "context", Value: "300k"}, {ID: "effort", Value: "low"}, {ID: "fast", Value: "false"}}},
			{Slug: "claude-opus-5-5-medium", Parameters: []translate.Parameter{{ID: "context", Value: "300k"}, {ID: "effort", Value: "medium"}, {ID: "fast", Value: "false"}}},
			{Slug: "claude-opus-5-5-high", Parameters: []translate.Parameter{{ID: "context", Value: "300k"}, {ID: "effort", Value: "high"}, {ID: "fast", Value: "false"}}},
		},
	},
	// As captured: gpt-5.4 offers "extra-high" where OpenAI says "xhigh".
	"gpt-5.4-medium": {
		ID: "gpt-5.4", Slug: "gpt-5.4-medium",
		Parameters: []translate.Parameter{{ID: "context", Value: "272k"}, {ID: "reasoning", Value: "medium"}, {ID: "fast", Value: "false"}},
		Effort:     &translate.EffortParameter{ID: "reasoning", Values: []string{"none", "low", "medium", "high", "extra-high"}},
		Variants: []translate.Variant{
			{Slug: "gpt-5.4-medium", Parameters: []translate.Parameter{{ID: "context", Value: "272k"}, {ID: "reasoning", Value: "medium"}, {ID: "fast", Value: "false"}}},
			{Slug: "gpt-5.4-high", Parameters: []translate.Parameter{{ID: "context", Value: "272k"}, {ID: "reasoning", Value: "high"}, {ID: "fast", Value: "false"}}},
			{Slug: "gpt-5.4-xhigh", Parameters: []translate.Parameter{{ID: "context", Value: "272k"}, {ID: "reasoning", Value: "extra-high"}, {ID: "fast", Value: "false"}}},
		},
	},
	// As captured by GetUsableModels: this route is Max mode.
	"claude-opus-5-5-high-fast": {
		ID: "claude-opus-5-5", Slug: "claude-opus-5-5-high-fast", MaxMode: true,
		Parameters: []translate.Parameter{{ID: "context", Value: "1m"}, {ID: "effort", Value: "high"}, {ID: "fast", Value: "true"}},
	},
	"gpt-5.4-mini-low": {
		ID: "gpt-5.4-mini", Slug: "gpt-5.4-mini-low", MaxMode: true,
		Parameters: []translate.Parameter{{ID: "reasoning", Value: "low"}},
		Effort:     &translate.EffortParameter{ID: "reasoning", Values: []string{"none", "low", "medium", "high", "xhigh"}},
		Variants: []translate.Variant{
			{Slug: "gpt-5.4-mini-none", Parameters: []translate.Parameter{{ID: "reasoning", Value: "none"}}},
			{Slug: "gpt-5.4-mini-low", Parameters: []translate.Parameter{{ID: "reasoning", Value: "low"}}},
			{Slug: "gpt-5.4-mini-medium", Parameters: []translate.Parameter{{ID: "reasoning", Value: "medium"}}},
		},
	},
}

func TestModelResolution(t *testing.T) {
	tests := []struct {
		name          string
		slug, effort  string
		wantDetails   string // model_details.model_id
		wantRequested string // requested_model as JSON
	}{
		{"slug without parameters", "default", "",
			"default", `{"modelId":"default"}`},
		{"slug with a fixed parameter", "composer-2.5", "",
			"composer-2.5", `{"modelId":"composer-2.5","parameters":[{"id":"fast","value":"false"}]}`},
		{"the base id and the parameters go to requested_model, the slug to model_details", "claude-opus-5-5-medium", "",
			"claude-opus-5-5-medium", `{"modelId":"claude-opus-5-5","parameters":[{"id":"context","value":"300k"},{"id":"effort","value":"medium"},{"id":"fast","value":"false"}]}`},
		{"effort sets the model's effort parameter and picks the matching slug", "claude-opus-5-5-medium", "high",
			"claude-opus-5-5-high", `{"modelId":"claude-opus-5-5","parameters":[{"id":"context","value":"300k"},{"id":"effort","value":"high"},{"id":"fast","value":"false"}]}`},
		{"the effort parameter is not always called effort", "gpt-5.4-mini-low", "none",
			"gpt-5.4-mini-none", `{"modelId":"gpt-5.4-mini","maxMode":true,"parameters":[{"id":"reasoning","value":"none"}]}`},
		{"an effort without a variant keeps the slug but still sets the parameter", "claude-opus-5-5-medium", "max",
			"claude-opus-5-5-medium", `{"modelId":"claude-opus-5-5","parameters":[{"id":"context","value":"300k"},{"id":"effort","value":"max"},{"id":"fast","value":"false"}]}`},
		{"an effort the model does not offer maps to the nearest one; a tie goes to the weaker", "gpt-5.4-mini-low", "minimal",
			"gpt-5.4-mini-none", `{"modelId":"gpt-5.4-mini","maxMode":true,"parameters":[{"id":"reasoning","value":"none"}]}`},
		{"an effort below the offered range maps to the weakest offered", "claude-opus-5-5-medium", "minimal",
			"claude-opus-5-5-low", `{"modelId":"claude-opus-5-5","parameters":[{"id":"context","value":"300k"},{"id":"effort","value":"low"},{"id":"fast","value":"false"}]}`},
		{"an effort above the offered range maps to the strongest offered", "gpt-5.4-mini-low", "max",
			"gpt-5.4-mini-low", `{"modelId":"gpt-5.4-mini","maxMode":true,"parameters":[{"id":"reasoning","value":"xhigh"}]}`},
		{"the catalog's own spelling is sent: xhigh reaches extra-high, not high", "gpt-5.4-medium", "xhigh",
			"gpt-5.4-xhigh", `{"modelId":"gpt-5.4","parameters":[{"id":"context","value":"272k"},{"id":"reasoning","value":"extra-high"},{"id":"fast","value":"false"}]}`},
		{"an effort spelled as the catalog spells it is kept", "gpt-5.4-medium", "high",
			"gpt-5.4-high", `{"modelId":"gpt-5.4","parameters":[{"id":"context","value":"272k"},{"id":"reasoning","value":"high"},{"id":"fast","value":"false"}]}`},
		{"a model without an effort parameter ignores the effort", "composer-2.5", "high",
			"composer-2.5", `{"modelId":"composer-2.5","parameters":[{"id":"fast","value":"false"}]}`},
		{"an unknown effort value is ignored", "claude-opus-5-5-medium", "turbo",
			"claude-opus-5-5-medium", `{"modelId":"claude-opus-5-5","parameters":[{"id":"context","value":"300k"},{"id":"effort","value":"medium"},{"id":"fast","value":"false"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{
				"model": tt.slug, "reasoning_effort": tt.effort,
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			})
			run, err := translate.Translate(decode(t, string(body)), translate.Options{Catalog: modelCatalog})
			if err != nil {
				t.Fatal(err)
			}
			if got := run.Request.GetModelDetails().GetModelId(); got != tt.wantDetails {
				t.Errorf("model_details.model_id = %q, want %q", got, tt.wantDetails)
			}
			if got := run.Request.GetModelDetails().GetDisplayModelId(); got != tt.wantDetails {
				t.Errorf("model_details.display_model_id = %q, want %q", got, tt.wantDetails)
			}
			got, _ := protojson.Marshal(run.Request.GetRequestedModel())
			if !jsonEqual(t, got, tt.wantRequested) {
				t.Errorf("requested_model\n got: %s\nwant: %s", got, tt.wantRequested)
			}
		})
	}
}

func TestMaxModeOnBothModelFields(t *testing.T) {
	for slug, want := range map[string]bool{"claude-opus-5-5-high-fast": true, "composer-2.5": false} {
		run, err := translate.Translate(decode(t, `{"model":"`+slug+`","messages":[{"role":"user","content":"hi"}]}`), translate.Options{Catalog: modelCatalog})
		if err != nil {
			t.Fatal(err)
		}
		if got := run.Request.GetRequestedModel().GetMaxMode(); got != want {
			t.Errorf("%s: requested_model.max_mode = %v, want %v", slug, got, want)
		}
		details := run.Request.GetModelDetails()
		if details.MaxMode == nil || *details.MaxMode != want {
			t.Errorf("%s: model_details.max_mode = %v, want %v", slug, details.MaxMode, want)
		}
	}
}

func TestModelErrors(t *testing.T) {
	req := decode(t, `{"model":"nope","messages":[{"role":"user","content":"hi"}]}`)
	_, err := translate.Translate(req, translate.Options{Catalog: modelCatalog})
	var unknown *translate.UnknownModelError
	if !errors.As(err, &unknown) || unknown.Slug != "nope" {
		t.Errorf("unknown slug: error = %v, want UnknownModelError for nope", err)
	}
	if _, err := translate.Translate(req, translate.Options{}); err == nil {
		t.Error("a missing catalog must be an error")
	}
}

func jsonEqual(t *testing.T, got []byte, want string) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatal(err)
	}
	ga, _ := json.Marshal(a)
	gb, _ := json.Marshal(b)
	return string(ga) == string(gb)
}
