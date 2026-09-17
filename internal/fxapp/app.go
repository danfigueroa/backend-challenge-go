package fxapp

import (
	"context"
	"log/slog"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"github.com/danfigueroa/backend-challenge-go/internal/platform/config"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/health"
)

func Options(cfg config.Config, extra ...fx.Option) fx.Option {
	options := []fx.Option{
		fx.Supply(cfg),
		fx.StartTimeout(cfg.App.StartTimeout),
		fx.StopTimeout(cfg.App.ShutdownTimeout),
		fx.WithLogger(func(logger *slog.Logger) fxevent.Logger {
			l := &fxevent.SlogLogger{Logger: logger.With(slog.String("component", "fx"))}
			l.UseLogLevel(slog.LevelDebug)
			l.UseErrorLevel(slog.LevelError)
			return l
		}),
		ObservabilityModule,
		PostgresModule,
		ApplicationModule,
		AdminModule,
	}
	if cfg.App.HasRole(config.RoleAPI) {
		options = append(options, APIModule)
	}
	if cfg.App.HasRole(config.RolePendingRef) {
		options = append(options, PendingReferenceModule)
	}
	options = append(options, extra...)
	options = append(options, fx.Invoke(registerDrain))
	return fx.Options(options...)
}

func registerDrain(lc fx.Lifecycle, checker *health.Checker, logger *slog.Logger) {
	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			checker.StartDraining()
			logger.InfoContext(ctx, "instance draining: readiness now reports DOWN")
			return nil
		},
	})
}
