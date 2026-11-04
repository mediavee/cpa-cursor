package cursor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/mediavee/cpa-cursor/internal/gen/agent/v1"
	aiserverv1 "github.com/mediavee/cpa-cursor/internal/gen/aiserver/v1"
)

// endStreamError builds the JSON of an end-of-stream frame the way Cursor does:
// an ErrorDetails message, base64 without padding, in the Connect details.
func endStreamError(t *testing.T, connectCode, message string, det *aiserverv1.ErrorDetails) string {
	t.Helper()
	body := map[string]any{"code": connectCode, "message": message}
	if det != nil {
		raw, err := proto.Marshal(det)
		if err != nil {
			t.Fatal(err)
		}
		body["details"] = []map[string]any{{
			"type":  "aiserver.v1.ErrorDetails",
			"value": base64.RawStdEncoding.EncodeToString(raw),
			"debug": map[string]any{"error": "ERROR_X"},
		}}
	}
	js, err := json.Marshal(map[string]any{"error": body})
	if err != nil {
		t.Fatal(err)
	}
	return string(js)
}

func details(code aiserverv1.ErrorDetails_Error, title, detail, action string, retryable *bool) *aiserverv1.ErrorDetails {
	c := &aiserverv1.CustomErrorDetails{Title: title, Detail: detail, IsRetryable: retryable}
	if action != "" {
		c.AnalyticsMetadata = &aiserverv1.ErrorAnalyticsMetadata{ActionRequired: proto.String(action)}
	}
	return &aiserverv1.ErrorDetails{Error: code, Details: c}
}

func TestErrorClassification(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name        string
		connectCode string
		det         *aiserverv1.ErrorDetails
		want        ErrorClass
	}{
		// research/cursor-protocol.md §5.3
		{"usage limit with action", "resource_exhausted", details(aiserverv1.ErrorDetails_ERROR_PRO_USER_USAGE_LIMIT, "Usage limit", "Resets later", "upgrade", &no), ClassUpgradeUsage},
		{"usage limit by code only", "resource_exhausted", details(aiserverv1.ErrorDetails_ERROR_PRO_USER_USAGE_LIMIT, "", "", "", nil), ClassUpgradeUsage},
		{"rate limit with action", "resource_exhausted", details(aiserverv1.ErrorDetails_ERROR_RATE_LIMITED, "Slow down", "", "upgrade", &yes), ClassUpgradeRate},
		{"generic rate limit", "resource_exhausted", details(aiserverv1.ErrorDetails_ERROR_GENERIC_RATE_LIMIT_EXCEEDED, "", "", "", nil), ClassUpgradeRate},
		{"login by action", "unauthenticated", details(aiserverv1.ErrorDetails_ERROR_UNSPECIFIED, "Sign in", "", "login", nil), ClassLogin},
		{"token expired", "unauthenticated", details(aiserverv1.ErrorDetails_ERROR_AUTH_TOKEN_EXPIRED, "", "", "", nil), ClassLogin},
		{"bare unauthenticated", "unauthenticated", nil, ClassLogin},
		{"payment", "resource_exhausted", details(aiserverv1.ErrorDetails_ERROR_USAGE_PRICING_REQUIRED, "Enable usage pricing", "", "payment", nil), ClassPayment},
		{"bad api key is a config problem", "unauthenticated", details(aiserverv1.ErrorDetails_ERROR_BAD_USER_API_KEY, "", "", "config", nil), ClassLogin},
		{"outdated client by action", "failed_precondition", details(aiserverv1.ErrorDetails_ERROR_OUTDATED_CLIENT, "Update", "", "config", nil), ClassOutdatedClient},
		{"outdated client by code", "failed_precondition", details(aiserverv1.ErrorDetails_ERROR_OUTDATED_CLIENT, "", "", "", nil), ClassOutdatedClient},
		{"stale version shows as resource_exhausted", "resource_exhausted", details(aiserverv1.ErrorDetails_ERROR_UNSPECIFIED, "Please update to the latest version of Cursor", "", "", nil), ClassOutdatedClient},
		{"suspicious usage", "permission_denied", details(aiserverv1.ErrorDetails_ERROR_SUSPICIOUS_USAGE_BLOCKED, "", "", "", nil), ClassBlocked},
		{"model blocked", "permission_denied", details(aiserverv1.ErrorDetails_ERROR_MODEL_BLOCKED, "", "", "", nil), ClassBlocked},
		{"unsupported region", "permission_denied", details(aiserverv1.ErrorDetails_ERROR_UNSUPPORTED_REGION, "", "", "", nil), ClassBlocked},
		{"account closed", "permission_denied", details(aiserverv1.ErrorDetails_ERROR_ACCOUNT_CLOSED, "", "", "", nil), ClassBlocked},
		{"conversation too long", "invalid_argument", details(aiserverv1.ErrorDetails_ERROR_CONVERSATION_TOO_LONG, "", "", "", &yes), ClassFatal},
		{"max mode required", "failed_precondition", details(aiserverv1.ErrorDetails_ERROR_MAX_MODE_REQUIRED, "", "", "", nil), ClassFatal},
		{"custom message not retryable", "unknown", details(aiserverv1.ErrorDetails_ERROR_CUSTOM_MESSAGE, "Nope", "", "", nil), ClassFatal},
		{"explicitly retryable", "unavailable", details(aiserverv1.ErrorDetails_ERROR_PROVIDER_ERROR, "Provider down", "", "", &yes), ClassRetryable},
		{"explicitly not retryable", "internal", details(aiserverv1.ErrorDetails_ERROR_PROVIDER_ERROR, "Bad", "", "", &no), ClassFatal},
		{"unspecified detail defaults to retryable", "internal", details(aiserverv1.ErrorDetails_ERROR_UNSPECIFIED, "", "", "", nil), ClassRetryable},
		{"bare unavailable", "unavailable", nil, ClassRetryable},
		{"bare invalid_argument", "invalid_argument", nil, ClassFatal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := parseEndStream([]byte(endStreamError(t, tc.connectCode, "message", tc.det)))
			var ce *Error
			if !errors.As(err, &ce) {
				t.Fatalf("err = %v, want *Error", err)
			}
			if ce.Class != tc.want {
				t.Errorf("class = %q, want %q (%v)", ce.Class, tc.want, ce)
			}
			if ClassOf(err) != tc.want {
				t.Errorf("ClassOf = %q", ClassOf(err))
			}
		})
	}
}

