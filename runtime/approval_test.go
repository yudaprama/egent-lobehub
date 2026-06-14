package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

func TestApprovalMode_Constants(t *testing.T) {
	if ApprovalHeadless == "" {
		t.Error("ApprovalHeadless should be defined")
	}
	if ApprovalAlways == "" {
		t.Error("ApprovalAlways should be defined")
	}
	if ApprovalOnDemand == "" {
		t.Error("ApprovalOnDemand should be defined")
	}
}

func TestNewApprovalGate_DefaultsToOnDemand(t *testing.T) {
	inner := newStub("t")
	gate := NewApprovalGate(inner, "t", "")
	if gate.mode != ApprovalOnDemand {
		t.Errorf("expected default mode to be ApprovalOnDemand, got %q", gate.mode)
	}
}

func TestApprovalGate_Info_DelegatesToInner(t *testing.T) {
	inner := newStub("tool_x")
	gate := NewApprovalGate(inner, "tool_x", ApprovalOnDemand)
	info, err := gate.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Name != "tool_x" {
		t.Errorf("expected name=tool_x, got %q", info.Name)
	}
}

func TestApprovalGate_HeadlessMode_BypassesApproval(t *testing.T) {
	// In headless mode, the gate should call the inner tool directly.
	// We can't easily test tool.Interrupt without a runner, but we can
	// verify that headless mode does NOT call the interrupt path.
	inner := newStub("t")
	gate := NewApprovalGate(inner, "t", ApprovalHeadless)

	// Without an Eino runner, tool.Interrupt would panic. But the gate
	// should call the inner tool directly in headless mode.
	// Note: this test runs the gate without a runner context, so we expect
	// it to call the inner tool. If tool.Interrupt fires, that's a failure.
	got, err := gate.InvokableRun(context.Background(), "{}")
	if err != nil {
		// We may get an error from the stub, but it shouldn't be a panic
		t.Logf("got error (acceptable in unit test): %v", err)
	}
	if got == "" && err == nil {
		t.Error("expected non-empty result from stub tool")
	}
}

func TestApprovalGate_WithDescription(t *testing.T) {
	gate := NewApprovalGate(newStub("t"), "t", ApprovalOnDemand).WithDescription("needs review")
	if gate.description != "needs review" {
		t.Errorf("expected description to be set, got %q", gate.description)
	}
}

