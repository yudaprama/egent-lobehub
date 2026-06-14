package middleware

import (
	"fmt"
	"unicode/utf16"
	"unicode/utf8"
)

// Default maximum length for tool execution result content (in characters).
// This prevents context overflow when sending results back to LLM.
const DefaultToolResultMaxLength = 25000

// ArchiveBypassIdentifiers contains tool identifiers whose results must never
// be truncated or archived, because they are themselves the read surface for
// archived content.
var ArchiveBypassIdentifiers = map[string]bool{
	"lobe-agent-documents": true,
}

// TruncateToolResult truncates tool result content if it exceeds the maximum length.
// Adds a truncation notice to inform the LLM that content was cut off.
// Ported from lobehub/apps/server/src/utils/truncateToolResult.ts
func TruncateToolResult(content string, maxLength int) string {
	if maxLength <= 0 {
		maxLength = DefaultToolResultMaxLength
	}

	if content == "" || len(content) <= maxLength {
		return content
	}

	// Avoid splitting a multi-byte UTF-8 rune when truncating.
	// If the cutoff lands in the middle of a rune, back up to rune boundary.
	// This is the Go equivalent of the TS code's UTF-16 surrogate pair check.
	cutoff := maxLength
	if cutoff > 0 && cutoff <= len(content) {
		for i := cutoff - 1; i >= 0 && i >= cutoff-3; i-- {
			if utf8.RuneStart(content[i]) && i < cutoff {
				_, size := utf8.DecodeRuneInString(content[i:])
				if i+size > cutoff {
					cutoff = i
					break
				}
			}
		}
	}

	// Ensure we don't go negative
	if cutoff < 0 {
		cutoff = 0
	}
	if cutoff > len(content) {
		cutoff = len(content)
	}

	truncated := content[:cutoff]
	remainingChars := len(content) - cutoff

	// Add truncation notice
	notice := fmt.Sprintf("\n\n[Content truncated: %d characters omitted to prevent context overflow. Original length: %d characters]", remainingChars, len(content))

	return truncated + notice
}

// TruncateToolResultWithState truncates the content field of a result object
// while preserving other fields. Generic helper for structured tool results.
func TruncateToolResultWithState[T any](result T, maxLength int, getContent func(T) string, setContent func(T, string) T) T {
	content := getContent(result)
	return setContent(result, TruncateToolResult(content, maxLength))
}

// ToolResultWithContent is a common interface for tool results with content.
type ToolResultWithContent interface {
	GetContent() string
	SetContent(string)
}

// TruncateToolResultGeneric is a helper for tool results that implement ToolResultWithContent.
func TruncateToolResultGeneric[T ToolResultWithContent](result T, maxLength int) T {
	content := result.GetContent()
	result.SetContent(TruncateToolResult(content, maxLength))
	return result
}

// isUTF16HighSurrogate checks if a rune is a UTF-16 high surrogate.
// Used for compatibility with JavaScript/TypeScript string handling.
func isUTF16HighSurrogate(r rune) bool {
	return r >= 0xD800 && r <= 0xDBFF
}

// isUTF16LowSurrogate checks if a rune is a UTF-16 low surrogate.
func isUTF16LowSurrogate(r rune) bool {
	return r >= 0xDC00 && r <= 0xDFFF
}

// utf16Length returns the UTF-16 code unit length of a string.
// This matches JavaScript's string.length behavior.
func utf16Length(s string) int {
	return len(utf16.Encode([]rune(s)))
}

// truncateUTF16 truncates a string to a maximum UTF-16 code unit length.
// This matches JavaScript's string.slice behavior for cross-platform compatibility.
func truncateUTF16(s string, maxUTF16Len int) string {
	runes := []rune(s)
	utf16Codes := utf16.Encode(runes)

	if len(utf16Codes) <= maxUTF16Len {
		return s
	}

	// Truncate at UTF-16 boundary
	truncatedUTF16 := utf16Codes[:maxUTF16Len]
	decoded := utf16.Decode(truncatedUTF16)

	return string(decoded)
}
