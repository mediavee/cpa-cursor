package translate

import (
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/mediavee/cpa-cursor/internal/gen/agent/v1"
)

// Catalog resolves the Cursor slug a client asked for. The plugin fills it from the account's
// GetUsableModels (usable slugs) and AiService/AvailableModels (base ids, parameters, variants).
type Catalog interface {
	Lookup(slug string) (Model, bool)
}

// Model is a usable slug with the base model and parameters Cursor expects on the wire.
type Model struct {
	// ID is the base model id sent as requested_model.model_id, for example "claude-opus-5-5".
	ID string
	// Slug is the usable slug sent as model_details.model_id, for example "claude-opus-5-5-high".
	Slug string
	// MaxMode is the variant's max mode flag.
	MaxMode bool
	// Parameters are the variant's parameter values, for example effort=high.
	Parameters []Parameter
	// Effort describes the parameter that carries reasoning effort; nil when the model has none.
	Effort *EffortParameter
	// Variants lists every usable variant of the base model, to find the slug of a changed parameter set.
	Variants []Variant
}

// Parameter is one model parameter value.
type Parameter struct {
	ID    string
	Value string
}

// EffortParameter is the model parameter that carries reasoning effort ("effort", "reasoning", ...)
// with the values the model offers.
type EffortParameter struct {
	ID     string
	Values []string
}

// Variant is one usable parameter combination of a base model and its slug.
type Variant struct {
	Slug       string
	Parameters []Parameter
}

// UnknownModelError reports a slug the catalog does not expose.
type UnknownModelError struct{ Slug string }

func (e *UnknownModelError) Error() string { return fmt.Sprintf("unknown Cursor model %q", e.Slug) }

// effortScale orders the effort values OpenAI and Cursor use, weakest first.
var effortScale = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// resolvedModel is the pair of model fields of an AgentRunRequest.
type resolvedModel struct {
	details   *agentv1.ModelDetails
	requested *agentv1.RequestedModel
}

// resolveModel looks up slug and applies the reasoning effort to the model's effort parameter.
// A model without an effort parameter ignores the effort, since the protocol cannot carry it.
// An effort the model does not offer maps to the nearest offered one, the weaker on a tie.
func resolveModel(cat Catalog, slug, effort string) (resolvedModel, error) {
	if cat == nil {
		return resolvedModel{}, fmt.Errorf("no model catalog")
	}
	m, ok := cat.Lookup(slug)
	if !ok {
		return resolvedModel{}, &UnknownModelError{Slug: slug}
	}
	params := slices.Clone(m.Parameters)
	detailsSlug := m.Slug
	if effort != "" && m.Effort != nil {
		if value, ok := nearestEffort(effort, m.Effort.Values); ok {
			params = setParameter(params, m.Effort.ID, value)
			for _, v := range m.Variants {
				if sameParameters(v.Parameters, params) {
					detailsSlug = v.Slug
					break
				}
			}
		}
	}
	requested := &agentv1.RequestedModel{ModelId: m.ID, MaxMode: m.MaxMode}
	for _, p := range params {
		requested.Parameters = append(requested.Parameters, &agentv1.RequestedModel_ModelParameterValue{Id: p.ID, Value: p.Value})
	}
	return resolvedModel{
		details:   &agentv1.ModelDetails{ModelId: detailsSlug, DisplayModelId: detailsSlug, DisplayName: detailsSlug, MaxMode: proto.Bool(m.MaxMode)},
		requested: requested,
	}, nil
}

// effortRank places an effort value on effortScale, or -1. Catalogs spell some values differently
// from OpenAI ("extra-high" for "xhigh"), so spellings are compared normalized.
func effortRank(v string) int {
	v = strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(v))
	if v == "extrahigh" {
		v = "xhigh"
	}
	return slices.Index(effortScale, v)
}

// nearestEffort picks the offered value closest to want and returns it exactly as the catalog
// spells it, since that is what Cursor accepts.
func nearestEffort(want string, offered []string) (string, bool) {
	if slices.Contains(offered, want) {
		return want, true
	}
	wantRank := effortRank(want)
	if wantRank < 0 {
		return "", false
	}
	best, bestRank := "", -1
	for _, o := range offered {
		rank := effortRank(o)
		if rank < 0 {
			continue
		}
		if bestRank < 0 || abs(rank-wantRank) < abs(bestRank-wantRank) ||
			(abs(rank-wantRank) == abs(bestRank-wantRank) && rank < bestRank) {
			best, bestRank = o, rank
		}
	}
	return best, bestRank >= 0
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func setParameter(params []Parameter, id, value string) []Parameter {
	for i := range params {
		if params[i].ID == id {
			params[i].Value = value
			return params
		}
	}
	return append(params, Parameter{ID: id, Value: value})
}

func sameParameters(a, b []Parameter) bool {
	if len(a) != len(b) {
		return false
	}
	for _, p := range a {
		if !slices.Contains(b, p) {
			return false
		}
	}
	return true
}
