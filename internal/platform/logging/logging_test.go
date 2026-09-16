package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/danfigueroa/backend-challenge-go/internal/platform/logging"
)

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("log line is not JSON: %q (%v)", buf.String(), err)
	}
	return entry
}

func TestContextFieldsAreAddedToEveryRecord(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := logging.New(&buf, slog.LevelInfo, "wallet-service", "instance-1")

	ctx := logging.With(context.Background(),
		slog.String(logging.KeyCorrelationID, "corr-1"),
		slog.String(logging.KeyWalletID, "wallet-1"),
	)
	ctx = logging.With(ctx, slog.String(logging.KeyCorrelationID, "corr-2"), slog.String(logging.KeyProviderID, "provider-a"))

	traceID, _ := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	spanID, _ := trace.SpanIDFromHex("0102030405060708")
	ctx = trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled}))

	logger.InfoContext(ctx, "transaction processed", slog.String(logging.KeyTransactionID, "tx-1"))
	entry := decode(t, &buf)

	want := map[string]string{
		"msg": "transaction processed", "level": "INFO", "service": "wallet-service", "instance": "instance-1",
		"correlationId": "corr-2", "walletId": "wallet-1", "providerId": "provider-a", "transactionId": "tx-1",
		"traceId": "0102030405060708090a0b0c0d0e0f10", "spanId": "0102030405060708",
	}
	for key, value := range want {
		if entry[key] != value {
			t.Errorf("%s = %v, want %q", key, entry[key], value)
		}
	}
}

func TestSensitiveAttributesAreRedacted(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := logging.New(&buf, slog.LevelDebug, "svc", "i")
	logger.Info("auth", slog.String("Authorization", "Bearer abc"), slog.String("client_secret", "s3cr3t"),
		slog.String("accessToken", "tok"), slog.String("walletId", "w-1"))

	entry := decode(t, &buf)
	for _, key := range []string{"Authorization", "client_secret", "accessToken"} {
		if entry[key] != "[REDACTED]" {
			t.Errorf("%s = %v, want redacted", key, entry[key])
		}
	}
	if entry["walletId"] != "w-1" {
		t.Errorf("non-sensitive attribute changed: %v", entry["walletId"])
	}
}

func TestLevelFiltering(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := logging.New(&buf, slog.LevelWarn, "svc", "i")
	logger.Info("hidden")
	if buf.Len() != 0 {
		t.Errorf("info logged at warn level: %s", buf.String())
	}
	logger.With(slog.String("component", "worker")).WithGroup("details").Warn("shown", slog.Int("n", 1))
	entry := decode(t, &buf)
	if entry["component"] != "worker" {
		t.Errorf("WithAttrs lost: %v", entry)
	}
}
