package cursor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"google.golang.org/protobuf/proto"

	aiserverv1 "github.com/mediavee/cpa-cursor/internal/gen/aiserver/v1"
)

// ErrorClass is how the caller should react to a Cursor error. The mapping
// to HTTP statuses and account cooldowns belongs to the caller.
type ErrorClass string

const (
	// ClassLogin: the credential is missing, expired or rejected.
	ClassLogin ErrorClass = "login"
	// ClassUpgradeUsage: a usage limit is hit; it resets with the billing cycle or a limit window.
	ClassUpgradeUsage ErrorClass = "upgrade_usage"
	// ClassUpgradeRate: a short rate limit is hit.
	ClassUpgradeRate ErrorClass = "upgrade_rate"
	// ClassPayment: usage-based pricing must be enabled before more requests.
	ClassPayment ErrorClass = "payment"
	// ClassBlocked: the account, the model or the region is refused.
	ClassBlocked ErrorClass = "blocked"
	// ClassOutdatedClient: the client version is too old; the account is not at fault.
	ClassOutdatedClient ErrorClass = "outdated_client"
	// ClassRetryable: the same request may succeed later.
	ClassRetryable ErrorClass = "retryable"
	// ClassFatal: retrying the same request will not help.
	ClassFatal ErrorClass = "fatal"
)

// Error is a Connect error, enriched with aiserver.v1.ErrorDetails when the
// server attached them.
type Error struct {
	// Class is the classification of research/cursor-protocol.md §5.3.
	Class ErrorClass
	// ConnectCode is the Connect code, for example "resource_exhausted".
	ConnectCode string
	// Message is the Connect message.
	Message string
	// Code is the ErrorDetails.Error enum; ERROR_UNSPECIFIED when absent.
	Code aiserverv1.ErrorDetails_Error
	// Title and Detail come from CustomErrorDetails.
	Title, Detail string
	// IsRetryable is CustomErrorDetails.is_retryable, nil when the server did not say.
	IsRetryable *bool
	// ActionRequired is analytics_metadata.action_required, for example "login" or "upgrade".
	ActionRequired string
	// HTTPStatus is the HTTP status of the response that carried the error, 0 inside a stream.
	HTTPStatus int
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("cursor: ")
	if e.Code != aiserverv1.ErrorDetails_ERROR_UNSPECIFIED {
		b.WriteString(e.Code.String())
	} else {
		b.WriteString(e.ConnectCode)
	}
	switch {
	case e.Title != "" && e.Detail != "":
		b.WriteString(": " + e.Title + ": " + e.Detail)
	case e.Title != "":
		b.WriteString(": " + e.Title)
	case e.Message != "":
		b.WriteString(": " + e.Message)
	}
	return b.String()
}

// ClassOf classifies any error returned by this package. Transport failures
// and stalls are retryable; cancellations and unknown errors have no class.
func ClassOf(err error) ErrorClass {
	var ce *Error
	switch {
	case errors.As(err, &ce):
		return ce.Class
	case errors.Is(err, ErrStalled), errors.Is(err, io.ErrUnexpectedEOF):
		return ClassRetryable
	case errors.Is(err, context.Canceled), errors.Is(err, ErrClosed):
		return ""
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return ClassRetryable
	}
	return ""
}

// connectError is the JSON shape of a Connect error, in an end-of-stream
// frame or in the body of a failed unary call.
type connectError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details []struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"details"`
}

// parseEndStream decodes the payload of an end-of-stream frame. It returns
// nil for a clean end.
func parseEndStream(payload []byte) error {
	var env struct {
		Error *connectError `json:"error"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return fmt.Errorf("cursor: malformed end-of-stream frame: %w", err)
	}
	if env.Error == nil {
		return nil
	}
	return newError(env.Error, 0)
}

// parseHTTPError builds the error for a non-2xx response. body may be empty
// or not Connect JSON.
func parseHTTPError(status int, body []byte) error {
	var ce connectError
	if err := json.Unmarshal(body, &ce); err != nil || ce.Code == "" {
		ce = connectError{Code: connectCodeFromStatus(status), Message: strings.TrimSpace(string(body))}
		if len(ce.Message) > 300 {
			ce.Message = ce.Message[:300]
		}
		if ce.Message == "" {
			ce.Message = http.StatusText(status)
		}
	}
	return newError(&ce, status)
}

func newError(ce *connectError, status int) *Error {
	e := &Error{ConnectCode: ce.Code, Message: ce.Message, HTTPStatus: status}
	for _, d := range ce.Details {
		if d.Type != "aiserver.v1.ErrorDetails" && d.Type != "type.googleapis.com/aiserver.v1.ErrorDetails" {
			continue
		}
		raw, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(d.Value, "="))
		if err != nil {
			continue
		}
		var det aiserverv1.ErrorDetails
		if proto.Unmarshal(raw, &det) != nil {
			continue
		}
		e.Code = det.GetError()
		if c := det.GetDetails(); c != nil {
			e.Title, e.Detail = strings.TrimSpace(c.GetTitle()), strings.TrimSpace(c.GetDetail())
			e.IsRetryable = c.IsRetryable
			e.ActionRequired = c.GetAnalyticsMetadata().GetActionRequired()
		}
		break
	}
	e.Class = classify(e)
	return e
}

func connectCodeFromStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_argument"
	case http.StatusUnauthorized:
		return "unauthenticated"
	case http.StatusForbidden:
		return "permission_denied"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusTooManyRequests:
		return "resource_exhausted"
	case http.StatusServiceUnavailable:
		return "unavailable"
	default:
		if status >= 500 {
			return "internal"
		}
		return "unknown"
	}
}

