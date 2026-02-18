package tracing

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Enabled {
		t.Error("expected tracing disabled by default")
	}

	if cfg.Exporter != "otlp" {
		t.Errorf("expected exporter 'otlp', got %s", cfg.Exporter)
	}

	if cfg.SampleRate != 1.0 {
		t.Errorf("expected sample rate 1.0, got %f", cfg.SampleRate)
	}

	if cfg.ServiceName != "pgbastion" {
		t.Errorf("expected service name 'pgbastion', got %s", cfg.ServiceName)
	}
}

func TestInit_Disabled(t *testing.T) {
	cfg := Config{
		Enabled: false,
	}

	shutdown, err := Init(context.Background(), cfg, testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Shutdown should work without error.
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown error: %v", err)
	}

	// Tracer should return noop tracer.
	tracer := Tracer()
	if tracer == nil {
		t.Error("expected tracer to not be nil")
	}
}

func TestInit_ExporterNone(t *testing.T) {
	cfg := Config{
		Enabled:  true,
		Exporter: "none",
	}

	shutdown, err := Init(context.Background(), cfg, testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown error: %v", err)
	}
}

func TestInit_UnknownExporter(t *testing.T) {
	cfg := Config{
		Enabled:  true,
		Exporter: "unknown",
	}

	_, err := Init(context.Background(), cfg, testLogger())
	if err == nil {
		t.Error("expected error for unknown exporter")
	}
}

func TestStartSpan(t *testing.T) {
	// Use noop tracer.
	cfg := Config{Enabled: false}
	Init(context.Background(), cfg, testLogger())

	ctx := context.Background()
	ctx, span := StartSpan(ctx, "test-span")
	defer span.End()

	// Should not panic.
	if span == nil {
		t.Error("expected span to not be nil")
	}
}

func TestSpanFromContext(t *testing.T) {
	cfg := Config{Enabled: false}
	Init(context.Background(), cfg, testLogger())

	ctx := context.Background()
	ctx, span := StartSpan(ctx, "test-span")
	defer span.End()

	retrievedSpan := SpanFromContext(ctx)
	if retrievedSpan == nil {
		t.Error("expected span from context")
	}
}

func TestAttributeHelpers(t *testing.T) {
	// Test all attribute helpers.
	tests := []struct {
		name     string
		attr     attribute.KeyValue
		expected string
	}{
		{"component", WithComponent("test"), "component"},
		{"node.name", WithNodeName("node1"), "node.name"},
		{"operation", WithOperation("failover"), "operation"},
		{"backend", WithBackend("10.0.1.1:5432"), "backend"},
		{"directive.id", WithDirectiveID("abc123"), "directive.id"},
		{"directive.type", WithDirectiveType("PROMOTE"), "directive.type"},
		{"target.node", WithTargetNode("node2"), "target.node"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if string(tt.attr.Key) != tt.expected {
				t.Errorf("expected key %s, got %s", tt.expected, tt.attr.Key)
			}
		})
	}
}

func TestRecordError(t *testing.T) {
	cfg := Config{Enabled: false}
	Init(context.Background(), cfg, testLogger())

	ctx := context.Background()
	ctx, span := StartSpan(ctx, "test-span")
	defer span.End()

	// Should not panic.
	RecordError(ctx, context.Canceled)
}

func TestAddEvent(t *testing.T) {
	cfg := Config{Enabled: false}
	Init(context.Background(), cfg, testLogger())

	ctx := context.Background()
	ctx, span := StartSpan(ctx, "test-span")
	defer span.End()

	// Should not panic.
	AddEvent(ctx, "test-event", attribute.String("key", "value"))
}

func TestSetAttributes(t *testing.T) {
	cfg := Config{Enabled: false}
	Init(context.Background(), cfg, testLogger())

	ctx := context.Background()
	ctx, span := StartSpan(ctx, "test-span")
	defer span.End()

	// Should not panic.
	SetAttributes(ctx, attribute.String("key", "value"))
}

func TestSpanNames(t *testing.T) {
	// Verify span name constants are defined.
	names := []string{
		SpanAPIRequest,
		SpanClusterFailover,
		SpanClusterSwitchover,
		SpanDirectiveExecute,
		SpanDirectivePublish,
		SpanHealthCheck,
		SpanRaftApply,
		SpanVIPCampaign,
		SpanVIPAssign,
		SpanVIPRemove,
		SpanProxyConnect,
		SpanProxyRoute,
	}

	for _, name := range names {
		if name == "" {
			t.Error("span name should not be empty")
		}
	}
}

func TestTracer_Nil(t *testing.T) {
	// Reset global tracer.
	oldTracer := globalTracer
	globalTracer = nil
	defer func() { globalTracer = oldTracer }()

	tracer := Tracer()
	if tracer == nil {
		t.Error("Tracer() should return noop tracer when global is nil")
	}
}

func TestSetStatus(t *testing.T) {
	cfg := Config{Enabled: false}
	Init(context.Background(), cfg, testLogger())

	ctx := context.Background()
	ctx, span := StartSpan(ctx, "test-span")
	defer span.End()

	// Should not panic.
	SetStatus(ctx, codes.Ok, "test status")
}

func TestInit_Stdout(t *testing.T) {
	cfg := Config{
		Enabled:     true,
		Exporter:    "stdout",
		ServiceName: "pgbastion-test",
		SampleRate:  1.0,
	}

	shutdown, err := Init(context.Background(), cfg, testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer shutdown(context.Background())

	// Create a span to verify tracing works.
	ctx, span := StartSpan(context.Background(), "test-span")
	span.SetAttributes(attribute.String("test", "value"))
	span.End()

	// Verify context has span.
	if trace.SpanFromContext(ctx) == nil {
		t.Error("expected span in context")
	}
}

func TestSamplerConfig(t *testing.T) {
	tests := []struct {
		name       string
		sampleRate float64
	}{
		{"always", 1.0},
		{"never", 0.0},
		{"half", 0.5},
		{"above_one", 1.5},
		{"negative", -0.5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				Enabled:     true,
				Exporter:    "stdout",
				ServiceName: "pgbastion-test",
				SampleRate:  tt.sampleRate,
			}

			shutdown, err := Init(context.Background(), cfg, testLogger())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			shutdown(context.Background())
		})
	}
}
