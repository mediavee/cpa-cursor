package plugin

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	agentv1 "github.com/mediavee/cpa-cursor/internal/gen/agent/v1"

	"github.com/mediavee/cpa-cursor/internal/cursor"
	"github.com/mediavee/cpa-cursor/internal/translate"
)

// modelsMinAge is how long model.for_auth reuses a credential's last listing: CPA asks again on every
// change of the credential, and AvailableModels is a few hundred KiB.
const modelsMinAge = 5 * time.Minute

// effortParameterIDs are the model parameters that carry reasoning effort.
var effortParameterIDs = []string{"effort", "reasoning", "reasoning_effort"}

// catalog is the account's model list: what CPA is told (infos) and what translate needs to build a
// request (models). It implements translate.Catalog and is immutable once built.
type catalog struct {
	at     time.Time
	models map[string]translate.Model
	infos  []pluginapi.ModelInfo
}

func (c *catalog) Lookup(slug string) (translate.Model, bool) {
	m, ok := c.models[slug]
	return m, ok
}

// buildCatalog pairs each usable slug (GetUsableModels) with its base model and variant
// (AvailableModels) to find the base id and the parameters Cursor expects next to the slug. A slug
// AvailableModels does not describe, such as "default", is sent as it is.
func buildCatalog(usable []*agentv1.ModelDetails, avail *cursor.AvailableModels, now time.Time) *catalog {
	type variantRef struct {
		base    *cursor.AvailableModel
		variant *cursor.ModelVariant
	}
	bySlug := map[string]variantRef{}
	if avail != nil {
		for i := range avail.Models {
			base := &avail.Models[i]
			for j := range base.Variants {
				if slug := base.Variants[j].LegacySlug; slug != "" {
					bySlug[slug] = variantRef{base, &base.Variants[j]}
				}
			}
		}
	}

	cat := &catalog{at: now, models: make(map[string]translate.Model, len(usable))}
	for _, d := range usable {
		slug := d.GetModelId()
		if slug == "" {
			continue
		}
		if _, dup := cat.models[slug]; dup {
			continue
		}
		model := translate.Model{ID: slug, Slug: slug, MaxMode: d.GetMaxMode()}
		info := pluginapi.ModelInfo{
			ID: slug, Object: "model", Created: now.Unix(), OwnedBy: ID, Type: ID,
			DisplayName: firstNonEmpty(d.GetDisplayName(), slug), Name: slug,
			SupportedParameters:       []string{"tools", "tool_choice"},
			SupportedInputModalities:  []string{"text"},
			SupportedOutputModalities: []string{"text"},
		}
		if ref, ok := bySlug[slug]; ok {
			model.ID = ref.base.Name
			model.Parameters = parameters(ref.variant)
			model.Effort = effortOf(ref.base)
			for _, v := range ref.base.Variants {
				if v.LegacySlug != "" {
					model.Variants = append(model.Variants, translate.Variant{Slug: v.LegacySlug, Parameters: parameters(&v)})
				}
			}
			info.ContextLength = ref.base.ContextTokenLimit
			if model.MaxMode && ref.base.ContextTokenLimitForMaxMode > 0 {
				info.ContextLength = ref.base.ContextTokenLimitForMaxMode
			}
			info.InputTokenLimit = info.ContextLength
			if ref.base.SupportsImages {
				info.SupportedInputModalities = append(info.SupportedInputModalities, "image")
			}
			if model.Effort != nil {
				info.SupportedParameters = append(info.SupportedParameters, "reasoning_effort")
			}
		}
		cat.models[slug] = model
		cat.infos = append(cat.infos, info)
	}
	return cat
}

func parameters(v *cursor.ModelVariant) []translate.Parameter {
	out := make([]translate.Parameter, 0, len(v.ParameterValues))
	for _, p := range v.ParameterValues {
		out = append(out, translate.Parameter{ID: p.ID, Value: p.Value})
	}
	return out
}

