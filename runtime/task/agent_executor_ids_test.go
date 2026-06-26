package task

import (
	"strings"
	"testing"
)

func TestNewTopicID_FormatAndUniqueness(t *testing.T) {
	a := newTopicID()
	b := newTopicID()
	if !strings.HasPrefix(a, "tpc_") {
		t.Fatalf("topic id missing tpc_ prefix: %q", a)
	}
	if len(a) != len("tpc_")+12 {
		t.Fatalf("topic id wrong length: %q (len %d)", a, len(a))
	}
	if a == b {
		t.Fatalf("topic ids should be unique, got %q twice", a)
	}
}

func TestNewOperationID_Unique(t *testing.T) {
	a := newOperationID()
	b := newOperationID()
	if a == b {
		t.Fatal("operation ids should be unique")
	}
	if a == "" {
		t.Fatal("operation id must be non-empty")
	}
}

func TestResolveTopicID(t *testing.T) {
	// Continued topic is reused verbatim.
	if got := resolveTopicID("tpc_existing123"); got != "tpc_existing123" {
		t.Fatalf("continue path: want tpc_existing123, got %q", got)
	}
	// Fresh topic is minted with the right shape.
	got := resolveTopicID("")
	if !strings.HasPrefix(got, "tpc_") {
		t.Fatalf("fresh path: want tpc_ prefix, got %q", got)
	}
}
