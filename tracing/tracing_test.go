package tracing

import (
	"context"
	"testing"
)

// TestInitDisabled verifies that tracing init returns a no-op shutdown
// when disabled, without error.
func TestInitDisabled(t *testing.T) {
	shutdown, err := Init(context.Background(), Config{
		Enabled: false,
	})
	if err != nil {
		t.Fatalf("Init with disabled should not error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown function should not be nil")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("disabled shutdown should be no-op: %v", err)
	}
}

// TestParseEndpointDefault verifies the default endpoint.
func TestParseEndpointDefault(t *testing.T) {
	// Unset env var to test default
	t.Setenv("OTLP_ENDPOINT", "")
	got := ParseEndpoint()
	if got != "localhost:4318" {
		t.Errorf("ParseEndpoint() = %q, want %q", got, "localhost:4318")
	}
}

// TestParseEndpointOverride verifies env var override.
func TestParseEndpointOverride(t *testing.T) {
	t.Setenv("OTLP_ENDPOINT", "otel-collector:4318")
	got := ParseEndpoint()
	if got != "otel-collector:4318" {
		t.Errorf("ParseEndpoint() = %q, want %q", got, "otel-collector:4318")
	}
}

// TestParseSampleRateDefault verifies the default sample rate.
func TestParseSampleRateDefault(t *testing.T) {
	t.Setenv("OTLP_SAMPLE_RATE", "")
	got := ParseSampleRate()
	if got != 1.0 {
		t.Errorf("ParseSampleRate() = %f, want 1.0", got)
	}
}

// TestParseSampleRateOverride verifies env var override.
func TestParseSampleRateOverride(t *testing.T) {
	t.Setenv("OTLP_SAMPLE_RATE", "0.5")
	got := ParseSampleRate()
	if got != 0.5 {
		t.Errorf("ParseSampleRate() = %f, want 0.5", got)
	}
}

// TestParseSampleRateInvalid verifies fallback on invalid input.
func TestParseSampleRateInvalid(t *testing.T) {
	t.Setenv("OTLP_SAMPLE_RATE", "not-a-number")
	got := ParseSampleRate()
	if got != 1.0 {
		t.Errorf("ParseSampleRate() with invalid input = %f, want 1.0 (default)", got)
	}
}

// TestParseSampleRateOutOfRange verifies fallback on out-of-range values.
func TestParseSampleRateOutOfRange(t *testing.T) {
	t.Setenv("OTLP_SAMPLE_RATE", "1.5")
	got := ParseSampleRate()
	if got != 1.0 {
		t.Errorf("ParseSampleRate() with >1 = %f, want 1.0 (default)", got)
	}
}

// TestTracerReturnsNonNil verifies Tracer() returns a usable wrapper.
func TestTracerReturnsNonNil(t *testing.T) {
	tr := Tracer("test")
	if tr == nil {
		t.Fatal("Tracer() returned nil")
	}

	// Start a span to verify the tracer is functional
	ctx, span := tr.Start(context.Background(), "test-span")
	if span == nil {
		t.Fatal("Start() returned nil span")
	}
	if ctx == nil {
		t.Fatal("Start() returned nil context")
	}
	span.End()
}

// TestMustInitDisabled verifies MustInit works with disabled config.
func TestMustInitDisabled(t *testing.T) {
	shutdown := MustInit(context.Background(), Config{Enabled: false})
	if shutdown == nil {
		t.Fatal("shutdown should not be nil")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("disabled shutdown should be no-op: %v", err)
	}
}
