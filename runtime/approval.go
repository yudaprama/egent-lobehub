package runtime

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// ApprovalMode controls when human approval is required before tool execution.
// Mirrors LobeHub's UserInterventionConfig.approvalMode.
type ApprovalMode string

const (
	// ApprovalHeadless runs without any human intervention (auto-approve).
	// Equivalent to LobeHub's { approvalMode: 'headless' }.
	ApprovalHeadless ApprovalMode = "headless"

	// ApprovalAlways requires approval for every tool call.
	// Equivalent to LobeHub's { approvalMode: 'always' }.
	ApprovalAlways ApprovalMode = "always"

	// ApprovalOnDemand only requires approval for tools marked as needing it.
	// Equivalent to LobeHub's default behavior.
	ApprovalOnDemand ApprovalMode = "on_demand"
)

// ApprovalRequest is sent to the human reviewer when a tool needs approval.
type ApprovalRequest struct {
	ToolName    string         `json:"toolName"`
	Identifier  string         `json:"identifier"`
	Arguments   string         `json:"arguments"`
	ParsedArgs  map[string]any `json:"parsedArgs,omitempty"`
	Reason      string         `json:"reason,omitempty"`
	OperationID string         `json:"operationId,omitempty"`
}

// ApprovalResponse is the human reviewer's decision.
type ApprovalResponse struct {
	Approved bool   `json:"approved"`
	Reason   string `json:"reason,omitempty"`
}

// ApprovalGate wraps a tool to require human approval before execution.
// When the wrapped tool is called:
//   - On first invocation, emits an Interrupt via Eino's tool.Interrupt()
//   - The runtime surfaces the interrupt to the client as an ApprovalRequest
//   - The client sends back an ApprovalResponse via runner.Resume()
//   - On resume, the tool checks the response and either executes or returns denied
type ApprovalGate struct {
	inner       tool.BaseTool
	identifier  string
	mode        ApprovalMode
	description string
}

// NewApprovalGate wraps a tool with approval logic.
// mode controls when approval is required.
func NewApprovalGate(inner tool.BaseTool, identifier string, mode ApprovalMode) *ApprovalGate {
	if mode == "" {
		mode = ApprovalOnDemand
	}
	return &ApprovalGate{
		inner:      inner,
		identifier: identifier,
		mode:       mode,
	}
}

// WithDescription adds a human-readable description for the approval prompt.
func (g *ApprovalGate) WithDescription(desc string) *ApprovalGate {
	g.description = desc
	return g
}

// Info returns the wrapped tool's info.
func (g *ApprovalGate) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return g.inner.Info(ctx)
}

// InvokableRun executes the approval flow.
// Returns "" with interrupt on first call (when approval needed).
// On resume, executes or denies based on the approval response.
func (g *ApprovalGate) InvokableRun(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
	// Headless mode: always execute, never interrupt
	if g.mode == ApprovalHeadless {
		return g.invokeInner(ctx, argsJSON, opts...)
	}

	// Check if this is a resume after interrupt
	wasInterrupted, _, _ := tool.GetInterruptState[any](ctx)
	if wasInterrupted {
		return g.handleResume(ctx, argsJSON, opts...)
	}

	// Always mode: always interrupt
	if g.mode == ApprovalAlways {
		return g.requestApproval(ctx, argsJSON)
	}

	// On-demand mode: only interrupt if explicitly marked
	// (In a full impl, this would check a per-tool flag from manifest)
	return g.invokeInner(ctx, argsJSON, opts...)
}

// requestApproval emits an interrupt to pause execution and ask the human.
func (g *ApprovalGate) requestApproval(ctx context.Context, argsJSON string) (string, error) {
	req := ApprovalRequest{
		ToolName:   g.toolName(),
		Identifier: g.identifier,
		Arguments:  argsJSON,
		Reason:     g.description,
	}
	if argsJSON != "" && argsJSON != "{}" {
		var parsed map[string]any
		if json.Unmarshal([]byte(argsJSON), &parsed) == nil {
			req.ParsedArgs = parsed
		}
	}
	// Emit interrupt — returns empty string, error is the interrupt signal
	return "", tool.Interrupt(ctx, req)
}

// handleResume processes the approval response after the human decides.
func (g *ApprovalGate) handleResume(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
	isResume, hasData, data := tool.GetResumeContext[ApprovalResponse](ctx)
	if !isResume || !hasData {
		// No resume data — treat as denied
		return fmt.Sprintf("Tool %s execution was not approved (no response).", g.identifier), nil
	}

	if !data.Approved {
		reason := data.Reason
		if reason == "" {
			reason = "no reason given"
		}
		return fmt.Sprintf("Tool %s execution denied by user: %s", g.identifier, reason), nil
	}

	// Approved — execute the actual tool
	return g.invokeInner(ctx, argsJSON, opts...)
}

// invokeInner calls the wrapped tool.
func (g *ApprovalGate) invokeInner(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
	invokable, ok := g.inner.(tool.InvokableTool)
	if !ok {
		return "", fmt.Errorf("wrapped tool %s is not invokable", g.identifier)
	}
	return invokable.InvokableRun(ctx, argsJSON, opts...)
}

// toolName extracts the tool name from the tool info.
func (g *ApprovalGate) toolName() string {
	info, err := g.inner.Info(context.Background())
	if err != nil || info == nil {
		return g.identifier
	}
	return info.Name
}

// IsInterruptEvent checks if an agent event is an approval interrupt.
// Returns the ApprovalRequest if true, nil otherwise.
func IsInterruptEvent(event any) (*ApprovalRequest, bool) {
	type interruptedAction struct {
		Action struct {
			Interrupted *struct {
				Data any `json:"Data"`
			} `json:"Interrupted"`
		} `json:"Action"`
	}

	// Use reflection-free type assertion via JSON roundtrip
	data, err := json.Marshal(event)
	if err != nil {
		return nil, false
	}
	var ia interruptedAction
	if err := json.Unmarshal(data, &ia); err != nil {
		return nil, false
	}
	if ia.Action.Interrupted == nil {
		return nil, false
	}

	// Try to extract ApprovalRequest from Data
	inner, err := json.Marshal(ia.Action.Interrupted.Data)
	if err != nil {
		return nil, false
	}
	var req ApprovalRequest
	if err := json.Unmarshal(inner, &req); err != nil {
		return nil, false
	}
	if req.ToolName == "" {
		return nil, false
	}
	return &req, true
}

// WrapWithApproval wraps a list of tools with approval gates based on mode.
// Tools in the alwaysApprove list get ApprovalAlways; others get the default mode.
func WrapWithApproval(tools []tool.BaseTool, defaultMode ApprovalMode, alwaysApprove map[string]bool) []tool.BaseTool {
	if defaultMode == ApprovalHeadless && len(alwaysApprove) == 0 {
		return tools
	}
	wrapped := make([]tool.BaseTool, len(tools))
	for i, t := range tools {
		info, _ := t.Info(context.Background())
		name := ""
		if info != nil {
			name = info.Name
		}
		mode := defaultMode
		if alwaysApprove[name] {
			mode = ApprovalAlways
		}
		if mode == ApprovalHeadless {
			wrapped[i] = t
			continue
		}
		wrapped[i] = NewApprovalGate(t, name, mode)
	}
	return wrapped
}
