package tracing

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Config holds the tracing configuration.
type Config struct {
	// Enabled controls whether tracing is active.
	Enabled bool
	// Endpoint is the OTLP HTTP endpoint (e.g. "localhost:4318").
	Endpoint string
	// ServiceName is the OpenTelemetry service name.
	ServiceName string
	// SampleRate controls the trace sampling rate (0.0–1.0).
	SampleRate float64
}

// Init initializes OpenTelemetry tracing with an OTLP HTTP exporter.
// It returns a shutdown function that should be called on process exit.
// When disabled or misconfigured, it returns a no-op shutdown and logs a warning.
func Init(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	if !cfg.Enabled {
		slog.Info("tracing: disabled")
		return func(context.Context) error { return nil }, nil
	}

	// Build resource
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceNameKey.String(cfg.ServiceName),
			semconv.ServiceVersion(versionFromEnv()),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: build resource: %w", err)
	}

	// Build OTLP HTTP exporter
	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(cfg.Endpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: create exporter: %w", err)
	}

	// Build tracer provider
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(cfg.SampleRate)),
	)

	// Register as global provider
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	slog.Info("tracing: initialized",
		"endpoint", cfg.Endpoint,
		"service", cfg.ServiceName,
		"sample_rate", cfg.SampleRate,
	)

	return tp.Shutdown, nil
}

// Tracer returns a tracer from the global provider with the given name.
func Tracer(name string) *TracerWrapper {
	return &TracerWrapper{tracer: otel.Tracer(name)}
}

// TracerWrapper wraps an OpenTelemetry tracer with convenience methods.
type TracerWrapper struct {
	tracer trace.Tracer
}

// Trace creates a new span with the given name.
func (t *TracerWrapper) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return t.tracer.Start(ctx, name, opts...)
}

// ParseEndpoint reads OTLP_ENDPOINT from env or returns default.
func ParseEndpoint() string {
	if v := os.Getenv("OTLP_ENDPOINT"); v != "" {
		return v
	}
	return "localhost:4318"
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
