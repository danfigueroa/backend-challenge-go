package fxapp

import (
	"context"
	"log/slog"
	"os"
	"time"

	"go.uber.org/fx"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/config"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/health"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/logging"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/metrics"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/tracing"
)

const healthCacheTTL = time.Second

var ObservabilityModule = fx.Module("observability",
	fx.Provide(
		NewLogger,
		metrics.New,
		fx.Annotate(metrics.NewObserver, fx.As(new(app.Observer))),
		NewTracing,
		NewHealthChecker,
	),
)

func NewLogger(cfg config.Config) *slog.Logger {
	logger := logging.New(os.Stdout, cfg.App.SlogLevel(), cfg.App.Name, cfg.App.InstanceID)
	slog.SetDefault(logger)
	return logger
}

func NewTracing(lc fx.Lifecycle, cfg config.Config) (*tracing.Provider, error) {
	provider, err := tracing.New(context.Background(), tracing.Settings{
		Enabled: cfg.Tracing.Enabled, Endpoint: cfg.Tracing.Endpoint, Insecure: cfg.Tracing.Insecure,
		SamplePercent: cfg.Tracing.SamplePercent, ServiceName: cfg.App.Name, InstanceID: cfg.App.InstanceID,
	})
	if err != nil {
		return nil, err
	}
	lc.Append(fx.StopHook(provider.Shutdown))
	return provider, nil
}

func NewHealthChecker(cfg config.Config) *health.Checker {
	return health.NewChecker(cfg.Database.HealthTimeout, healthCacheTTL)
}
