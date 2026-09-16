package pendingref

import (
	"context"
	"log/slog"
	"time"

	"github.com/danfigueroa/backend-challenge-go/internal/app/wageringapp"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/metrics"
	"github.com/danfigueroa/backend-challenge-go/internal/worker"
)

const Name = "pending-reference-resolver"

type Resolver interface {
	ResolveDuePending(ctx context.Context, limit int) (wageringapp.ResolveStats, error)
}

type Settings struct {
	PollInterval     time.Duration
	MaxBackoff       time.Duration
	BatchSize        int
	IterationTimeout time.Duration
}

type Worker struct {
	resolver Resolver
	settings Settings
	metrics  *metrics.Metrics
	logger   *slog.Logger
}

func New(resolver Resolver, s Settings, m *metrics.Metrics, logger *slog.Logger) *Worker {
	return &Worker{resolver: resolver, settings: s, metrics: m, logger: logger.With(slog.String("worker", Name))}
}

func (w *Worker) Name() string { return Name }

func (w *Worker) Run(ctx context.Context) error {
	return worker.Loop(ctx, worker.LoopSettings{
		Interval:   w.settings.PollInterval,
		MaxBackoff: w.settings.MaxBackoff,
		OnError: func(err error) {
			w.metrics.WorkerErrors.WithLabelValues(Name).Inc()
			w.logger.WarnContext(ctx, "pending reference iteration failed", slog.Any("error", err))
		},
	}, w.iterate)
}

func (w *Worker) iterate(ctx context.Context) (bool, error) {
	iterationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.settings.IterationTimeout)
	defer cancel()

	stats, err := w.resolver.ResolveDuePending(iterationCtx, w.settings.BatchSize)
	if stats.Claimed > 0 {
		w.logger.InfoContext(ctx, "pending references resolved",
			slog.Int("claimed", stats.Claimed), slog.Int("processed", stats.Processed), slog.Int("rejected", stats.Rejected),
			slog.Int("stillPending", stats.StillPending), slog.Int("failed", stats.Failed), slog.Int("deferred", stats.Deferred))
	}
	w.metrics.AddPendingDeferred(stats.Deferred)
	return err == nil && stats.Claimed == w.settings.BatchSize, err
}
