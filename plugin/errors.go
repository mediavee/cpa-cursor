package plugin

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"

	"github.com/mediavee/cpa-cursor/internal/cursor"
	aiserverv1 "github.com/mediavee/cpa-cursor/internal/gen/aiserver/v1"
	"github.com/mediavee/cpa-cursor/internal/translate"
)

const (
	// rateLimitRetryAfter is the cooldown after a short rate limit: Cursor does not say how long.
	rateLimitRetryAfter = 30
	// outdatedRetryAfter keeps the cooldown that CPA applies to any 5xx as short as it allows; a stale
	// client says nothing about the account.
	outdatedRetryAfter = 1
	// resetLookupTimeout bounds the extra calls that find out when a usage limit resets.
	resetLookupTimeout = 5 * time.Second
)

// upstreamError maps an error raised while serving a request to the status and retry_after CPA reads
// from a plugin error (the error code is dropped by CPA, so the message carries what a human needs).
// token is only used to ask Cursor when a usage limit resets.
func (p *Plugin) upstreamError(ctx context.Context, rt *runtime, token string, err error) *pluginabi.Error {
	var abiErr *pluginabi.Error
	if errors.As(err, &abiErr) {
		return abiErr
	}
	var unknown *translate.UnknownModelError
	switch {
	case errors.As(err, &unknown):
		return newError("unknown_model", 400, "Cursor model %q is not in this account's model list; list /v1/models again", unknown.Slug)
	case errors.Is(err, translate.ErrNoActiveMessage):
		return newError("bad_request", 400, "%v", err)
	case errors.Is(err, context.DeadlineExceeded):
		return newError("timeout", 504, "Cursor did not finish within request_timeout (%s)", rt.cfg.RequestTimeout)
	case errors.Is(err, context.Canceled):
		return newError("canceled", 499, "request canceled")
	case errors.Is(err, cursor.ErrStalled):
		return newError("stalled", 504, "Cursor stopped sending data: %v", err)
	}
	var pe *cursor.PanicError
	if errors.As(err, &pe) {
		return newError("plugin_panic", 500, "%v", pe)
	}

	var ce *cursor.Error
	if !errors.As(err, &ce) {
		if cursor.ClassOf(err) == cursor.ClassRetryable {
			return newError("upstream_unreachable", 503, "Cursor is unreachable: %v", err)
		}
		return newError("plugin_error", 500, "%v", err)
	}
	switch ce.Class {
	case cursor.ClassLogin:
		return newError("login_required", 401, "Cursor rejected the credential, reconnection required (log in again or provide a valid API key): %v", ce)
	case cursor.ClassUpgradeUsage:
		out := newError("usage_limit", 429, "Cursor usage limit reached: %v", ce)
		out.RetryAfterSeconds = p.usageResetSeconds(ctx, rt, token)
		return out
	case cursor.ClassUpgradeRate:
		out := newError("rate_limit", 429, "Cursor rate limit reached: %v", ce)
		out.RetryAfterSeconds = rateLimitRetryAfter
		return out
	case cursor.ClassPayment:
		return newError("payment_required", 402, "Cursor requires usage-based pricing to be enabled for this account: %v", ce)
	case cursor.ClassBlocked:
		return newError("blocked", 403, "Cursor refuses this account, model or region: %v", ce)
	case cursor.ClassOutdatedClient:
		out := newError("outdated_client", 503, "Cursor says this client is outdated: set plugins.configs.cursor.client_version to a current cursor-agent release (now %q): %v", rt.cfg.Upstream.ClientVersion, ce)
		out.RetryAfterSeconds = outdatedRetryAfter
		return out
	case cursor.ClassFatal:
		if ce.Code == aiserverv1.ErrorDetails_ERROR_MAX_TOKENS {
			return newError("max_tokens", 400, "Cursor stopped on its output limit: %v", ce)
		}
		return newError("rejected", 400, "Cursor rejected the request: %v", ce)
	}
	return newError("upstream_error", 503, "Cursor failed, the request may be retried: %v", ce)
}

// usageResetSeconds says how long a usage limit lasts: until reset_at_ms of the limit status, else
// until the end of the billing cycle. 0 lets CPA back off on its own when Cursor does not answer.
func (p *Plugin) usageResetSeconds(ctx context.Context, rt *runtime, token string) int {
	if token == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resetLookupTimeout)
	defer cancel()
	now := p.now()
	seconds := func(t time.Time) int {
		if !t.After(now) {
			return 0
		}
		return int(math.Ceil(t.Sub(now).Seconds()))
	}
	if st, err := rt.client.GetUsageLimitStatusAndActiveGrants(ctx, token); err == nil {
		if s := seconds(st.Policy.ResetAtMs.Time()); s > 0 {
			return s
		}
	}
	if u, err := rt.client.GetCurrentPeriodUsage(ctx, token); err == nil {
		return seconds(u.BillingCycleEnd.Time())
	}
	return 0
}

// isMaxTokens reports the one Cursor error that is a length stop rather than a failure.
func isMaxTokens(err error) bool {
	var ce *cursor.Error
	return errors.As(err, &ce) && ce.Code == aiserverv1.ErrorDetails_ERROR_MAX_TOKENS
}

// describe renders an ABI error for logs.
func describe(e *pluginabi.Error) string {
	return fmt.Sprintf("%d %s", e.HTTPStatus, e.Code)
}
