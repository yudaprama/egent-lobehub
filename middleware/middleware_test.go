package middleware

import (
	"errors"
	"strings"
	"testing"
)

func TestClassifyToolError_ByCode(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantKind ToolErrorKind
	}{
		{
			name:     "rate_limited_retry",
			err:      &codeErr{code: "RATE_LIMITED", msg: "too many requests"},
			wantKind: ErrorKindRetry,
		},
		{
			name:     "bad_request_replan",
			err:      &codeErr{code: "BAD_REQUEST", msg: "invalid argument"},
			wantKind: ErrorKindReplan,
		},
		{
			name:     "forbidden_stop",
			err:      &codeErr{code: "FORBIDDEN", msg: "access denied"},
			wantKind: ErrorKindStop,
		},
		{
			name:     "unrecognized_code_defaults_to_stop",
			err:      &codeErr{code: "UNKNOWN", msg: "something went wrong"},
			wantKind: ErrorKindStop, // Unknown failures default to stop
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyToolError(tt.err)
			if got.Kind != tt.wantKind {
				t.Errorf("ClassifyToolError(%q).Kind = %q, want %q", tt.name, got.Kind, tt.wantKind)
			}
		})
	}
}

func TestClassifyToolError_ByHTTPStatus(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantKind ToolErrorKind
	}{
		{
			name:     "http_401_stop",
			err:      errors.New("API error (401) unauthorized"),
			wantKind: ErrorKindStop,
		},
		{
			name:     "http_403_stop",
			err:      errors.New("403 Forbidden"),
			wantKind: ErrorKindStop,
		},
		{
			name:     "http_400_replan",
			err:      errors.New("HTTP 400 Bad Request"),
			wantKind: ErrorKindReplan,
		},
		{
			name:     "http_404_replan",
			err:      errors.New("GET returned 404"),
			wantKind: ErrorKindReplan,
		},
		{
			name:     "http_429_retry",
			err:      errors.New("429 Too Many Requests"),
			wantKind: ErrorKindRetry,
		},
		{
			name:     "http_500_retry",
			err:      errors.New("HTTP status 500 internal server error"),
			wantKind: ErrorKindRetry,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyToolError(tt.err)
			if got.Kind != tt.wantKind {
				t.Errorf("ClassifyToolError(%q).Kind = %q, want %q", tt.name, got.Kind, tt.wantKind)
			}
		})
	}
}

func TestClassifyToolError_ByKeyword(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantKind ToolErrorKind
	}{
		{
			name:     "timeout_keyword_retry",
			err:      errors.New("connection timeout after 30s"),
			wantKind: ErrorKindRetry,
		},
		{
			name:     "permission_denied_keyword_stop",
			err:      errors.New("permission denied for resource"),
			wantKind: ErrorKindStop,
		},
		{
			name:     "not_found_keyword_replan",
			err:      errors.New("resource not found"),
			wantKind: ErrorKindReplan,
		},
		{
			name:     "api_key_keyword_stop",
			err:      errors.New("invalid API key"),
			wantKind: ErrorKindStop,
		},
		{
			name:     "billing_keyword_stop",
			err:      errors.New("quota exceeded for billing plan"),
			wantKind: ErrorKindStop,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyToolError(tt.err)
			if got.Kind != tt.wantKind {
				t.Errorf("ClassifyToolError(%q).Kind = %q, want %q", tt.name, got.Kind, tt.wantKind)
			}
		})
	}
}

func TestClassifyToolError_NilError_DefaultsToStop(t *testing.T) {
	got := ClassifyToolError(nil)
	if got.Kind != ErrorKindStop {
		t.Errorf("ClassifyToolError(nil).Kind = %q, want %q", got.Kind, ErrorKindStop)
	}
	if got.Code != "" {
		t.Errorf("ClassifyToolError(nil).Code = %q, want empty", got.Code)
	}
}

func TestTruncateToolResult_ShorterThanLimit(t *testing.T) {
	result := TruncateToolResult("short", 100)
	if result != "short" {
		t.Errorf("TruncateToolResult(short, 100) = %q, want %q", result, "short")
	}
}

func TestTruncateToolResult_ExactLimit(t *testing.T) {
	content := "1234567890"
	result := TruncateToolResult(content, 10)
	if result != content {
		t.Errorf("TruncateToolResult(exact, 10) = %q, want %q", result, content)
	}
}

func TestTruncateToolResult_Truncated(t *testing.T) {
	content := "This is a very long tool result that should be truncated"
	result := TruncateToolResult(content, 20)
	
	if !strings.Contains(result, "[Content truncated:") {
		t.Errorf("TruncateToolResult() missing truncation notice: %q", result)
	}
	if !strings.Contains(result, "This is a very long") {
		t.Errorf("TruncateToolResult() should include start of original content: %q", result)
	}
}

func TestTruncateToolResult_EmptyContent(t *testing.T) {
	result := TruncateToolResult("", 100)
	if result != "" {
		t.Errorf("TruncateToolResult(empty) = %q, want empty", result)
	}
}

type codeErr struct {
	code string
	msg  string
}

func (e *codeErr) Code() string  { return e.code }
func (e *codeErr) Error() string { return e.msg }
