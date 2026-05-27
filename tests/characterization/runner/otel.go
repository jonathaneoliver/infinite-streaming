package runner

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// OTel tracing for characterization runs (issue #493). Additive to the
// existing cycle-label PATCH path — same semantic content, different
// transport. Spans land in any standard trace backend (Tempo, Jaeger,
// Honeycomb, Datadog, Grafana Trace View) for cross-cycle aggregates,
// TraceQL queries, and CI integration that the bespoke control_events
// path doesn't offer.
//
// The dashboard's CycleBandsRail still reads from label_changed
// control_events; nothing about the existing display surface changes.
//
// See .claude/standards/characterization-principles.md § 9 and the
// issue body for the full rationale.

// tracerName is the instrumentation library identifier on every span.
// Stable across releases so trace backends can group / filter by it.
const tracerName = "github.com/jonathaneoliver/infinite-streaming/tests/characterization"

// Environment variables that gate exporter selection.
//
//   CHAR_OTEL_ENDPOINT  — OTLP HTTP collector URL (e.g. http://localhost:4318).
//                         Sends spans to the configured backend in real time.
//   CHAR_OTEL_STDOUT    — non-empty enables the stdout exporter (verbose; pollutes
//                         test output — opt-in for debugging only).
//   CHAR_OTEL_DISABLE   — non-empty forces the noop tracer regardless of the above.
//
// With neither endpoint nor stdout set, the tracer is a real recorder
// but with no exporter — spans accumulate in-memory and are dropped at
// shutdown. That's fine when nobody's watching; the cost is bounded
// by the span buffer (~512 by default).
const (
	envOTelEndpoint = "CHAR_OTEL_ENDPOINT"
	envOTelStdout   = "CHAR_OTEL_STDOUT"
	envOTelDisable  = "CHAR_OTEL_DISABLE"
)

// tracingState wraps the configured TracerProvider so we can shut it
// down at the end of the test invocation (flushes any buffered spans
// to the exporter). One per process.
var (
	tracingMu       sync.Mutex
	tracingProvider *sdktrace.TracerProvider
)

// InitTracing wires up OpenTelemetry for the duration of one test
// invocation and returns a shutdown function the caller MUST defer.
//
// runMeta supplies the resource-level attributes that apply to every
// span: service name, test name, run id, platform. These appear once
// per trace in the backend's resource panel, not on each span.
//
// Idempotent — calling twice in one process reuses the existing
// provider (the first config wins). Callers in different tests can
// safely InitTracing independently; the shutdown of the second is a
// no-op until the first also shuts down.
func InitTracing(ctx context.Context, runMeta map[string]string) (shutdown func(context.Context) error, err error) {
	tracingMu.Lock()
	defer tracingMu.Unlock()
	if tracingProvider != nil {
		return func(context.Context) error { return nil }, nil
	}
	if os.Getenv(envOTelDisable) != "" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return func(context.Context) error { return nil }, nil
	}

	exporter, err := buildExporter(ctx)
	if err != nil {
		return nil, fmt.Errorf("otel exporter: %w", err)
	}

	// Resource attributes ride on every span as the trace's "what
	// emitted this" identity. Use semconv keys where defined so trace
	// backends pick them up in their service/host panels.
	attrs := []attribute.KeyValue{
		semconv.ServiceName("characterization"),
		semconv.ServiceVersion("v1"),
	}
	for k, v := range runMeta {
		if k == "" || v == "" {
			continue
		}
		attrs = append(attrs, attribute.String(k, v))
	}
	res, err := resource.New(ctx, resource.WithAttributes(attrs...))
	if err != nil {
		// resource.New only fails when a detector errors — fall back
		// to a minimal resource rather than failing the test.
		res = resource.NewWithAttributes("", attrs...)
	}

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
	}
	if exporter != nil {
		opts = append(opts, sdktrace.WithBatcher(exporter,
			// Tighter batch interval than the SDK default (5s) so a
			// short test run (~10 min) doesn't lose its last batch on
			// shutdown if the OTLP collector is slow to ack.
			sdktrace.WithBatchTimeout(2*time.Second),
		))
	}
	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	tracingProvider = tp

	return func(shutdownCtx context.Context) error {
		tracingMu.Lock()
		defer tracingMu.Unlock()
		if tracingProvider == nil {
			return nil
		}
		// Two-step: ForceFlush drains the batcher, Shutdown closes
		// the exporter. Errors are reported separately — a flush
		// failure shouldn't prevent the shutdown from closing the
		// network connection.
		flushErr := tracingProvider.ForceFlush(shutdownCtx)
		shutErr := tracingProvider.Shutdown(shutdownCtx)
		tracingProvider = nil
		if flushErr != nil {
			return flushErr
		}
		return shutErr
	}, nil
}

// buildExporter picks the exporter based on env vars. Returns nil
// (no exporter, in-memory drop) when nothing is configured.
func buildExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	if ep := os.Getenv(envOTelEndpoint); ep != "" {
		// Strip scheme — otlptracehttp.WithEndpoint expects host:port.
		host := ep
		insecure := false
		switch {
		case len(host) > 7 && host[:7] == "http://":
			host = host[7:]
			insecure = true
		case len(host) > 8 && host[:8] == "https://":
			host = host[8:]
		}
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(host)}
		if insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		return otlptracehttp.New(ctx, opts...)
	}
	if os.Getenv(envOTelStdout) != "" {
		return stdouttrace.New(stdouttrace.WithPrettyPrint())
	}
	return nil, nil
}

// Tracer returns the global characterization tracer. Safe to call
// before InitTracing — returns a noop until the SDK is configured.
// All test code (cycles.go, modes/*) should call this rather than
// otel.Tracer directly so any rename of the instrumentation library
// is a one-line change.
func Tracer() trace.Tracer {
	return otel.Tracer(tracerName)
}
