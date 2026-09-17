package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

type Role string

const (
	RoleAPI        Role = "api"
	RoleConsumer   Role = "consumer"
	RoleOutbox     Role = "outbox"
	RolePendingRef Role = "pendingref"
)

var allRoles = []Role{RoleAPI, RoleConsumer, RoleOutbox, RolePendingRef}

type Config struct {
	App      App
	HTTP     HTTP
	Admin    Admin
	Database Database
	Retry    Retry
	Pending  Pending
	Tracing  Tracing
	Auth     Auth
}

type App struct {
	Name            string        `env:"APP_NAME" envDefault:"wallet-service"`
	InstanceID      string        `env:"APP_INSTANCE_ID"`
	Roles           []Role        `env:"APP_ROLES" envDefault:"api,consumer,outbox,pendingref" envSeparator:","`
	LogLevel        string        `env:"LOG_LEVEL" envDefault:"info"`
	StartTimeout    time.Duration `env:"APP_START_TIMEOUT" envDefault:"60s"`
	ShutdownTimeout time.Duration `env:"APP_SHUTDOWN_TIMEOUT" envDefault:"30s"`
}

type HTTP struct {
	Addr              string        `env:"HTTP_ADDR" envDefault:":8080"`
	ReadHeaderTimeout time.Duration `env:"HTTP_READ_HEADER_TIMEOUT" envDefault:"5s"`
	ReadTimeout       time.Duration `env:"HTTP_READ_TIMEOUT" envDefault:"10s"`
	WriteTimeout      time.Duration `env:"HTTP_WRITE_TIMEOUT" envDefault:"15s"`
	IdleTimeout       time.Duration `env:"HTTP_IDLE_TIMEOUT" envDefault:"60s"`
	RequestTimeout    time.Duration `env:"HTTP_REQUEST_TIMEOUT" envDefault:"10s"`
	MaxBodyBytes      int64         `env:"HTTP_MAX_BODY_BYTES" envDefault:"65536"`
}

type Admin struct {
	Addr string `env:"ADMIN_ADDR" envDefault:":9090"`
}

type Database struct {
	URL              string        `env:"DATABASE_URL"`
	MigrationsURL    string        `env:"DATABASE_MIGRATIONS_URL"`
	MaxConns         int32         `env:"DATABASE_MAX_CONNS" envDefault:"20"`
	MinConns         int32         `env:"DATABASE_MIN_CONNS" envDefault:"2"`
	LockTimeout      time.Duration `env:"DATABASE_LOCK_TIMEOUT" envDefault:"2s"`
	StatementTimeout time.Duration `env:"DATABASE_STATEMENT_TIMEOUT" envDefault:"5s"`
	HealthTimeout    time.Duration `env:"DATABASE_HEALTH_TIMEOUT" envDefault:"2s"`
}

type Retry struct {
	Attempts  int           `env:"RETRY_ATTEMPTS" envDefault:"5"`
	BaseDelay time.Duration `env:"RETRY_BASE_DELAY" envDefault:"20ms"`
	MaxDelay  time.Duration `env:"RETRY_MAX_DELAY" envDefault:"500ms"`
}

type Pending struct {
	TTL          time.Duration `env:"PENDING_TTL" envDefault:"30m"`
	BaseDelay    time.Duration `env:"PENDING_BASE_DELAY" envDefault:"1s"`
	MaxDelay     time.Duration `env:"PENDING_MAX_DELAY" envDefault:"60s"`
	ClaimLease   time.Duration `env:"PENDING_CLAIM_LEASE" envDefault:"30s"`
	PollInterval time.Duration `env:"PENDING_POLL_INTERVAL" envDefault:"1s"`
	BatchSize    int           `env:"PENDING_BATCH_SIZE" envDefault:"50"`
	ErrorBackoff time.Duration `env:"PENDING_ERROR_BACKOFF" envDefault:"30s"`
	Iteration    time.Duration `env:"PENDING_ITERATION_TIMEOUT" envDefault:"20s"`
}

type Auth struct {
	Issuer   string `env:"AUTH_ISSUER"`
	JWKSURL  string `env:"AUTH_JWKS_URL"`
	Audience string `env:"AUTH_AUDIENCE" envDefault:"wallet-api"`
}

type Tracing struct {
	Enabled       bool   `env:"OTEL_TRACES_ENABLED" envDefault:"false"`
	Endpoint      string `env:"OTEL_EXPORTER_OTLP_ENDPOINT"`
	Insecure      bool   `env:"OTEL_EXPORTER_OTLP_INSECURE" envDefault:"true"`
	SamplePercent int    `env:"OTEL_TRACES_SAMPLE_PERCENT" envDefault:"100"`
}

func Load() (Config, error) {
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return Config{}, fmt.Errorf("config: parse environment: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.App.InstanceID == "" {
		if host, err := os.Hostname(); err == nil {
			c.App.InstanceID = host
		}
	}
	if c.Database.MigrationsURL == "" {
		c.Database.MigrationsURL = c.Database.URL
	}
}