func TestApprovalRequest_JSONSerialization(t *testing.T) {
	req := ApprovalRequest{
		ToolName:   "delete_file",
		Identifier: "fs.delete",
		Arguments:  `{"path":"/tmp/x"}`,
		Reason:     "deletes a file",
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Check fields are present
	str := string(data)
	for _, want := range []string{`"toolName":"delete_file"`, `"identifier":"fs.delete"`, `"reason":"deletes a file"`} {
		if !strings.Contains(str, want) {
			t.Errorf("expected %q in marshaled output, got: %s", want, str)
		}
	}
}

func TestApprovalResponse_JSONSerialization(t *testing.T) {
	resp := ApprovalResponse{Approved: true, Reason: "looks safe"}
	data, _ := json.Marshal(resp)
	str := string(data)
	if !strings.Contains(str, `"approved":true`) {
		t.Errorf("expected approved=true, got: %s", str)
	}
	if !strings.Contains(str, `"looks safe"`) {
		t.Errorf("expected reason, got: %s", str)
	}
}

func TestIsInterruptEvent_DetectsApprovalRequest(t *testing.T) {
	// Simulate an AgentEvent with Action.Interrupted
	mockEvent := struct {
		Action *struct {
			Interrupted *struct {
				Data ApprovalRequest `json:"Data"`
			} `json:"Interrupted"`
		} `json:"Action"`
	}{
		Action: &struct {
			Interrupted *struct {
				Data ApprovalRequest `json:"Data"`
			} `json:"Interrupted"`
		}{
			Interrupted: &struct {
				Data ApprovalRequest `json:"Data"`
			}{
				Data: ApprovalRequest{
					ToolName:   "send_email",
					Identifier: "gmail.send",
					Reason:     "sends an email",
				},
			},
		},
	}
	req, ok := IsInterruptEvent(mockEvent)
	if !ok {
		t.Fatal("expected to detect interrupt event")
	}
	if req.ToolName != "send_email" {
		t.Errorf("expected toolName=send_email, got %q", req.ToolName)
	}
	if req.Identifier != "gmail.send" {
		t.Errorf("expected identifier=gmail.send, got %q", req.Identifier)
	}
}

func TestIsInterruptEvent_NotInterrupt(t *testing.T) {
	mockEvent := struct {
		Action *struct {
			Interrupted *struct {
				Data string `json:"Data"`
			} `json:"Interrupted"`
		} `json:"Action"`
	}{
		Action: nil,
	}
	_, ok := IsInterruptEvent(mockEvent)
	if ok {
		t.Error("expected to NOT detect interrupt event")
	}
}

func TestIsInterruptEvent_WrongDataType(t *testing.T) {
	// Action.Interrupted exists but Data isn't an ApprovalRequest
	mockEvent := struct {
		Action *struct {
			Interrupted *struct {
				Data string `json:"Data"`
			} `json:"Interrupted"`
		} `json:"Action"`
	}{
		Action: &struct {
			Interrupted *struct {
				Data string `json:"Data"`
			} `json:"Interrupted"`
		}{
			Interrupted: &struct {
				Data string `json:"Data"`
			}{
				Data: "just a string",
			},
		},
	}
	_, ok := IsInterruptEvent(mockEvent)
	if ok {
		t.Error("expected to NOT detect interrupt (wrong data type)")
	}
}

func TestWrapWithApproval_HeadlessReturnsUnchanged(t *testing.T) {
	tools := []tool.BaseTool{newStub("a"), newStub("b")}
	alwaysApprove := map[string]bool{"a": true}
	wrapped := WrapWithApproval(tools, ApprovalHeadless, alwaysApprove)
	if len(wrapped) != len(tools) {
		t.Errorf("expected unchanged count, got %d", len(wrapped))
	}
}

func TestWrapWithApproval_OnDemandWrapsAll(t *testing.T) {
	tools := []tool.BaseTool{newStub("a"), newStub("b")}
	wrapped := WrapWithApproval(tools, ApprovalOnDemand, nil)
	if len(wrapped) != 2 {
		t.Fatalf("expected 2 wrapped tools, got %d", len(wrapped))
	}
	for i, w := range wrapped {
		_, ok := w.(*ApprovalGate)
		if !ok {
			t.Errorf("tool %d not wrapped as ApprovalGate: %T", i, w)
		}
	}
}

func TestWrapWithApproval_AlwaysModeWrapsAll(t *testing.T) {
	tools := []tool.BaseTool{newStub("a"), newStub("b")}
	wrapped := WrapWithApproval(tools, ApprovalAlways, nil)
	if len(wrapped) != 2 {
		t.Fatalf("expected 2 wrapped, got %d", len(wrapped))
	}
}

func TestApprovalGate_HeadlessExecutesInner(t *testing.T) {
	// Verify headless gate actually executes the inner tool
	inner := &countingTool{count: 0}
	gate := NewApprovalGate(inner, "counter", ApprovalHeadless)
	_, _ = gate.InvokableRun(context.Background(), "{}")
	if inner.count != 1 {
		t.Errorf("expected inner to be called once, got %d", inner.count)
	}
}

type countingTool struct {
	count int
}

func (c *countingTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "counter", Desc: "counts"}, nil
}

func (c *countingTool) InvokableRun(_ context.Context, _ string, _ ...tool.Option) (string, error) {
	c.count++
	return "ok", nil
}

func TestApprovalRequest_ParsedArgs(t *testing.T) {
	req := ApprovalRequest{
		ToolName:   "search",
		Arguments:  `{"query":"hello","limit":5}`,
		ParsedArgs: map[string]any{"query": "hello", "limit": float64(5)},
	}
	data, _ := json.Marshal(req)
	str := string(data)
	if !strings.Contains(str, `"parsedArgs"`) {
		t.Error("expected parsedArgs in output")
	}
}

// Quick sanity: ensure time.Duration usage compiles (no unused imports).
var _ = time.Second
