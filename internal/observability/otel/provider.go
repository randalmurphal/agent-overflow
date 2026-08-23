package otel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Config captures the user-tunable fields that drive provider construction.
type Config struct {
	// Enabled toggles whether the real OTLP exporters are wired up. When
	// false the provider returns no-op tracer/meter implementations and
	// NewProvider never performs network I/O.
	Enabled bool

	// Endpoint is the OTLP gRPC endpoint (host:port). Ignored when Enabled
	// is false. If blank while enabled, NewProvider falls back to the OTel
	// environment default (localhost:4317) so the user still gets a
	// meaningful error when the collector isn't listening.
	Endpoint string

	// ServiceName is reported as service.name on all spans/metrics. Kept
	// configurable mostly so tests can set a short name, but production
	// callers should use the constant DefaultServiceName.
	ServiceName string
}

// DefaultServiceName is attached to every span/metric when Config.ServiceName
// is left blank.
const DefaultServiceName = "agent-overflow"

// instrumentationName is the library name used for otel.Tracer and Meter
// lookups. Keeping it as a constant lets tests assert provenance without
// depending on package path tricks.
const instrumentationName = "agent-overflow/observability"

// Metrics groups the instruments the app records. Kept on Provider so callers
// can access them without a global lookup.
type Metrics struct {
	TurnsStarted        metric.Int64Counter
	TurnsCompleted      metric.Int64Counter
	TurnsErrored        metric.Int64Counter
	ItemsPersisted      metric.Int64Counter
	PayloadsPersisted   metric.Int64Counter
	ProviderFrames      metric.Int64Histogram
	ReplayEventsQueued  metric.Int64Counter
	ReplayEventsDropped metric.Int64Counter
}

// Provider owns the tracer + meter providers and a small set of pre-built
// instruments. Call Shutdown to release resources and flush the exporter.
type Provider struct {
	tracerProvider trace.TracerProvider
	meterProvider  metric.MeterProvider
	tracer         trace.Tracer
	meter          metric.Meter
	metrics        Metrics
	enabled        bool
	endpoint       string
	shutdownFns    []func(context.Context) error
}

// NewProvider constructs a Provider from Config. When Config.Enabled is false
// the returned Provider is a no-op and its Shutdown method is a no-op too.
func NewProvider(ctx context.Context, cfg Config) (*Provider, error) {
	if !cfg.Enabled {
		return newNoopProvider()
	}
	return newOTLPProvider(ctx, cfg)
}

func newNoopProvider() (*Provider, error) {
	tp := tracenoop.NewTracerProvider()
	mp := metricnoop.NewMeterProvider()
	p := &Provider{
		tracerProvider: tp,
		meterProvider:  mp,
		tracer:         tp.Tracer(instrumentationName),
		meter:          mp.Meter(instrumentationName),
		enabled:        false,
	}
	if err := p.buildMetrics(); err != nil {
		return nil, fmt.Errorf("otel: build no-op metrics: %w", err)
	}
	return p, nil
}

func newOTLPProvider(ctx context.Context, cfg Config) (*Provider, error) {
	serviceName := cfg.ServiceName
	if serviceName == "" {
		serviceName = DefaultServiceName
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			attribute.String("service.name", serviceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("otel: build resource: %w", err)
	}

	traceExporter, tracerProvider, traceShutdown, err := buildTraceProvider(ctx, cfg.Endpoint, res)
	if err != nil {
		return nil, err
	}

	meterProvider, meterShutdown, err := buildMeterProvider(ctx, cfg.Endpoint, res)
	if err != nil {
		// Clean up the trace exporter we already created before bubbling up.
		_ = traceExporter.Shutdown(context.Background())
		return nil, err
	}

	p := &Provider{
		tracerProvider: tracerProvider,
		meterProvider:  meterProvider,
		tracer:         tracerProvider.Tracer(instrumentationName),
		meter:          meterProvider.Meter(instrumentationName),
		enabled:        true,
		endpoint:       cfg.Endpoint,
		shutdownFns:    []func(context.Context) error{traceShutdown, meterShutdown},
	}
	if err := p.buildMetrics(); err != nil {
		// We built exporters but failed instruments — shut the exporters
		// down before returning so we don't leak goroutines.
		shutCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		_ = p.Shutdown(shutCtx)
		return nil, fmt.Errorf("otel: build metrics: %w", err)
	}
	return p, nil
}

func buildTraceProvider(ctx context.Context, endpoint string, res *resource.Resource) (*otlptrace.Exporter, *sdktrace.TracerProvider, func(context.Context) error, error) {
	opts := []otlptracegrpc.Option{
		// We prefer insecure for local OTLP collectors (jaeger, honeycomb
		// self-hosted, etc). A user pointing at a TLS-terminated endpoint
		// can still reach it over grpc but will see a clear dial error if
		// the cert doesn't match — we don't try to be clever here.
		otlptracegrpc.WithInsecure(),
	}
	if endpoint != "" {
		opts = append(opts, otlptracegrpc.WithEndpoint(endpoint))
	}
	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("otel: build trace exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	return exporter, tp, tp.Shutdown, nil
}

func buildMeterProvider(ctx context.Context, endpoint string, res *resource.Resource) (*sdkmetric.MeterProvider, func(context.Context) error, error) {
	opts := []otlpmetricgrpc.Option{
		otlpmetricgrpc.WithInsecure(),
	}
	if endpoint != "" {
		opts = append(opts, otlpmetricgrpc.WithEndpoint(endpoint))
	}
	exporter, err := otlpmetricgrpc.New(ctx, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("otel: build metric exporter: %w", err)
	}

	// A 15-second interval is a reasonable default for a desktop app where
	// metrics are not the hot-path; the app doesn't emit enough volume to
	// need sub-second granularity.
	reader := sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(15*time.Second))
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
	)
	return mp, mp.Shutdown, nil
}

