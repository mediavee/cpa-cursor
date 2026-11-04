package cursor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"

	agentv1 "github.com/mediavee/cpa-cursor/internal/gen/agent/v1"
)

const serverConfigTTL = time.Hour

// Int64 is a 64-bit integer that Connect JSON writes as a string.
type Int64 int64

func (i *Int64) UnmarshalJSON(b []byte) error {
	s := string(bytes.Trim(b, `"`))
	if s == "" || s == "null" {
		*i = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("cursor: %q is not an int64", s)
	}
	*i = Int64(n)
	return nil
}

// Time reads the value as Unix milliseconds.
func (i Int64) Time() time.Time {
	if i == 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(i))
}

// callUnary posts a Connect JSON request and returns the response body.
func (c *Client) callUnary(ctx context.Context, token, service, method string, body any) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("cursor: marshal %s/%s: %w", service, method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.APIURL+"/"+service+"/"+method, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("cursor: build %s/%s: %w", service, method, err)
	}
	c.setUnaryHeaders(req.Header, token)
	resp, err := c.unary.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cursor: %s/%s: %w", service, method, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxMessageBytes))
	if err != nil {
		return nil, fmt.Errorf("cursor: read %s/%s: %w", service, method, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseHTTPError(resp.StatusCode, data)
	}
	return data, nil
}

func (c *Client) setUnaryHeaders(h http.Header, token string) {
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("Connect-Protocol-Version", "1")
	h.Set("X-Ghost-Mode", fmt.Sprint(c.cfg.GhostMode))
	h.Set("X-Cursor-Client-Version", c.cfg.ClientVersion)
	h.Set("X-Cursor-Client-Type", "cli")
	h.Set("X-Request-Id", uuid.NewString())
	h.Set("User-Agent", userAgent)
}

func (c *Client) unaryInto(ctx context.Context, token, service, method string, body, out any) error {
	data, err := c.callUnary(ctx, token, service, method, body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("cursor: decode %s/%s: %w", service, method, err)
	}
	return nil
}

// ServerConfig is the part of aiserver.v1.ServerConfigService/GetServerConfig we use.
type ServerConfig struct {
	AgentURLConfig struct {
		AgentURL              string `json:"agentUrl"`
		AgentnURL             string `json:"agentnUrl"`
		LocalLoopInferenceURL string `json:"localLoopInferenceUrl"`
	} `json:"agentUrlConfig"`
}

// AgentURL is the host the CLI streams to: agentUrl in ghost mode, agentnUrl otherwise.
func (s *ServerConfig) AgentURL(ghostMode bool) string {
	if ghostMode {
		return s.AgentURLConfig.AgentURL
	}
	return s.AgentURLConfig.AgentnURL
}

// GetServerConfig returns the server configuration, cached for an hour. Once
// fetched, its agent host replaces Config.AgentURL for new runs.
func (c *Client) GetServerConfig(ctx context.Context, token string) (*ServerConfig, error) {
	c.mu.Lock()
	if c.serverCfg != nil && c.clock.Now().Sub(c.serverCfgAt) < serverConfigTTL {
		cached := c.serverCfg
		c.mu.Unlock()
		return cached, nil
	}
	c.mu.Unlock()

	var sc ServerConfig
	if err := c.unaryInto(ctx, token, "aiserver.v1.ServerConfigService", "GetServerConfig", struct{}{}, &sc); err != nil {
		return nil, err
	}
	if u := sc.AgentURL(c.cfg.GhostMode); u != "" {
		if parsed, err := url.Parse(u); err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			return nil, fmt.Errorf("cursor: server config announced an unusable agent URL %q", u)
		}
	}
	c.mu.Lock()
	c.serverCfg, c.serverCfgAt = &sc, c.clock.Now()
	c.mu.Unlock()
	return &sc, nil
}

// GetUsableModels lists the models the account can use, in the CLI's own message type.
func (c *Client) GetUsableModels(ctx context.Context, token string) ([]*agentv1.ModelDetails, error) {
	data, err := c.callUnary(ctx, token, "agent.v1.AgentService", "GetUsableModels", struct{}{})
	if err != nil {
		return nil, err
	}
	var resp agentv1.GetUsableModelsResponse
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("cursor: decode GetUsableModels: %w", err)
	}
	return resp.GetModels(), nil
}

// ParameterValue is one option of a model parameter.
type ParameterValue struct {
	Value              string `json:"value"`
	DisplayName        string `json:"displayName"`
	IncreasesModelCost bool   `json:"increasesModelCost"`
}

// ParameterDefinition describes a model parameter such as context, effort or fast.
type ParameterDefinition struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ParameterType struct {
		EnumParameter    *struct{ Values []ParameterValue } `json:"enumParameter"`
		BooleanParameter *struct{ Values []ParameterValue } `json:"booleanParameter"`
	} `json:"parameterType"`
}

// Values are the allowed values, whatever the parameter's type.
func (p ParameterDefinition) Values() []ParameterValue {
	switch {
	case p.ParameterType.EnumParameter != nil:
		return p.ParameterType.EnumParameter.Values
	case p.ParameterType.BooleanParameter != nil:
		return p.ParameterType.BooleanParameter.Values
	}
	return nil
}

