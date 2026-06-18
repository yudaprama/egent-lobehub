package composio

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// APIError represents a non-2xx response from the Composio API. The Code
// field is the best-effort stable machine code from the response body
// (e.g. "AUTH_ERROR", "validation_error") or a synthetic one derived from
// the HTTP status when the body has no code.
//
// Callers should branch on IsAuthError for the agent-runtime "stop"
// recovery strategy — see egent-lobehub/middleware.ClassifyError.
type APIError struct {
	// StatusCode is the HTTP status (e.g. 401, 403, 422, 500).
	StatusCode int `json:"status_code"`
	// Code is the machine-readable error code from the body, or HTTP_<status>.
	Code string `json:"code"`
	// Message is the human-readable message from the body.
	Message string `json:"message"`
	// Body is the raw response body (truncated to 1KB) for log forwarding.
	Body string `json:"-"`
}

// Error implements the error interface.
func (e *APIError) Error() string {
	if e.Code != "" && e.Code != fmt.Sprintf("HTTP_%d", e.StatusCode) {
		return fmt.Sprintf("composio: %s (status %d): %s", e.Code, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("composio: status %d: %s", e.StatusCode, e.Message)
}

// IsAuthError reports whether the error is a 401/403 (token invalid or
// revoked). The agent runtime should map this to ErrorKindStop via
// egent-lobehub/middleware.ClassifyError so the loop does not retry.
//
// LobeHub's lambda/composio.ts:148-178 maps the same condition to a
// synthetic error: "AUTH_ERROR" + status: "FAILED"; we expose the same
// branch on the Go side so the agent runtime and the UI agree.
func (e *APIError) IsAuthError() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

// IsRetryable reports whether the error is a transient 5xx or 429 that the
// caller may retry with backoff. Used by egent-lobehub/middleware to pick
// ErrorKindRetry over ErrorKindStop.
func (e *APIError) IsRetryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// parseAPIError turns a non-2xx body into a structured *APIError.
//
// Composio returns errors as either:
//
//	{"message": "..."}
//
// or
//
//	{"code": "...", "message": "...", "error": "..."}
//
// depending on the endpoint. We handle both shapes plus a 4xx/5xx status
// fallback so the caller always gets a structured error even when the body
// is HTML (e.g. a Cloudflare interstitial) or empty.
func parseAPIError(status int, body []byte) *APIError {
	const maxBody = 1024
	e := &APIError{
		StatusCode: status,
		Code:       fmt.Sprintf("HTTP_%d", status),
	}
	s := string(body)
	e.Body = s
	if len(s) > maxBody {
		e.Body = s[:maxBody]
	}
	var generic struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
	}
	if err := json.Unmarshal(body, &generic); err == nil {
		switch {
		case generic.Message != "":
			e.Message = generic.Message
		case generic.Error != "":
			e.Message = generic.Error
		}
		switch {
		case generic.Code != "":
			e.Code = generic.Code
		case generic.ErrorCode != "":
			e.Code = generic.ErrorCode
		case status == http.StatusUnauthorized:
			e.Code = "AUTH_ERROR"
		}
	}
	if e.Message == "" {
		e.Message = http.StatusText(status)
	}
	return e
}
