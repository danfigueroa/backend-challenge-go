package config_test

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/danfigueroa/backend-challenge-go/internal/platform/config"
)

func setAuth(t *testing.T) {
	t.Helper()
	t.Setenv("SQS_INPUT_QUEUE_URL", "http://localstack:4566/000000000000/wager-transactions.fifo")
	t.Setenv("SQS_DLQ_URL", "http://localstack:4566/000000000000/wager-transactions-dlq.fifo")
	t.Setenv("SNS_EVENTS_TOPIC_ARN", "arn:aws:sns:us-east-1:000000000000:wallet-events.fifo")
	t.Setenv("AUTH_ISSUER", "http://keycloak:8080/realms/wagering")
	t.Setenv("AUTH_JWKS_URL", "http://keycloak:8080/realms/wagering/protocol/openid-connect/certs")
}

func TestAuthIsOnlyRequiredForTheAPIRole(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@db:5432/wallet")
	t.Setenv("APP_ROLES", "pendingref")
	if _, err := config.Load(); err != nil {
		t.Errorf("worker-only instance must not require auth or messaging settings: %v", err)
	}
	t.Setenv("APP_ROLES", "consumer")
	if _, err := config.Load(); err == nil {
		t.Error("consumer instance without queue settings accepted")
	}
	t.Setenv("APP_ROLES", "outbox")
	if _, err := config.Load(); err == nil {
		t.Error("outbox instance without topic ARN accepted")
	}
	t.Setenv("APP_ROLES", "api")
	if _, err := config.Load(); err == nil {
		t.Error("api instance without auth settings accepted")
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@db:5432/wallet")
	t.Setenv("APP_INSTANCE_ID", "instance-1")
	setAuth(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []config.Role{config.RoleAPI, config.RoleConsumer, config.RoleOutbox, config.RolePendingRef} {
		if !cfg.App.HasRole(r) {
			t.Errorf("default roles miss %s", r)
		}
	}
	if cfg.Database.MigrationsURL != cfg.Database.URL || cfg.HTTP.Addr != ":8080" || cfg.Admin.Addr != ":9090" ||
		cfg.Pending.TTL != 30*time.Minute || cfg.App.SlogLevel() != slog.LevelInfo || cfg.Tracing.Enabled {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@db:5432/wallet")
	t.Setenv("DATABASE_MIGRATIONS_URL", "postgres://owner:p@db:5432/wallet")
	t.Setenv("APP_ROLES", "api,outbox")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("PENDING_TTL", "10s")
	setAuth(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.App.HasRole(config.RoleAPI) || cfg.App.HasRole(config.RoleConsumer) || cfg.Pending.TTL != 10*time.Second ||
		cfg.Database.MigrationsURL == cfg.Database.URL || cfg.App.SlogLevel() != slog.LevelDebug {
		t.Errorf("overrides not applied: %+v", cfg)
	}
}

func TestLoadReportsEveryInvalidSetting(t *testing.T) {
	t.Setenv("DATABASE_URL", "mysql://nope")
	t.Setenv("APP_ROLES", "api,jobs")
	t.Setenv("LOG_LEVEL", "loud")
	t.Setenv("ADMIN_ADDR", ":8080")
	t.Setenv("DATABASE_LOCK_TIMEOUT", "10s")
	t.Setenv("OTEL_TRACES_ENABLED", "true")
	t.Setenv("AUTH_ISSUER", "keycloak/realms/wagering")

	_, err := config.Load()
	if err == nil {
		t.Fatal("invalid configuration accepted")
	}
	for _, fragment := range []string{"DATABASE_URL", `unknown role "jobs"`, "LOG_LEVEL", "must differ", "DATABASE_LOCK_TIMEOUT", "OTEL_EXPORTER_OTLP_ENDPOINT", "AUTH_ISSUER", "AUTH_JWKS_URL"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error does not mention %q: %v", fragment, err)
		}
	}
}

func TestLoadRejectsUnparsableValues(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@db/wallet")
	t.Setenv("PENDING_TTL", "forever")

	_, err := config.Load()
	if err == nil || errors.Unwrap(err) == nil {
		t.Fatalf("unparsable duration accepted: %v", err)
	}
}