// ModelVariant is one concrete combination of parameters.
type ModelVariant struct {
	ParameterValues []struct {
		ID    string `json:"id"`
		Value string `json:"value"`
	} `json:"parameterValues"`
	DisplayName                 string `json:"displayName"`
	VariantStringRepresentation string `json:"variantStringRepresentation"`
	LegacySlug                  string `json:"legacySlug"`
	IsMaxMode                   bool   `json:"isMaxMode"`
	IsDefaultMaxConfig          bool   `json:"isDefaultMaxConfig"`
	IsDefaultNonMaxConfig       bool   `json:"isDefaultNonMaxConfig"`
}

// AvailableModel is one base model of aiserver.v1.AiService/AvailableModels.
type AvailableModel struct {
	Name                        string                `json:"name"`
	ServerModelName             string                `json:"serverModelName"`
	ClientDisplayName           string                `json:"clientDisplayName"`
	DefaultOn                   bool                  `json:"defaultOn"`
	SupportsAgent               bool                  `json:"supportsAgent"`
	SupportsThinking            bool                  `json:"supportsThinking"`
	SupportsImages              bool                  `json:"supportsImages"`
	SupportsMaxMode             bool                  `json:"supportsMaxMode"`
	SupportsNonMaxMode          bool                  `json:"supportsNonMaxMode"`
	ContextTokenLimit           int64                 `json:"contextTokenLimit"`
	ContextTokenLimitForMaxMode int64                 `json:"contextTokenLimitForMaxMode"`
	DegradationStatus           string                `json:"degradationStatus"`
	IDAliases                   []string              `json:"idAliases"`
	ParameterDefinitions        []ParameterDefinition `json:"parameterDefinitions"`
	Variants                    []ModelVariant        `json:"variants"`
}

// AvailableModels is the response of aiserver.v1.AiService/AvailableModels.
type AvailableModels struct {
	Models []AvailableModel `json:"models"`
}

// AvailableModels sends the CLI's own request, which makes the server describe
// each model's parameters and variants.
func (c *Client) AvailableModels(ctx context.Context, token string) (*AvailableModels, error) {
	var out AvailableModels
	req := map[string]any{"useModelParameters": true, "doNotUseMarkdown": true, "additionalModelNames": []string{}}
	if err := c.unaryInto(ctx, token, "aiserver.v1.AiService", "AvailableModels", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PlanUsage is the spend of the current billing period, in cents, split into
// the Auto and API pools.
type PlanUsage struct {
	TotalSpend       float64 `json:"totalSpend"`
	IncludedSpend    float64 `json:"includedSpend"`
	BonusSpend       float64 `json:"bonusSpend"`
	Limit            float64 `json:"limit"`
	AutoSpend        float64 `json:"autoSpend"`
	AutoLimit        float64 `json:"autoLimit"`
	APISpend         float64 `json:"apiSpend"`
	APILimit         float64 `json:"apiLimit"`
	AutoPercentUsed  float64 `json:"autoPercentUsed"`
	APIPercentUsed   float64 `json:"apiPercentUsed"`
	TotalPercentUsed float64 `json:"totalPercentUsed"`
}

// CurrentPeriodUsage is the response of DashboardService/GetCurrentPeriodUsage.
type CurrentPeriodUsage struct {
	BillingCycleStart Int64     `json:"billingCycleStart"`
	BillingCycleEnd   Int64     `json:"billingCycleEnd"`
	PlanUsage         PlanUsage `json:"planUsage"`
	DisplayMessage    string    `json:"displayMessage"`
	// AutoBucketModels are the models billed to the Auto pool; the others bill the API pool.
	AutoBucketModels []string `json:"autoBucketModels"`
}

func (c *Client) GetCurrentPeriodUsage(ctx context.Context, token string) (*CurrentPeriodUsage, error) {
	var out CurrentPeriodUsage
	if err := c.unaryInto(ctx, token, "aiserver.v1.DashboardService", "GetCurrentPeriodUsage", struct{}{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UsageLimitPolicyStatus says whether a limit is hit and when it resets.
type UsageLimitPolicyStatus struct {
	IsInSlowPool       bool     `json:"isInSlowPool"`
	ErrorTitle         string   `json:"errorTitle"`
	ErrorDetail        string   `json:"errorDetail"`
	SlownessMs         int64    `json:"slownessMs"`
	LimitType          string   `json:"limitType"`
	Stage              string   `json:"stage"`
	ResetAtMs          Int64    `json:"resetAtMs"`
	ResetDaysRemaining int64    `json:"resetDaysRemaining"`
	AllowedModelIDs    []string `json:"allowedModelIds"`
}

// UsageLimitStatus is the response of DashboardService/GetUsageLimitStatusAndActiveGrants.
type UsageLimitStatus struct {
	Policy UsageLimitPolicyStatus `json:"usageLimitPolicyStatus"`
}

func (c *Client) GetUsageLimitStatusAndActiveGrants(ctx context.Context, token string) (*UsageLimitStatus, error) {
	var out UsageLimitStatus
	if err := c.unaryInto(ctx, token, "aiserver.v1.DashboardService", "GetUsageLimitStatusAndActiveGrants", struct{}{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PlanInfo is the response of DashboardService/GetPlanInfo.
type PlanInfo struct {
	PlanInfo struct {
		PlanName            string `json:"planName"`
		Price               string `json:"price"`
		BillingCycleEnd     Int64  `json:"billingCycleEnd"`
		IncludedUsagePeriod string `json:"includedUsagePeriod"`
	} `json:"planInfo"`
}

func (c *Client) GetPlanInfo(ctx context.Context, token string) (*PlanInfo, error) {
	var out PlanInfo
	if err := c.unaryInto(ctx, token, "aiserver.v1.DashboardService", "GetPlanInfo", struct{}{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