// classify follows the CLI's own decision order (research/cursor-protocol.md §5.3).
func classify(e *Error) ErrorClass {
	switch e.ActionRequired {
	case "login":
		return ClassLogin
	case "payment":
		return ClassPayment
	case "upgrade":
		if isRateLimit(e.Code) {
			return ClassUpgradeRate
		}
		return ClassUpgradeUsage
	case "config":
		if e.Code == aiserverv1.ErrorDetails_ERROR_OUTDATED_CLIENT {
			return ClassOutdatedClient
		}
		return ClassLogin
	}

	switch e.Code {
	case aiserverv1.ErrorDetails_ERROR_NOT_LOGGED_IN,
		aiserverv1.ErrorDetails_ERROR_AGENT_REQUIRES_LOGIN,
		aiserverv1.ErrorDetails_ERROR_AUTH_TOKEN_NOT_FOUND,
		aiserverv1.ErrorDetails_ERROR_AUTH_TOKEN_EXPIRED,
		aiserverv1.ErrorDetails_ERROR_INVALID_AUTH_ID,
		aiserverv1.ErrorDetails_ERROR_UNAUTHORIZED,
		aiserverv1.ErrorDetails_ERROR_BAD_API_KEY,
		aiserverv1.ErrorDetails_ERROR_BAD_USER_API_KEY:
		return ClassLogin
	case aiserverv1.ErrorDetails_ERROR_USAGE_PRICING_REQUIRED,
		aiserverv1.ErrorDetails_ERROR_USAGE_PRICING_REQUIRED_CHANGEABLE:
		return ClassPayment
	case aiserverv1.ErrorDetails_ERROR_FREE_USER_USAGE_LIMIT,
		aiserverv1.ErrorDetails_ERROR_PRO_USER_USAGE_LIMIT,
		aiserverv1.ErrorDetails_ERROR_PRO_USER_ONLY:
		return ClassUpgradeUsage
	case aiserverv1.ErrorDetails_ERROR_FREE_USER_RATE_LIMIT_EXCEEDED,
		aiserverv1.ErrorDetails_ERROR_PRO_USER_RATE_LIMIT_EXCEEDED,
		aiserverv1.ErrorDetails_ERROR_RATE_LIMITED,
		aiserverv1.ErrorDetails_ERROR_RATE_LIMITED_CHANGEABLE,
		aiserverv1.ErrorDetails_ERROR_GENERIC_RATE_LIMIT_EXCEEDED,
		aiserverv1.ErrorDetails_ERROR_API_KEY_RATE_LIMIT:
		return ClassUpgradeRate
	case aiserverv1.ErrorDetails_ERROR_OUTDATED_CLIENT:
		return ClassOutdatedClient
	case aiserverv1.ErrorDetails_ERROR_SUSPICIOUS_USAGE_BLOCKED,
		aiserverv1.ErrorDetails_ERROR_MODEL_BLOCKED,
		aiserverv1.ErrorDetails_ERROR_UNSUPPORTED_REGION,
		aiserverv1.ErrorDetails_ERROR_ACCOUNT_CLOSED:
		return ClassBlocked
	}
	return classifyFallback(e)
}

func isRateLimit(c aiserverv1.ErrorDetails_Error) bool {
	switch c {
	case aiserverv1.ErrorDetails_ERROR_FREE_USER_RATE_LIMIT_EXCEEDED,
		aiserverv1.ErrorDetails_ERROR_PRO_USER_RATE_LIMIT_EXCEEDED,
		aiserverv1.ErrorDetails_ERROR_RATE_LIMITED,
		aiserverv1.ErrorDetails_ERROR_RATE_LIMITED_CHANGEABLE,
		aiserverv1.ErrorDetails_ERROR_GENERIC_RATE_LIMIT_EXCEEDED,
		aiserverv1.ErrorDetails_ERROR_API_KEY_RATE_LIMIT:
		return true
	}
	return false
}

// classifyFallback handles errors without a decisive ErrorDetails code.
func classifyFallback(e *Error) ErrorClass {
	switch e.Code {
	case aiserverv1.ErrorDetails_ERROR_CLAUDE_IMAGE_TOO_LARGE,
		aiserverv1.ErrorDetails_ERROR_MAX_TOKENS,
		aiserverv1.ErrorDetails_ERROR_CONVERSATION_TOO_LONG,
		aiserverv1.ErrorDetails_ERROR_DEPRECATED,
		aiserverv1.ErrorDetails_ERROR_MAX_MODE_REQUIRED,
		aiserverv1.ErrorDetails_ERROR_MODEL_NO_LONGER_SUPPORTED,
		aiserverv1.ErrorDetails_ERROR_USER_ABORTED_REQUEST,
		aiserverv1.ErrorDetails_ERROR_DEBOUNCED:
		return ClassFatal
	case aiserverv1.ErrorDetails_ERROR_CUSTOM_MESSAGE:
		if e.IsRetryable == nil || !*e.IsRetryable {
			return ClassFatal
		}
	}
	// A stale client shows up as a bare resource_exhausted asking to update.
	if e.ConnectCode == "resource_exhausted" && strings.Contains(strings.ToLower(e.Title+" "+e.Message), "update") {
		return ClassOutdatedClient
	}
	if e.IsRetryable != nil {
		if *e.IsRetryable {
			return ClassRetryable
		}
		return ClassFatal
	}
	switch e.ConnectCode {
	case "unauthenticated":
		return ClassLogin
	case "invalid_argument", "not_found", "permission_denied", "unimplemented", "failed_precondition",
		"out_of_range", "already_exists", "data_loss":
		return ClassFatal
	}
	return ClassRetryable
}
