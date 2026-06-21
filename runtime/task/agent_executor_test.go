package task

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestMockExecutor_RecordsCalls verifies the mock captures every Run call
// for later assertion. This is the most common assertion pattern in
// workflow tests.
func TestMockExecutor_RecordsCalls(t *testing.T) {
	m := NewMockExecutor(&AgentRunResult{
		OperationID:      "op_1",
		TopicID:          "topic_1",
		ModelUsed:        "gpt-5",
		AssistantContent: "hello",
	})

	result, err := m.Run(context.Background(), AgentRunParams{
		AgentID: "agt_1",
		Prompt:  "test",
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.OperationID != "op_1" {
		t.Errorf("expected OperationID=op_1, got %q", result.OperationID)
	}
	if result.AssistantContent != "hello" {
		t.Errorf("expected AssistantContent=hello, got %q", result.AssistantContent)
	}
	if len(m.RunCalls) != 1 {
		t.Errorf("expected 1 Run call, got %d", len(m.RunCalls))
	}
	if m.RunCalls[0].AgentID != "agt_1" {
		t.Errorf("expected AgentID=agt_1, got %q", m.RunCalls[0].AgentID)
	}
}

// TestMockExecutor_HeartbeatCallbacks verifies the progress callback
// fires the expected number of times when a delay is configured. The
// Temporal activity layer relies on heartbeats to keep the workflow
// alive during long agent runs.
func TestMockExecutor_HeartbeatCallbacks(t *testing.T) {
	m := NewMockExecutor(&AgentRunResult{AssistantContent: "ok"})
	m.Delay = 200 * time.Millisecond

	var count int32
	progress := func(_ any) {
		atomic.AddInt32(&count, 1)
	}

	_, err := m.Run(context.Background(), AgentRunParams{AgentID: "agt_1"}, progress)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The mock ticks every Delay/4 = 50ms for the 200ms duration.
	// Expect at least 1 tick (we don't assert exact count to avoid
	// CI flake). 0 ticks would indicate the loop didn't fire.
	if atomic.LoadInt32(&count) < 1 {
		t.Errorf("expected at least 1 progress callback, got %d", count)
	}
}

// TestMockExecutor_InterruptRecordsOp verifies the Interrupt call is
// recorded for assertion.
func TestMockExecutor_InterruptRecordsOp(t *testing.T) {
	m := NewMockExecutor(&AgentRunResult{AssistantContent: "ok"})

	if err := m.Interrupt(context.Background(), "op_1"); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if len(m.Interrupts) != 1 || m.Interrupts[0] != "op_1" {
		t.Errorf("expected Interrupts=[op_1], got %v", m.Interrupts)
	}
}

// TestMockExecutor_ReturnsError verifies the error path.
func TestMockExecutor_ReturnsError(t *testing.T) {
	m := NewMockExecutor(&AgentRunResult{AssistantContent: "ok"})
	m.Err = errors.New("rate limit")

	_, err := m.Run(context.Background(), AgentRunParams{AgentID: "agt_1"}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "rate limit" {
		t.Errorf("expected error=rate limit, got %q", err.Error())
	}
}

// TestNoopExecutor verifies the default executor used when none is
// configured. Returns empty result, no error.
func TestNoopExecutor(t *testing.T) {
	var e NoopExecutor
	result, err := e.Run(context.Background(), AgentRunParams{AgentID: "agt_1"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.AssistantContent != "" {
		t.Errorf("expected empty AssistantContent, got %q", result.AssistantContent)
	}
	if err := e.Interrupt(context.Background(), "op_1"); err != nil {
		t.Errorf("Interrupt: %v", err)
	}
}

// TestProgressCallback_NoopSafe verifies NoopProgress does not panic and
// can be called from any goroutine.
func TestProgressCallback_NoopSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("NoopProgress panicked: %v", r)
		}
	}()
	NoopProgress("anything")
	NoopProgress(nil)
}
