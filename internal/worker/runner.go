package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"time"
)

type Worker interface {
	Name() string
	Run(ctx context.Context) error
}

type Runner struct {
	worker Worker
	logger *slog.Logger

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	runErr  error
	started bool
}

func NewRunner(w Worker, logger *slog.Logger) *Runner {
	return &Runner{worker: w, logger: logger.With(slog.String("worker", w.Name()))}
}

func (r *Runner) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return fmt.Errorf("worker %s already started", r.worker.Name())
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r.cancel, r.done, r.started = cancel, make(chan struct{}), true

	go func() {
		defer close(r.done)
		defer func() {
			if recovered := recover(); recovered != nil {
				r.logger.Error("worker panicked", slog.Any("panic", recovered), slog.String("stack", string(debug.Stack())))
				r.setErr(fmt.Errorf("worker %s panicked: %v", r.worker.Name(), recovered))
			}
		}()
		r.logger.Info("worker started")
		err := r.worker.Run(runCtx)
		if err != nil && !errors.Is(err, context.Canceled) {
			r.logger.Error("worker stopped with error", slog.Any("error", err))
			r.setErr(err)
			return
		}
		r.logger.Info("worker stopped")
	}()
	return nil
}

func (r *Runner) setErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runErr = err
}

func (r *Runner) Stop(ctx context.Context) error {
	r.mu.Lock()
	if !r.started {
		r.mu.Unlock()
		return nil
	}
	cancel, done := r.cancel, r.done
	r.mu.Unlock()

	cancel()
	select {
	case <-done:
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.runErr
	case <-ctx.Done():
		return fmt.Errorf("worker %s did not stop before the deadline: %w", r.worker.Name(), ctx.Err())
	}
}

func (r *Runner) Done() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done
}

type Iteration func(ctx context.Context) (moreWork bool, err error)

type LoopSettings struct {
	Interval   time.Duration
	MaxBackoff time.Duration
	OnError    func(err error)
}

func Loop(ctx context.Context, s LoopSettings, iterate Iteration) error {
	backoff := s.Interval
	for {
		more, err := iterate(ctx)
		wait := jitter(s.Interval)
		switch {
		case ctx.Err() != nil:
			return fmt.Errorf("loop stopped: %w", ctx.Err())
		case err != nil:
			if s.OnError != nil {
				s.OnError(err)
			}
			backoff = min(backoff*2, s.MaxBackoff)
			wait = jitter(backoff)
		case more:
			backoff = s.Interval
			continue
		default:
			backoff = s.Interval
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("loop stopped: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + rand.N(d/2+1)
}
