package tracing

import (
	"context"
	"fmt"
	"net/url"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

type Settings struct {
	Enabled       bool
	Endpoint      string
	Insecure      bool
	SamplePercent int
	ServiceName   string
	InstanceID    string
}

type Provider struct {
	trace.TracerProvider
	shutdown func(context.Context) error
}

func New(ctx context.Context, s Settings) (*Provider, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	if !s.Enabled {
		provider := noop.NewTracerProvider()
		otel.SetTracerProvider(provider)
		return &Provider{TracerProvider: provider, shutdown: func(context.Context) error { return nil }}, nil
	}

	endpoint, err := url.Parse(s.Endpoint)
	if err != nil || endpoint.Host == "" {
		return nil, fmt.Errorf("tracing: invalid OTLP endpoint %q", s.Endpoint)
	}
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(endpoint.Host)}
	if s.Insecure || endpoint.Scheme == "http" {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("tracing: create OTLP exporter: %w", err)
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(semconv.SchemaURL,
		semconv.ServiceName(s.ServiceName),
		semconv.ServiceInstanceID(s.InstanceID),
	))
	if err != nil {
		return nil, fmt.Errorf("tracing: build resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(float64(s.SamplePercent)/100))),
	)
	otel.SetTracerProvider(provider)
	return &Provider{TracerProvider: provider, shutdown: provider.Shutdown}, nil
}

func (p *Provider) Shutdown(ctx context.Context) error {
	if err := p.shutdown(ctx); err != nil {
		return fmt.Errorf("tracing: shutdown: %w", err)
	}
	return nil
}
