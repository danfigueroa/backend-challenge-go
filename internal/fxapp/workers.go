package fxapp

import (
	"log/slog"

	"go.uber.org/fx"

	"github.com/danfigueroa/backend-challenge-go/internal/app/wageringapp"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/config"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/metrics"
	"github.com/danfigueroa/backend-challenge-go/internal/worker"
	"github.com/danfigueroa/backend-challenge-go/internal/worker/pendingref"
)

type PendingReferenceRunner struct {
	*worker.Runner
}

var PendingReferenceModule = fx.Module("pending-references",
	fx.Provide(NewPendingReferenceRunner),
	fx.Invoke(func(*PendingReferenceRunner) {}),
)

func NewPendingReferenceRunner(lc fx.Lifecycle, cfg config.Config, svc *wageringapp.Service, m *metrics.Metrics, logger *slog.Logger) *PendingReferenceRunner {
	w := pendingref.New(svc, pendingref.Settings{
		PollInterval:     cfg.Pending.PollInterval,
		MaxBackoff:       cfg.Pending.ErrorBackoff,
		BatchSize:        cfg.Pending.BatchSize,
		IterationTimeout: cfg.Pending.Iteration,
	}, m, logger)
	runner := worker.NewRunner(w, logger)
	lc.Append(fx.StartStopHook(runner.Start, runner.Stop))
	return &PendingReferenceRunner{Runner: runner}
}