func effortOf(base *cursor.AvailableModel) *translate.EffortParameter {
	for _, def := range base.ParameterDefinitions {
		if !slices.Contains(effortParameterIDs, def.ID) {
			continue
		}
		var values []string
		for _, v := range def.Values() {
			values = append(values, v.Value)
		}
		if len(values) > 0 {
			return &translate.EffortParameter{ID: def.ID, Values: values}
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// catalogStore keeps the last catalog of each credential and merges concurrent fetches of one.
type catalogStore struct {
	mu      sync.Mutex
	byAuth  map[string]*catalog
	flights map[string]*flight
}

type flight struct {
	done chan struct{}
	cat  *catalog
	err  error
}

// get returns the catalog of authID if it is younger than maxAge (any age when maxAge is zero).
func (s *catalogStore) get(authID string, now time.Time, maxAge time.Duration) *catalog {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.byAuth[authID]
	if c == nil || (maxAge > 0 && now.Sub(c.at) >= maxAge) {
		return nil
	}
	return c
}

// catalogFor returns the credential's catalog, fetched from Cursor when the stored one is older than
// maxAge. Calls for one credential share a single fetch.
func (p *Plugin) catalogFor(ctx context.Context, rt *runtime, authID, token string, maxAge time.Duration) (*catalog, error) {
	s := &p.catalogs
	if c := s.get(authID, p.now(), maxAge); c != nil {
		return c, nil
	}
	s.mu.Lock()
	if f, ok := s.flights[authID]; ok {
		s.mu.Unlock()
		select {
		case <-f.done:
			return f.cat, f.err
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
	f := &flight{done: make(chan struct{})}
	if s.flights == nil {
		s.flights = map[string]*flight{}
	}
	s.flights[authID] = f
	s.mu.Unlock()

	f.cat, f.err = p.fetchCatalog(ctx, rt, token)
	s.mu.Lock()
	delete(s.flights, authID)
	if f.err == nil {
		if s.byAuth == nil {
			s.byAuth = map[string]*catalog{}
		}
		s.byAuth[authID] = f.cat
	}
	s.mu.Unlock()
	close(f.done)
	return f.cat, f.err
}

func (p *Plugin) fetchCatalog(ctx context.Context, rt *runtime, token string) (*catalog, error) {
	usable, err := rt.client.GetUsableModels(ctx, token)
	if err != nil {
		return nil, err
	}
	avail, err := rt.client.AvailableModels(ctx, token)
	if err != nil {
		// Without the parameters a slug is still sent as it is; only effort mapping is lost.
		p.logf("warn", "AvailableModels failed, models are listed without parameters: %v", err)
		avail = nil
	}
	return buildCatalog(usable, avail, p.now()), nil
}

// modelsForAuth answers model.for_auth: the models of one credential. A credential with no usable
// session token yet lists none; its models appear once the refresh gave it one.
func (p *Plugin) modelsForAuth(raw []byte) (any, error) {
	var req pluginapi.AuthModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, newError("bad_request", 400, "model.for_auth: %v", err)
	}
	rt, err := p.runtime()
	if err != nil {
		return nil, err
	}
	c, err := parseCredential(req.StorageJSON, req.Metadata)
	if err != nil {
		return nil, newError("bad_auth_file", 400, "%v", err)
	}
	if !c.usable(p.now()) {
		return pluginapi.ModelResponse{Provider: ID}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), exchangeTimeout)
	defer cancel()
	cat, err := p.catalogFor(ctx, rt, req.AuthID, c.accessToken(), modelsMinAge)
	if err != nil {
		if stale := p.catalogs.get(req.AuthID, p.now(), 0); stale != nil {
			p.logf("warn", "listing models of %s failed, keeping the previous list: %v", req.AuthID, err)
			return pluginapi.ModelResponse{Provider: ID, Models: stale.infos}, nil
		}
		return nil, p.upstreamError(ctx, rt, "", err)
	}
	return pluginapi.ModelResponse{Provider: ID, Models: cat.infos}, nil
}
