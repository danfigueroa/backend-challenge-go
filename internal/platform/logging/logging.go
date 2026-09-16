package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

const (
	KeyCorrelationID = "correlationId"
	KeyMessageID     = "messageId"
	KeyTransactionID = "transactionId"
	KeyWalletID      = "walletId"
	KeyProviderID    = "providerId"
	KeyTraceID       = "traceId"
	KeySpanID        = "spanId"
)

var sensitiveKeys = []string{"authorization", "password", "secret", "token", "credential", "apikey", "api_key", "cookie"}

type fieldsKey struct{}

func With(ctx context.Context, attrs ...slog.Attr) context.Context {
	existing, _ := ctx.Value(fieldsKey{}).([]slog.Attr)
	merged := make([]slog.Attr, 0, len(existing)+len(attrs))
	for _, current := range existing {
		if !containsKey(attrs, current.Key) {
			merged = append(merged, current)
		}
	}
	merged = append(merged, attrs...)
	return context.WithValue(ctx, fieldsKey{}, merged)
}

func Fields(ctx context.Context) []slog.Attr {
	fields, _ := ctx.Value(fieldsKey{}).([]slog.Attr)
	return fields
}

func containsKey(attrs []slog.Attr, key string) bool {
	for _, a := range attrs {
		if a.Key == key {
			return true
		}
	}
	return false
}

type contextHandler struct {
	slog.Handler
}

func (h contextHandler) Handle(ctx context.Context, record slog.Record) error {
	record.AddAttrs(Fields(ctx)...)
	if span := trace.SpanContextFromContext(ctx); span.IsValid() {
		record.AddAttrs(slog.String(KeyTraceID, span.TraceID().String()), slog.String(KeySpanID, span.SpanID().String()))
	}
	if err := h.Handler.Handle(ctx, record); err != nil {
		return fmt.Errorf("logging: handle record: %w", err)
	}
	return nil
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}

func New(w io.Writer, level slog.Level, service, instance string) *slog.Logger {
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level, ReplaceAttr: redact})
	return slog.New(contextHandler{handler}).With(slog.String("service", service), slog.String("instance", instance))
}

func redact(_ []string, a slog.Attr) slog.Attr {
	key := strings.ToLower(a.Key)
	for _, sensitive := range sensitiveKeys {
		if strings.Contains(key, sensitive) {
			return slog.String(a.Key, "[REDACTED]")
		}
	}
	return a
}
