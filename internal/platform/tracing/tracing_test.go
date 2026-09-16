package tracing_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/danfigueroa/backend-challenge-go/internal/platform/tracing"
)

func TestDisabledTracingUsesNoopProviderAndW3CPropagation(t *testing.T) {
	p, err := tracing.New(context.Background(), tracing.Settings{Enabled: false, ServiceName: "svc"})
	if err != nil {
		t.Fatal(err)
	}
	_, span := p.Tracer("test").Start(context.Background(), "op")
	if span.SpanContext().IsValid() {
		t.Error("disabled tracing produced a recording span")
	}
	span.End()

	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(context.Background(), carrier)
	fields := otel.GetTextMapPropagator().Fields()
	if len(fields) == 0 || fields[0] != "traceparent" {
		t.Errorf("propagator fields = %v, want W3C trace context", fields)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Error(err)
	}
}

func TestEnabledTracingExportsAndShutsDown(t *testing.T) {
	p, err := tracing.New(context.Background(), tracing.Settings{
		Enabled: true, Endpoint: "http://127.0.0.1:1", Insecure: true, SamplePercent: 100, ServiceName: "svc", InstanceID: "i",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, span := p.Tracer("test").Start(context.Background(), "op")
	if !span.SpanContext().IsValid() || !span.SpanContext().IsSampled() {
		t.Error("enabled tracing did not sample the span")
	}
	span.End()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = p.Shutdown(ctx)
}

func TestInvalidEndpointIsRejected(t *testing.T) {
	if _, err := tracing.New(context.Background(), tracing.Settings{Enabled: true, Endpoint: "::not a url::"}); err == nil {
		t.Error("invalid endpoint accepted")
	}
}