func (p *Provider) buildMetrics() error {
	var errs []error
	addErr := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	var err error
	p.metrics.TurnsStarted, err = p.meter.Int64Counter("turns.started",
		metric.WithDescription("Live turn spans registered from provider turn starts."))
	addErr(err)
	p.metrics.TurnsCompleted, err = p.meter.Int64Counter("turns.completed",
		metric.WithDescription("Terminal turn spans that completed without error or interruption."))
	addErr(err)
	p.metrics.TurnsErrored, err = p.meter.Int64Counter("turns.errored",
		metric.WithDescription("Terminal turn spans that ended with a provider error, interruption, persistence failure, or cleanup."))
	addErr(err)
	p.metrics.ItemsPersisted, err = p.meter.Int64Counter("items.persisted",
		metric.WithDescription("Items inserted by triage after a provider event."))
	addErr(err)
	p.metrics.PayloadsPersisted, err = p.meter.Int64Counter("payloads.persisted",
		metric.WithDescription("Heavy payloads persisted by triage."))
	addErr(err)
	p.metrics.ProviderFrames, err = p.meter.Int64Histogram("provider.stream.frames",
		metric.WithDescription("Size in bytes of a single provider stream frame."),
		metric.WithUnit("By"))
	addErr(err)
	p.metrics.ReplayEventsQueued, err = p.meter.Int64Counter("replay.events.queued",
		metric.WithDescription("Replay events enqueued for write."))
	addErr(err)
	p.metrics.ReplayEventsDropped, err = p.meter.Int64Counter("replay.events.dropped",
		metric.WithDescription("Replay events dropped because the bounded channel was full."))
	addErr(err)

	return errors.Join(errs...)
}

// Enabled reports whether the Provider is wired to real exporters.
func (p *Provider) Enabled() bool {
	if p == nil {
		return false
	}
	return p.enabled
}

// Endpoint returns the OTLP endpoint this provider was built against. Empty
// for the no-op provider or when the user left the field blank.
func (p *Provider) Endpoint() string {
	if p == nil {
		return ""
	}
	return p.endpoint
}

// Tracer returns the tracer the rest of the app should use. Callers must not
// reach for otel.GetTracerProvider() — we avoid the global on purpose.
func (p *Provider) Tracer() trace.Tracer {
	if p == nil {
		return tracenoop.NewTracerProvider().Tracer(instrumentationName)
	}
	return p.tracer
}

// Meter mirrors Tracer for metrics.
func (p *Provider) Meter() metric.Meter {
	if p == nil {
		return metricnoop.NewMeterProvider().Meter(instrumentationName)
	}
	return p.meter
}

// Metrics exposes the pre-built instruments.
func (p *Provider) Metrics() Metrics {
	if p == nil {
		m, _ := newNoopMetrics()
		return m
	}
	return p.metrics
}

// TracerProvider exposes the trace.TracerProvider.
func (p *Provider) TracerProvider() trace.TracerProvider {
	if p == nil {
		return tracenoop.NewTracerProvider()
	}
	return p.tracerProvider
}

// MeterProvider exposes the metric.MeterProvider.
func (p *Provider) MeterProvider() metric.MeterProvider {
	if p == nil {
		return metricnoop.NewMeterProvider()
	}
	return p.meterProvider
}

// newNoopMetrics builds a Metrics struct whose instruments are backed by the
// noop meter. Used when Provider is nil so callers can still invoke
// Add/Record without nil-checking.
func newNoopMetrics() (Metrics, error) {
	meter := metricnoop.NewMeterProvider().Meter(instrumentationName)
	ts, err := meter.Int64Counter("turns.started")
	if err != nil {
		return Metrics{}, err
	}
	tc, err := meter.Int64Counter("turns.completed")
	if err != nil {
		return Metrics{}, err
	}
	te, err := meter.Int64Counter("turns.errored")
	if err != nil {
		return Metrics{}, err
	}
	ip, err := meter.Int64Counter("items.persisted")
	if err != nil {
		return Metrics{}, err
	}
	pp, err := meter.Int64Counter("payloads.persisted")
	if err != nil {
		return Metrics{}, err
	}
	pf, err := meter.Int64Histogram("provider.stream.frames")
	if err != nil {
		return Metrics{}, err
	}
	rq, err := meter.Int64Counter("replay.events.queued")
	if err != nil {
		return Metrics{}, err
	}
	rd, err := meter.Int64Counter("replay.events.dropped")
	if err != nil {
		return Metrics{}, err
	}
	return Metrics{
		TurnsStarted:        ts,
		TurnsCompleted:      tc,
		TurnsErrored:        te,
		ItemsPersisted:      ip,
		PayloadsPersisted:   pp,
		ProviderFrames:      pf,
		ReplayEventsQueued:  rq,
		ReplayEventsDropped: rd,
	}, nil
}