func (c Config) Validate() error {
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}

	check(c.App.Name != "", "APP_NAME is required")
	check(c.App.InstanceID != "", "APP_INSTANCE_ID is required when the hostname is unavailable")
	check(len(c.App.Roles) > 0, "APP_ROLES must list at least one role")
	for _, r := range c.App.Roles {
		check(slices.Contains(allRoles, r), "APP_ROLES contains unknown role %q (valid: %v)", r, allRoles)
	}
	var level slog.Level
	check(level.UnmarshalText([]byte(c.App.LogLevel)) == nil, "LOG_LEVEL %q is invalid", c.App.LogLevel)
	check(c.App.StartTimeout > 0 && c.App.ShutdownTimeout > 0, "APP_START_TIMEOUT and APP_SHUTDOWN_TIMEOUT must be positive")

	check(c.HTTP.Addr != "" && c.Admin.Addr != "", "HTTP_ADDR and ADMIN_ADDR are required")
	check(c.HTTP.Addr != c.Admin.Addr, "HTTP_ADDR and ADMIN_ADDR must differ")
	check(c.HTTP.ReadHeaderTimeout > 0 && c.HTTP.ReadTimeout > 0 && c.HTTP.WriteTimeout > 0 && c.HTTP.IdleTimeout > 0 && c.HTTP.RequestTimeout > 0,
		"HTTP timeouts must be positive")
	check(c.HTTP.RequestTimeout < c.HTTP.WriteTimeout, "HTTP_REQUEST_TIMEOUT must be shorter than HTTP_WRITE_TIMEOUT")
	check(c.HTTP.MaxBodyBytes > 0, "HTTP_MAX_BODY_BYTES must be positive")

	check(strings.HasPrefix(c.Database.URL, "postgres://") || strings.HasPrefix(c.Database.URL, "postgresql://"),
		"DATABASE_URL must be a postgres:// URL")
	check(c.Database.MaxConns >= 1 && c.Database.MinConns >= 0 && c.Database.MinConns <= c.Database.MaxConns,
		"DATABASE_MIN_CONNS/DATABASE_MAX_CONNS are inconsistent")
	check(c.Database.LockTimeout > 0 && c.Database.StatementTimeout > 0 && c.Database.HealthTimeout > 0,
		"database timeouts must be positive")
	check(c.Database.LockTimeout < c.Database.StatementTimeout, "DATABASE_LOCK_TIMEOUT must be shorter than DATABASE_STATEMENT_TIMEOUT")

	check(c.Retry.Attempts >= 1 && c.Retry.BaseDelay > 0 && c.Retry.MaxDelay >= c.Retry.BaseDelay, "RETRY_* values are inconsistent")
	check(c.Pending.TTL > 0 && c.Pending.BaseDelay > 0 && c.Pending.MaxDelay >= c.Pending.BaseDelay, "PENDING_TTL/PENDING_*_DELAY values are inconsistent")
	check(c.Pending.ClaimLease > 0 && c.Pending.PollInterval > 0 && c.Pending.BatchSize > 0, "PENDING_CLAIM_LEASE, PENDING_POLL_INTERVAL and PENDING_BATCH_SIZE must be positive")
	check(c.Pending.ErrorBackoff >= c.Pending.PollInterval, "PENDING_ERROR_BACKOFF must be at least PENDING_POLL_INTERVAL")
	check(c.Pending.Iteration > 0 && c.Pending.Iteration < c.Pending.ClaimLease, "PENDING_ITERATION_TIMEOUT must be positive and shorter than PENDING_CLAIM_LEASE")
	check(c.Pending.Iteration < c.App.ShutdownTimeout, "PENDING_ITERATION_TIMEOUT must be shorter than APP_SHUTDOWN_TIMEOUT")

	if c.App.HasRole(RoleAPI) {
		check(isHTTPURL(c.Auth.Issuer), "AUTH_ISSUER must be an http(s) URL when the api role is enabled")
		check(isHTTPURL(c.Auth.JWKSURL), "AUTH_JWKS_URL must be an http(s) URL when the api role is enabled")
		check(c.Auth.Audience != "", "AUTH_AUDIENCE is required when the api role is enabled")
	}

	if c.Tracing.Enabled {
		check(c.Tracing.Endpoint != "", "OTEL_EXPORTER_OTLP_ENDPOINT is required when tracing is enabled")
		check(c.Tracing.SamplePercent >= 0 && c.Tracing.SamplePercent <= 100, "OTEL_TRACES_SAMPLE_PERCENT must be within [0, 100]")
	}

	if len(errs) > 0 {
		return fmt.Errorf("config: invalid configuration: %w", errors.Join(errs...))
	}
	return nil
}

func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func (a App) HasRole(r Role) bool { return slices.Contains(a.Roles, r) }

func (a App) SlogLevel() slog.Level {
	var level slog.Level
	_ = level.UnmarshalText([]byte(a.LogLevel))
	return level
}
