package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// TruncationMiddleware wraps a tool with result truncation.
// Truncates tool output to prevent context overflow when sending back to LLM.
type TruncationMiddleware struct {
	inner       tool.BaseTool
	maxLength   int
	skipArchive bool
}

// NewTruncationMiddleware creates a truncation wrapper for a tool.
func NewTruncationMiddleware(inner tool.BaseTool, maxLength int) *TruncationMiddleware {
	if maxLength <= 0 {
		maxLength = DefaultToolResultMaxLength
	}
	return &TruncationMiddleware{
		inner:     inner,
		maxLength: maxLength,
	}
}

// WithSkipArchive disables truncation for archive-bypass tools.
func (t *TruncationMiddleware) WithSkipArchive(identifier string) *TruncationMiddleware {
	if ArchiveBypassIdentifiers[identifier] {
		t.skipArchive = true
	}
	return t
}

// Info returns the tool info from the wrapped tool.
func (t *TruncationMiddleware) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.inner.Info(ctx)
}

// InvokableRun executes the wrapped tool and truncates the result.
func (t *TruncationMiddleware) InvokableRun(ctx context.Context, argumentsJSON string, opts ...tool.Option) (string, error) {
	if t.skipArchive {
		return t.invoke(ctx, argumentsJSON, opts...)
	}

	invokable, ok := t.inner.(tool.InvokableTool)
	if !ok {
		return "", fmt.Errorf("wrapped tool is not invokable")
	}

	result, err := invokable.InvokableRun(ctx, argumentsJSON, opts...)
	if err != nil {
		return result, err
	}

	original := result
	truncated := TruncateToolResult(result, t.maxLength)
	if truncated != original {
		slog.Debug("tool result truncated",
			"from", len(original),
			"to", len(truncated),
			"limit", t.maxLength,
		)
	}

	return truncated, nil
}

// invoke calls the underlying tool without truncation.
func (t *TruncationMiddleware) invoke(ctx context.Context, argumentsJSON string, opts ...tool.Option) (string, error) {
	invokable, ok := t.inner.(tool.InvokableTool)
	if !ok {
		return "", fmt.Errorf("wrapped tool is not invokable")
	}
	return invokable.InvokableRun(ctx, argumentsJSON, opts...)
}

// IsInvokable checks if the wrapped tool is invokable.
func (t *TruncationMiddleware) IsInvokable() bool {
	_, ok := t.inner.(tool.InvokableTool)
	return ok
}

// IsStreamable checks if the wrapped tool is streamable.
func (t *TruncationMiddleware) IsStreamable() bool {
	_, ok := t.inner.(tool.StreamableTool)
	return ok
}

// ToolExecutionResult is the result of a tool execution with metadata.
type ToolExecutionResult struct {
	Content       string
	Success       bool
	ExecutionTime time.Duration
	Deferred      bool
	Error         error
	Classification *ClassifiedToolError
}

// ClassifiedToolMiddleware wraps a tool with execution timing, error classification,
// and optional truncation. This mirrors LobeHub's ToolExecutionService responsibilities.
type ClassifiedToolMiddleware struct {
	inner         tool.BaseTool
	identifier    string
	maxLength     int
	skipTruncate  bool
	skipArchive   bool
}

// ClassifiedToolConfig holds configuration for the classified tool wrapper.
type ClassifiedToolConfig struct {
	Identifier     string
	MaxLength      int
	SkipTruncate   bool
	SkipArchive    bool
}

// NewClassifiedToolMiddleware creates a new tool wrapper with full execution metadata.
func NewClassifiedToolMiddleware(inner tool.BaseTool, cfg ClassifiedToolConfig) *ClassifiedToolMiddleware {
	if cfg.MaxLength <= 0 {
		cfg.MaxLength = DefaultToolResultMaxLength
	}
	return &ClassifiedToolMiddleware{
		inner:        inner,
		identifier:   cfg.Identifier,
		maxLength:    cfg.MaxLength,
		skipTruncate: cfg.SkipTruncate,
		skipArchive:  cfg.SkipArchive,
	}
}

// Info returns the tool info from the wrapped tool.
func (c *ClassifiedToolMiddleware) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return c.inner.Info(ctx)
}

// InvokableRun executes the wrapped tool with timing and error classification.
func (c *ClassifiedToolMiddleware) InvokableRun(ctx context.Context, argumentsJSON string, opts ...tool.Option) (string, error) {
	invokable, ok := c.inner.(tool.InvokableTool)
	if !ok {
		return "", fmt.Errorf("wrapped tool is not invokable")
	}

	// Skip execution for archive-bypass tools that should not be truncated
	if c.skipArchive && ArchiveBypassIdentifiers[c.identifier] {
		return invokable.InvokableRun(ctx, argumentsJSON, opts...)
	}

	start := time.Now()
	result, err := invokable.InvokableRun(ctx, argumentsJSON, opts...)
	elapsed := time.Since(start)

	if err != nil {
		classified := ClassifyToolError(err)
		slog.Warn("tool error classified",
			"tool", c.identifier,
			"kind", classified.Kind,
			"code", classified.Code,
			"elapsed", elapsed,
			"message", classified.Message,
		)
		// Return a formatted error message that the LLM can understand
		return fmt.Sprintf("Tool execution error (%s): %s", classified.Kind, classified.Message), nil
	}

	// Apply truncation unless disabled
	if !c.skipTruncate {
		original := result
		result = TruncateToolResult(result, c.maxLength)
		if result != original {
			slog.Debug("tool result truncated",
				"tool", c.identifier,
				"from", len(original),
				"to", len(result),
			)
		}
	}

	return result, nil
}

// IsInvokable checks if the wrapped tool is invokable.
func (c *ClassifiedToolMiddleware) IsInvokable() bool {
	_, ok := c.inner.(tool.InvokableTool)
	return ok
}

// IsStreamable checks if the wrapped tool is streamable.
func (c *ClassifiedToolMiddleware) IsStreamable() bool {
	_, ok := c.inner.(tool.StreamableTool)
	return ok
}

// WrapWithMiddleware applies a chain of middleware wrappers to a tool.
// Order: permission -> classification/truncation -> inner tool
func WrapWithMiddleware(inner tool.BaseTool, identifier string, permConfig *PermissionConfig, maxLength int) tool.BaseTool {
	// Layer 1: Apply truncation + error classification
	classified := NewClassifiedToolMiddleware(inner, ClassifiedToolConfig{
		Identifier:    identifier,
		MaxLength:     maxLength,
		SkipArchive:   true,
	})

	// Layer 2: Apply permission gate on top
	if permConfig != nil {
		return NewPermissionGate(classified, identifier, permConfig)
	}

	return classified
}