func TestErrorCarriesCursorDetails(t *testing.T) {
	no := false
	err := parseEndStream([]byte(endStreamError(t, "resource_exhausted", "raw message",
		details(aiserverv1.ErrorDetails_ERROR_PRO_USER_USAGE_LIMIT, " Usage limit ", " Resets Dec 1 ", "upgrade", &no))))
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatal(err)
	}
	if ce.Code != aiserverv1.ErrorDetails_ERROR_PRO_USER_USAGE_LIMIT || ce.Title != "Usage limit" || ce.Detail != "Resets Dec 1" ||
		ce.IsRetryable == nil || *ce.IsRetryable || ce.ActionRequired != "upgrade" || ce.ConnectCode != "resource_exhausted" || ce.Message != "raw message" {
		t.Errorf("error = %+v", ce)
	}
	want := "cursor: ERROR_PRO_USER_USAGE_LIMIT: Usage limit: Resets Dec 1"
	if ce.Error() != want {
		t.Errorf("Error() = %q, want %q", ce.Error(), want)
	}
}

func TestEndStreamWithoutErrorIsClean(t *testing.T) {
	for _, js := range []string{`{}`, `{"metadata":{"x":["y"]}}`} {
		if err := parseEndStream([]byte(js)); err != nil {
			t.Errorf("%s: %v", js, err)
		}
	}
	if err := parseEndStream([]byte(`not json`)); err == nil {
		t.Error("malformed end-of-stream frame accepted")
	}
}

func TestUndecodableDetailsFallBackToConnectCode(t *testing.T) {
	js := `{"error":{"code":"resource_exhausted","message":"slow","details":[{"type":"aiserver.v1.ErrorDetails","value":"!!!"}]}}`
	var ce *Error
	if err := parseEndStream([]byte(js)); !errors.As(err, &ce) || ce.Code != aiserverv1.ErrorDetails_ERROR_UNSPECIFIED || ce.Message != "slow" {
		t.Fatalf("err = %v", err)
	}
}

func TestHTTPErrorWithoutConnectBody(t *testing.T) {
	var ce *Error
	err := parseHTTPError(http.StatusUnauthorized, []byte("nope"))
	if !errors.As(err, &ce) || ce.Class != ClassLogin || ce.HTTPStatus != 401 || ce.ConnectCode != "unauthenticated" {
		t.Fatalf("err = %+v", ce)
	}
	err = parseHTTPError(http.StatusBadGateway, nil)
	if !errors.As(err, &ce) || ce.Class != ClassRetryable {
		t.Fatalf("err = %+v", ce)
	}
}

func TestClassOfTransportAndCancellation(t *testing.T) {
	if ClassOf(context.Canceled) != "" || ClassOf(ErrClosed) != "" {
		t.Error("cancellation must have no class")
	}
	if ClassOf(fmt.Errorf("wrap: %w", &Error{Class: ClassBlocked})) != ClassBlocked {
		t.Error("ClassOf does not unwrap")
	}
}

func TestErrorOnTheWireIsDecodedByTheSession(t *testing.T) {
	up := newFakeUpstream(t)
	s, fs := startSession(t, up, SystemClock{}, RunParams{Request: &agentv1.AgentRunRequest{}})
	fs.recv()
	no := false
	fs.end(endStreamError(t, "resource_exhausted", "limit",
		details(aiserverv1.ErrorDetails_ERROR_PRO_USER_USAGE_LIMIT, "Usage limit", "Resets later", "upgrade", &no)))
	var ce *Error
	if err := nextErr(t, s); !errors.As(err, &ce) || ce.Class != ClassUpgradeUsage {
		t.Fatalf("err = %v", err)
	}
}

func TestHTTPFailureOnOpenIsDecoded(t *testing.T) {
	up := newFakeUpstream(t)
	up.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthenticated","message":"bad token"}`))
	})
	c := up.client(t, SystemClock{})
	s, err := c.Run(context.Background(), "stale", RunParams{Request: runRequest("hi")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var ce *Error
	if err := nextErr(t, s); !errors.As(err, &ce) || ce.Class != ClassLogin || ce.HTTPStatus != 401 {
		t.Fatalf("err = %v", err)
	}
}
