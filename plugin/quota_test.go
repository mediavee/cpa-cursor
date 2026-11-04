package plugin

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func (h *harness) quota(storage map[string]any) (pluginapi.QuotaFetchResponse, *pluginabi.Error) {
	h.t.Helper()
	res, e := h.call(pluginabi.MethodQuotaFetch, mustJSON(pluginapi.QuotaFetchRequest{AuthID: "auth-1", Provider: "cursor", StorageJSON: authFile(storage)}))
	var out pluginapi.QuotaFetchResponse
	if e == nil {
		if err := json.Unmarshal(res, &out); err != nil {
			h.t.Fatal(err)
		}
	}
	return out, e
}

func TestQuotaReportsPlanSpendAndPools(t *testing.T) {
	h := newHarness(t)
	out, e := h.quota(map[string]any{"access_token": h.token(time.Hour, "s")})
	if e != nil {
		t.Fatalf("quota: %+v", e)
	}
	if out.Subscription == nil || out.Subscription.Plan != "Ultra" || out.Subscription.TierName != "$200/mo" {
		t.Errorf("subscription = %+v", out.Subscription)
	}
	metrics := map[string]pluginapi.QuotaMetric{}
	for _, m := range out.Summary {
		metrics[m.Key] = m
	}
	if m := metrics["total_spend"]; m.Value != 120 || m.Currency != "USD" {
		t.Errorf("total_spend = %+v, want 120 USD (12000 cents)", m)
	}
	if m := metrics["limit"]; m.Value != 200 {
		t.Errorf("limit = %+v", m)
	}
	if metrics["auto_percent_used"].Value != 60 || metrics["api_percent_used"].Value != 25 {
		t.Errorf("percentages = %+v", metrics)
	}
	if len(out.Groups) != 3 {
		t.Fatalf("groups = %+v", out.Groups)
	}
	auto, api, limit := out.Groups[0].Buckets[0], out.Groups[1].Buckets[0], out.Groups[2].Buckets[0]
	if auto.RemainingFraction != 0.4 || api.RemainingFraction != 0.75 {
		t.Errorf("remaining auto=%v api=%v, want 0.4 and 0.75", auto.RemainingFraction, api.RemainingFraction)
	}
	if auto.ResetTime != time.Unix(1794489320, 0).UTC().Format(time.RFC3339) {
		t.Errorf("reset = %q: pools reset with the billing cycle", auto.ResetTime)
	}
	if limit.RemainingFraction != 1 {
		t.Errorf("limit status = %+v, want no limit hit", limit)
	}
}

func TestQuotaShowsAHitLimit(t *testing.T) {
	h := newHarness(t)
	h.up.reply(limitStatusPath, `{"usageLimitPolicyStatus":{"stage":"hard","errorTitle":"Out of usage","resetAtMs":"1794000000000"}}`)
	out, e := h.quota(map[string]any{"access_token": h.token(time.Hour, "s")})
	if e != nil {
		t.Fatalf("quota: %+v", e)
	}
	b := out.Groups[2].Buckets[0]
	if b.RemainingFraction != 0 || b.Window != "hard" || b.Description != "Out of usage" || b.ResetTime == "" {
		t.Errorf("limit status = %+v", b)
	}
}

func TestQuotaSurvivesAMissingPlanAndLimitStatus(t *testing.T) {
	h := newHarness(t)
	h.up.replyStatus("/aiserver.v1.DashboardService/GetPlanInfo", 500, `{"code":"internal","message":"x"}`)
	h.up.replyStatus(limitStatusPath, 500, `{"code":"internal","message":"x"}`)
	out, e := h.quota(map[string]any{"access_token": h.token(time.Hour, "s")})
	if e != nil || out.Subscription != nil || len(out.Groups) != 2 || len(out.Summary) == 0 {
		t.Errorf("out = %+v err = %+v: only the period usage is required", out, e)
	}
}

func TestQuotaNeedsAUsableToken(t *testing.T) {
	h := newHarness(t)
	if _, e := h.quota(map[string]any{"api_key": "k"}); e == nil || e.HTTPStatus != 401 {
		t.Errorf("error = %+v, want 401", e)
	}
	h.up.replyStatus("/aiserver.v1.DashboardService/GetCurrentPeriodUsage", 401, `{"code":"unauthenticated","message":"x"}`)
	if _, e := h.quota(map[string]any{"access_token": h.token(time.Hour, "s")}); e == nil || e.HTTPStatus != 401 {
		t.Errorf("rejected token: error = %+v, want 401", e)
	}
}
