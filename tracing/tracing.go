package tracing

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/cloudwego/eino/callbacks"
	"github.com/yudaprama/otelcallback"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// Config holds the tracing/telemetry configuration.
type Config struct {
	// Enabled controls whether tracing/telemetry is active.
	Enabled bool
	// Endpoint is the OTLP gRPC endpoint in host:port form
	// (e.g. "localhost:4317"). planoctl injects this via
	// OTEL_TRACING_GRPC_ENDPOINT.
	Endpoint string
	// ServiceName is the OpenTelemetry service name.
	ServiceName string
	// SampleRate controls the trace sampling rate (0.0–1.0).
	SampleRate float64
}

// Handler is the Eino callback handler produced by Init, once telemetry is
// enabled. It emits an OTel span + metrics for every model/tool/agent call.
// Callers attach it via adk.WithCallbacks(Handler). It is nil while telemetry
// is disabled.
var Handler callbacks.Handler

// Init initializes OpenTelemetry tracing + metrics via the generic
// eino-ext opentelemetry callback (OTLP gRPC → Alloy → Grafana Cloud), and
// registers the TracerProvider/MeterProvider globally so the package-level
// manual spans (see Tracer) share the same exporters.
//
// It returns a shutdown function that should be called on process exit.
// When disabled, it returns a no-op shutdown and leaves Handler nil.
func Init(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	if !cfg.Enabled {
		slog.Info("tracing: disabled")
		Handler = nil
		return func(context.Context) error { return nil }, nil
	}

	h, sd, herr := otelcallback.NewHandler(ctx, &otelcallback.Config{
		ExportEndpoint: cfg.Endpoint,
		ServiceName:    cfg.ServiceName,
		EnableTracing:  true,
		EnableMetrics:  true,
		Insecure:       true,
		SampleRate:     cfg.SampleRate,
		ResourceAttributes: map[string]string{
			"service.version": versionFromEnv(),
		},
	})
	if herr != nil {
		return nil, fmt.Errorf("tracing: init provider: %w", herr)
	}

	Handler = h

	slog.Info("tracing: initialized",
		"endpoint", cfg.Endpoint,
		"service", cfg.ServiceName,
		"sample_rate", cfg.SampleRate,
	)

	return sd, nil
}

// Tracer returns a tracer wrapper for the given component name. The underlying
// otel.Tracer is resolved lazily from the global provider on each Start call,
// so this is safe to assign at package-init time — the global provider is only
// registered once Init runs in main.
func Tracer(name string) *TracerWrapper {
	return &TracerWrapper{name: name}
}

// TracerWrapper wraps an OpenTelemetry tracer name with a lazy Start.
type TracerWrapper struct {
	name string
}

// Start resolves the global tracer and opens a new span with the given name.
func (t *TracerWrapper) Start(ctx context.Context, spanName string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return otel.Tracer(t.name).Start(ctx, spanName, opts...)
}

// ParseEndpoint reads OTEL_TRACING_GRPC_ENDPOINT (injected by planoctl as
// "http://localhost:4317"), strips the URL scheme (the gRPC client wants a
// bare host:port), and defaults to localhost:4317 — Alloy's OTLP gRPC
// receiver.
func ParseEndpoint() string {
	v := os.Getenv("OTEL_TRACING_GRPC_ENDPOINT")
	if v == "" {
		return "localhost:4317"
	}
	v = strings.TrimPrefix(v, "https://")
	v = strings.TrimPrefix(v, "http://")
	return v
}

// ParseSampleRate reads OTLP_SAMPLE_RATE from env or returns default.
func ParseSampleRate() float64 {
	if v := os.Getenv("OTLP_SAMPLE_RATE"); v != "" {
		var rate float64
		if _, err := fmt.Sscanf(v, "%f", &rate); err == nil && rate >= 0 && rate <= 1 {
			return rate
		}
	}
	return 1.0 // default: sample everything
}

func versionFromEnv() string {
	if v := os.Getenv("EGENT_LOBEHUB_VERSION"); v != "" {
		return v
	}
	return "dev"
}

// MustInit initializes tracing or panics. Use for tests.
func MustInit(ctx context.Context, cfg Config) func(context.Context) error {
	shutdown, err := Init(ctx, cfg)
	if err != nil {
		panic(fmt.Sprintf("tracing init failed: %v", err))
	}
	return shutdown
}
