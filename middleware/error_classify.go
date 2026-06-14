package middleware

import (
	"fmt"
	"regexp"
	"strings"
)

// ToolErrorKind represents the classification of tool execution errors.
// Mirrors LobeHub's error classification for agent runtime recovery strategies.
type ToolErrorKind string

const (
	// ErrorKindReplan indicates the agent should replan with corrected parameters.
	// Used for bad requests, invalid arguments, schema errors, etc.
	ErrorKindReplan ToolErrorKind = "replan"

	// ErrorKindRetry indicates the agent should retry the same operation.
	// Used for transient failures like timeouts, rate limits, network errors.
	ErrorKindRetry ToolErrorKind = "retry"

	// ErrorKindStop indicates the agent should stop execution.
	// Used for auth failures, permission errors, quota exhaustion, etc.
	ErrorKindStop ToolErrorKind = "stop"
)

// ClassifiedToolError represents a tool error with recovery strategy classification.
type ClassifiedToolError struct {
	Code    string
	Kind    ToolErrorKind
	Message string
}

// Error codes that should trigger retry (transient failures)
var retryCodes = map[string]bool{
	"RATE_LIMITED":        true,
	"SERVICE_UNAVAILABLE": true,
	"TOO_MANY_REQUESTS":   true,
}

// Error codes that should trigger replan (fixable by changing parameters)
var replanCodes = map[string]bool{
	"BAD_REQUEST":          true,
	"INVALID_ARGUMENT":     true,
	"MANIFEST_NOT_FOUND":   true,
	"MCP_CONFIG_NOT_FOUND": true,
	"MCP_EXECUTION_ERROR":  true,
}

// Error codes that should trigger stop (permanent failures)
var stopCodes = map[string]bool{
	"FORBIDDEN":               true,
	"INSUFFICIENT_PERMISSIONS": true,
	"NOT_IMPLEMENTED":         true,
	"PERMISSION_DENIED":       true,
	"UNAUTHORIZED":            true,
}

// Keywords in error messages that suggest retry
var retryKeywords = []string{
	"timeout",
	"timed out",
	"too many requests",
	"temporarily unavailable",
	"service unavailable",
	"network",
	"socket hang up",
	"econnreset",
	"econnrefused",
	"enotfound",
}

// Keywords in error messages that suggest replan
var replanKeywords = []string{
	"invalid",
	"malformed",
	"schema",
	"parse",
	"not found",
	"missing required",
	"manifest not found",
	"not implemented",
}

// Keywords in error messages that suggest stop
var stopKeywords = []string{
	"unauthorized",
	"forbidden",
	"permission denied",
	"api key",
	"quota",
	"billing",
	"not configured",
}

// HTTP status code pattern for extracting status from error messages
var statusPattern = regexp.MustCompile(`\b([45]\d{2})\b`)

// toolErrorSignal represents extracted error information for classification.
type toolErrorSignal struct {
	Code    string
	Message string
	Status  int
}

// normalizeCode converts error codes to uppercase with underscores.
func normalizeCode(code string) string {
	if code == "" {
		return ""
	}
	code = strings.TrimSpace(code)
	code = strings.ToUpper(code)
	code = strings.ReplaceAll(code, " ", "_")
	code = strings.ReplaceAll(code, "-", "_")
	return code
}

// tryExtractStatus attempts to extract HTTP status code from error message.
func tryExtractStatus(message string) int {
	matches := statusPattern.FindStringSubmatch(message)
	if len(matches) < 2 {
		return 0
	}
	var status int
	fmt.Sscanf(matches[1], "%d", &status)
	return status
}

// hasAnyKeyword checks if the text contains any of the given keywords.
func hasAnyKeyword(text string, keywords []string) bool {
	lowerText := strings.ToLower(text)
	for _, keyword := range keywords {
		if strings.Contains(lowerText, keyword) {
			return true
		}
	}
	return false
}

// normalizeSignal extracts structured error information from various error types.
func normalizeSignal(err error) toolErrorSignal {
	if err == nil {
		return toolErrorSignal{Message: "unknown error"}
	}

	message := strings.ToLower(err.Error())
	signal := toolErrorSignal{
		Message: message,
		Status:  tryExtractStatus(message),
	}

	// Try to extract code from error types that have a Code() method or Code field
	// This is a best-effort extraction since Go errors don't have a standard structure
	type coder interface {
		Code() string
	}
	if c, ok := err.(coder); ok {
		signal.Code = normalizeCode(c.Code())
	}

	return signal
}

// classifyKind determines the error recovery strategy based on signal attributes.
func classifyKind(signal toolErrorSignal) ToolErrorKind {
	// Check code-based classification first
	if signal.Code != "" {
		if stopCodes[signal.Code] {
			return ErrorKindStop
		}
		if replanCodes[signal.Code] {
			return ErrorKindReplan
		}
		if retryCodes[signal.Code] {
			return ErrorKindRetry
		}
	}

	// Check HTTP status code
	if signal.Status != 0 {
		switch signal.Status {
		case 401, 403:
			return ErrorKindStop
		case 400, 404, 409, 422:
			return ErrorKindReplan
		case 408, 425, 429:
			return ErrorKindRetry
		}
		if signal.Status >= 500 {
			return ErrorKindRetry
		}
	}

	// Check message keywords
	if hasAnyKeyword(signal.Message, stopKeywords) {
		return ErrorKindStop
	}
	if hasAnyKeyword(signal.Message, replanKeywords) {
		return ErrorKindReplan
	}
	if hasAnyKeyword(signal.Message, retryKeywords) {
		return ErrorKindRetry
	}

	// Default to stop for unknown failures
	// Unknown failures may happen after a side effect already succeeded,
	// so only explicitly classified retryable errors should be replayed.
	return ErrorKindStop
}

// ClassifyToolError classifies a tool execution error into a recovery strategy.
// Ported from lobehub/apps/server/src/services/toolExecution/errorClassification.ts
func ClassifyToolError(err error) ClassifiedToolError {
	signal := normalizeSignal(err)
	return ClassifiedToolError{
		Code:    signal.Code,
		Kind:    classifyKind(signal),
		Message: signal.Message,
	}
}
