package plugin

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"

	aiserverv1 "github.com/mediavee/cpa-cursor/internal/gen/aiserver/v1"
)

const limitStatusPath = "/aiserver.v1.DashboardService/GetUsageLimitStatusAndActiveGrants"

// failWith makes the next run end with a Cursor error and returns what execute reports.
func (h *harness) failWith(code aiserverv1.ErrorDetails_Error, connectCode string) *pluginabi.Error {
	h.t.Helper()
	h.up.setScript(func(r *fakeRun) {
		r.endError(connectError(h.t, connectCode, "from cursor", errDetails(code, "Title", "")))
	})
	_, e := h.execute("auth-1", h.token(time.Hour, "s"), "composer-x", chatBody("composer-x", userMsg("hi")))
	if e == nil {
		h.t.Fatalf("%v: execute succeeded", code)
	}
	return e
}

func TestCursorErrorsBecomeTheStatusCPAActsOn(t *testing.T) {
	for _, tc := range []struct {
		name       string
		code       aiserverv1.ErrorDetails_Error
		connect    string
		status     int
		retryAfter int
		contains   string
	}{
		{"login", aiserverv1.ErrorDetails_ERROR_BAD_API_KEY, "unauthenticated", 401, 0, "reconnection required"},
		{"short rate limit", aiserverv1.ErrorDetails_ERROR_RATE_LIMITED, "resource_exhausted", 429, 30, "rate limit"},
		{"payment", aiserverv1.ErrorDetails_ERROR_USAGE_PRICING_REQUIRED, "resource_exhausted", 402, 0, "usage-based pricing"},
		{"blocked", aiserverv1.ErrorDetails_ERROR_MODEL_BLOCKED, "permission_denied", 403, 0, "refuses"},
		{"outdated client", aiserverv1.ErrorDetails_ERROR_OUTDATED_CLIENT, "failed_precondition", 503, 1, "client_version"},
		{"fatal", aiserverv1.ErrorDetails_ERROR_CONVERSATION_TOO_LONG, "invalid_argument", 400, 0, "rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			e := h.failWith(tc.code, tc.connect)
			if e.HTTPStatus != tc.status || e.RetryAfterSeconds != tc.retryAfter || !strings.Contains(e.Message, tc.contains) {
				t.Errorf("error = %d retry_after=%d %q, want %d retry_after=%d containing %q", e.HTTPStatus, e.RetryAfterSeconds, e.Message, tc.status, tc.retryAfter, tc.contains)
			}
		})
	}
}

func TestUsageLimitRetryAfterIsTheLimitReset(t *testing.T) {
	h := newHarness(t)
	reset := h.clock().Add(90 * time.Minute)
	h.up.reply(limitStatusPath, fmt.Sprintf(`{"usageLimitPolicyStatus":{"stage":"hard","limitType":"auto","resetAtMs":"%d"}}`, reset.UnixMilli()))
	e := h.failWith(aiserverv1.ErrorDetails_ERROR_PRO_USER_USAGE_LIMIT, "resource_exhausted")
	if e.HTTPStatus != 429 || e.RetryAfterSeconds != 5400 {
		t.Errorf("error = %d retry_after=%d, want 429 and 5400 s (reset_at_ms is 90 minutes away)", e.HTTPStatus, e.RetryAfterSeconds)
	}
}

func TestUsageLimitFallsBackToTheEndOfTheBillingCycle(t *testing.T) {
	h := newHarness(t)
	// The default fixture has no reset_at_ms; the cycle ends at 1794489320 (unix seconds).
	e := h.failWith(aiserverv1.ErrorDetails_ERROR_FREE_USER_USAGE_LIMIT, "resource_exhausted")
	want := int(1794489320 - h.clock().Unix())
	if e.HTTPStatus != 429 || e.RetryAfterSeconds != want {
		t.Errorf("error = %d retry_after=%d, want 429 and %d s (until the billing cycle ends)", e.HTTPStatus, e.RetryAfterSeconds, want)
	}
}

func TestUsageLimitWithoutAnyResetLeavesTheBackoffToCPA(t *testing.T) {
	h := newHarness(t)
	h.up.replyStatus(limitStatusPath, 500, `{"code":"internal","message":"x"}`)
	h.up.replyStatus("/aiserver.v1.DashboardService/GetCurrentPeriodUsage", 500, `{"code":"internal","message":"x"}`)
	e := h.failWith(aiserverv1.ErrorDetails_ERROR_PRO_USER_USAGE_LIMIT, "resource_exhausted")
	if e.HTTPStatus != 429 || e.RetryAfterSeconds != 0 {
		t.Errorf("error = %d retry_after=%d, want 429 and 0: no guess when Cursor does not say", e.HTTPStatus, e.RetryAfterSeconds)
	}
}

func TestTransportFailureIsRetryable(t *testing.T) {
	h := newHarness(t)
	h.up.setScript(func(r *fakeRun) { r.endError(`{"error":{"code":"unavailable","message":"overloaded"}}`) })
	_, e := h.execute("auth-1", h.token(time.Hour, "s"), "composer-x", chatBody("composer-x", userMsg("hi")))
	if e == nil || e.HTTPStatus != 503 {
		t.Errorf("error = %+v, want 503 so CPA cools the credential down briefly", e)
	}
}

func TestMaxTokensEndsTheAnswerWithLengthWhenTextWasProduced(t *testing.T) {
	h := newHarness(t)
	h.up.setScript(func(r *fakeRun) {
		r.text("cut o")
		r.endError(connectError(t, "invalid_argument", "too long", errDetails(aiserverv1.ErrorDetails_ERROR_MAX_TOKENS, "Max tokens", "")))
	})
	resp, e := h.execute("auth-1", h.token(time.Hour, "s"), "composer-x", chatBody("composer-x", userMsg("hi")))
	if e != nil {
		t.Fatalf("error = %+v: a length stop after output is an answer", e)
	}
	if content(resp) != "cut o" || resp.Choices[0].FinishReason != "length" {
		t.Errorf("response = %+v", resp.Choices[0])
	}
}
