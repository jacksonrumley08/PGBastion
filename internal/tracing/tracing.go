// Package tracing provides OpenTelemetry distributed tracing support for PGBastion.
package tracing

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

// Config holds tracing configuration.
type Config struct {
	Enabled     bool    `yaml:"enabled"`
	Exporter    string  `yaml:"exporter"`    // "otlp", "stdout", "none"
	Endpoint    string  `yaml:"endpoint"`    // e.g., "localhost:4317" for OTLP
	ServiceName string  `yaml:"service_name"`
	SampleRate  float64 `yaml:"sample_rate"` // 0.0-1.0
	Insecure    bool    `yaml:"insecure"`    // Use insecure connection for OTLP
}

// DefaultConfig returns the default tracing configuration.
func DefaultConfig() Config {
	return Config{
		Enabled:     false,
		Exporter:    "otlp",
		Endpoint:    "localhost:4317",
		ServiceName: "pgbastion",
		SampleRate:  1.0,
		Insecure:    true,
	}
}

// Tracer holds the global tracer instance.
var (
	globalTracer     trace.Tracer
	globalTracerOnce sync.Once
	noopTracer       = trace.NewNoopTracerProvider().Tracer("noop")
)

// Shutdown function to clean up the tracer provider.
var shutdownFunc func(context.Context) error

// Init initializes the OpenTelemetry tracer with the given configuration.
// Returns a shutdown function that should be called when the application exits.
func Init(ctx context.Context, cfg Config, logger *slog.Logger) (func(context.Context) error, error) {
	if !cfg.Enabled {
		logger.Info("tracing disabled")
		globalTracer = noopTracer
		return func(context.Context) error { return nil }, nil
	}

	logger.Info("initializing tracing",
		"exporter", cfg.Exporter,
		"endpoint", cfg.Endpoint,
		"service_name", cfg.ServiceName,
		"sample_rate", cfg.SampleRate,
	)

	// Create resource with service information.
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion("1.0.0"),
			attribute.String("environment", "production"),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("creating resource: %w", err)
	}

	// Create exporter based on configuration.
	var exporter sdktrace.SpanExporter
	switch cfg.Exporter {
	case "otlp":
		opts := []otlptracegrpc.Option{
			otlptracegrpc.WithEndpoint(cfg.Endpoint),
		}
		if cfg.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		exp, err := otlptracegrpc.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("creating OTLP exporter: %w", err)
		}
		exporter = exp

	case "stdout":
		exp, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return nil, fmt.Errorf("creating stdout exporter: %w", err)
		}
		exporter = exp

	case "none", "":
		logger.Info("tracing exporter set to none, using noop tracer")
		globalTracer = noopTracer
		return func(context.Context) error { return nil }, nil

	default:
		return nil, fmt.Errorf("unknown tracing exporter: %s", cfg.Exporter)
	}

	// Create sampler.
	var sampler sdktrace.Sampler
	if cfg.SampleRate >= 1.0 {
		sampler = sdktrace.AlwaysSample()
	} else if cfg.SampleRate <= 0.0 {
		sampler = sdktrace.NeverSample()
	} else {
		sampler = sdktrace.TraceIDRatioBased(cfg.SampleRate)
	}

	// Create tracer provider.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)

	// Set as global provider.
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	globalTracerOnce.Do(func() {
		globalTracer = tp.Tracer(cfg.ServiceName)
	})

	shutdownFunc = tp.Shutdown
	logger.Info("tracing initialized successfully")

	return tp.Shutdown, nil
}

// Tracer returns the global tracer instance.
func Tracer() trace.Tracer {
	if globalTracer == nil {
		return noopTracer
	}
	return globalTracer
}

// StartSpan starts a new span with the given name and options.
func StartSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return Tracer().Start(ctx, name, opts...)
}

// SpanFromContext returns the span from the context.
func SpanFromContext(ctx context.Context) trace.Span {
	return trace.SpanFromContext(ctx)
}

// Common span attribute helpers.

// WithComponent adds a component attribute to a span.
func WithComponent(component string) attribute.KeyValue {
	return attribute.String("component", component)
}

// WithNodeName adds a node name attribute to a span.
func WithNodeName(nodeName string) attribute.KeyValue {
	return attribute.String("node.name", nodeName)
}

// WithOperation adds an operation attribute to a span.
func WithOperation(operation string) attribute.KeyValue {
	return attribute.String("operation", operation)
}

// WithError adds an error attribute to a span.
func WithError(err error) attribute.KeyValue {
	return attribute.String("error", err.Error())
}

// WithBackend adds a backend address attribute to a span.
func WithBackend(backend string) attribute.KeyValue {
	return attribute.String("backend", backend)
}

// WithDirectiveID adds a directive ID attribute to a span.
func WithDirectiveID(id string) attribute.KeyValue {
	return attribute.String("directive.id", id)
}

// WithDirectiveType adds a directive type attribute to a span.
func WithDirectiveType(typ string) attribute.KeyValue {
	return attribute.String("directive.type", typ)
}

// WithTargetNode adds a target node attribute to a span.
func WithTargetNode(node string) attribute.KeyValue {
	return attribute.String("target.node", node)
}

// RecordError records an error on the current span.
func RecordError(ctx context.Context, err error) {
	span := trace.SpanFromContext(ctx)
	if span != nil && span.IsRecording() {
		span.RecordError(err)
	}
}

// SetStatus sets the status of the current span.
func SetStatus(ctx context.Context, code codes.Code, description string) {
	span := trace.SpanFromContext(ctx)
	if span != nil && span.IsRecording() {
		span.SetStatus(code, description)
	}
}

// AddEvent adds an event to the current span.
func AddEvent(ctx context.Context, name string, attrs ...attribute.KeyValue) {
	span := trace.SpanFromContext(ctx)
	if span != nil && span.IsRecording() {
		span.AddEvent(name, trace.WithAttributes(attrs...))
	}
}

// SetAttributes sets attributes on the current span.
func SetAttributes(ctx context.Context, attrs ...attribute.KeyValue) {
	span := trace.SpanFromContext(ctx)
	if span != nil && span.IsRecording() {
		span.SetAttributes(attrs...)
	}
}

// Span names for various operations.
const (
	SpanAPIRequest        = "api.request"
	SpanClusterFailover   = "cluster.failover"
	SpanClusterSwitchover = "cluster.switchover"
	SpanDirectiveExecute  = "cluster.directive.execute"
	SpanDirectivePublish  = "cluster.directive.publish"
	SpanHealthCheck       = "cluster.health_check"
	SpanRaftApply         = "consensus.raft.apply"
	SpanVIPCampaign       = "vip.campaign"
	SpanVIPAssign         = "vip.assign"
	SpanVIPRemove         = "vip.remove"
	SpanProxyConnect      = "proxy.connect"
	SpanProxyRoute        = "proxy.route"
)
