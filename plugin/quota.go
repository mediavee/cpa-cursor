package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/mediavee/cpa-cursor/internal/cursor"
)

const quotaTimeout = 15 * time.Second

// quotaFetch answers quota.fetch: plan, spend and limit of the Auto and API pools, the limit status
// and the end of the billing cycle. Only the period usage is required; a missing plan or limit
// status leaves out its part of the answer.
func (p *Plugin) quotaFetch(raw []byte) (any, error) {
	var req pluginapi.QuotaFetchRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, newError("bad_request", 400, "quota.fetch: %v", err)
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
		return nil, newError("login_required", 401, "the Cursor session token is missing or has expired: reconnection required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), quotaTimeout)
	defer cancel()
	token := c.accessToken()

	var (
		usage    *cursor.CurrentPeriodUsage
		limit    *cursor.UsageLimitStatus
		plan     *cursor.PlanInfo
		usageErr error
	)
	var wg sync.WaitGroup
	run := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if v := recover(); v != nil {
					p.logf("error", "panic in quota.fetch: %v", v)
				}
			}()
			fn()
		}()
	}
	run(func() { usage, usageErr = rt.client.GetCurrentPeriodUsage(ctx, token) })
	run(func() { limit, _ = rt.client.GetUsageLimitStatusAndActiveGrants(ctx, token) })
	run(func() { plan, _ = rt.client.GetPlanInfo(ctx, token) })
	wg.Wait()
	if usageErr != nil {
		return nil, p.upstreamError(ctx, rt, token, usageErr)
	}
	return buildQuota(usage, limit, plan), nil
}

func buildQuota(usage *cursor.CurrentPeriodUsage, limit *cursor.UsageLimitStatus, plan *cursor.PlanInfo) pluginapi.QuotaFetchResponse {
	out := pluginapi.QuotaFetchResponse{}
	if plan != nil && plan.PlanInfo.PlanName != "" {
		out.Subscription = &pluginapi.QuotaSubscription{Plan: plan.PlanInfo.PlanName, TierName: plan.PlanInfo.Price}
	}

	u := usage.PlanUsage
	dollars := func(key, label string, cents float64) pluginapi.QuotaMetric {
		return pluginapi.QuotaMetric{Key: key, Label: label, Value: cents / 100, Format: "currency", Currency: "USD"}
	}
	percent := func(key, label string, v float64) pluginapi.QuotaMetric {
		return pluginapi.QuotaMetric{Key: key, Label: label, Value: v, Unit: "%", Format: "number"}
	}
	out.Summary = []pluginapi.QuotaMetric{
		dollars("total_spend", "Total spend", u.TotalSpend),
		dollars("limit", "Included limit", u.Limit),
		percent("total_percent_used", "Total used", u.TotalPercentUsed),
		percent("auto_percent_used", "Auto + Composer pool used", u.AutoPercentUsed),
		percent("api_percent_used", "API pool used", u.APIPercentUsed),
	}
	for _, m := range []struct {
		key, label string
		cents      float64
	}{
		{"included_spend", "Included spend", u.IncludedSpend},
		{"bonus_spend", "Bonus spend", u.BonusSpend},
		{"auto_spend", "Auto + Composer pool spend", u.AutoSpend},
		{"auto_limit", "Auto + Composer pool limit", u.AutoLimit},
		{"api_spend", "API pool spend", u.APISpend},
		{"api_limit", "API pool limit", u.APILimit},
	} {
		if m.cents != 0 {
			out.Summary = append(out.Summary, dollars(m.key, m.label, m.cents))
		}
	}

	cycleEnd := ""
	if t := usage.BillingCycleEnd.Time(); !t.IsZero() {
		cycleEnd = t.UTC().Format(time.RFC3339)
	}
	pool := func(name string, usedPercent float64, description string) pluginapi.QuotaGroup {
		return pluginapi.QuotaGroup{DisplayName: name, Buckets: []pluginapi.QuotaBucket{{
			Window: "billing cycle", RemainingFraction: remaining(usedPercent), ResetTime: cycleEnd, Description: description,
		}}}
	}
	out.Groups = []pluginapi.QuotaGroup{
		pool("Auto + Composer pool", u.AutoPercentUsed, fmt.Sprintf("billed for %d models (default, composer-*, grok, ...)", len(usage.AutoBucketModels))),
		pool("API pool", u.APIPercentUsed, "billed for every other model (Claude, GPT, Gemini, ...)"),
	}
	if limit != nil {
		out.Groups = append(out.Groups, limitGroup(limit.Policy))
	}
	return out
}

// limitGroup describes the account's limit status: not hit, or the stage reached and when it resets.
func limitGroup(s cursor.UsageLimitPolicyStatus) pluginapi.QuotaGroup {
	b := pluginapi.QuotaBucket{Window: "limit status", RemainingFraction: 1, Description: "no limit hit"}
	if t := s.ResetAtMs.Time(); !t.IsZero() {
		b.ResetTime = t.UTC().Format(time.RFC3339)
	}
	switch {
	case s.IsInSlowPool || s.Stage != "":
		b.RemainingFraction = 0
		b.Window = s.Stage
		if b.Window == "" {
			b.Window = "slow pool"
		}
		b.Description = joinNonEmpty(s.ErrorTitle, s.ErrorDetail)
		if s.IsInSlowPool {
			b.Description = joinNonEmpty(b.Description, fmt.Sprintf("slow pool, %d ms of added latency", s.SlownessMs))
		}
	}
	return pluginapi.QuotaGroup{DisplayName: "Limit status", Buckets: []pluginapi.QuotaBucket{b}}
}

// remaining turns a used percentage into the fraction left, within [0, 1].
func remaining(usedPercent float64) float64 {
	return max(0, min(1, 1-usedPercent/100))
}

func joinNonEmpty(parts ...string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += ": "
		}
		out += p
	}
	return out
}
