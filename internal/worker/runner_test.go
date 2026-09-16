package worker_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/danfigueroa/backend-challenge-go/internal/worker"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

type funcWorker struct {
	name string
	run  func(ctx context.Context) error
}

func (w funcWorker) Name() string                  { return w.name }
func (w funcWorker) Run(ctx context.Context) error { return w.run(ctx) }

func logger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func TestRunnerStopsGracefully(t *testing.T) {
	var iterations atomic.Int32
	w := funcWorker{name: "loop", run: func(ctx context.Context) error {
		return worker.Loop(ctx, worker.LoopSettings{Interval: time.Millisecond, MaxBackoff: 10 * time.Millisecond}, func(context.Context) (bool, error) {
			iterations.Add(1)
			return false, nil
		})
	}}
	r := worker.NewRunner(w, logger())
	startCtx, cancelStart := context.WithCancel(context.Background())
	if err := r.Start(startCtx); err != nil {
		t.Fatal(err)
	}
	cancelStart()

	time.Sleep(20 * time.Millisecond)
	if iterations.Load() == 0 {
		t.Fatal("worker stopped when the start context was cancelled")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("stop = %v", err)
	}
	select {
	case <-r.Done():
	default:
		t.Error("done channel not closed after stop")
	}
	if err := r.Start(context.Background()); err == nil {
		t.Error("runner restarted")
	}
}

func TestRunnerStopDeadline(t *testing.T) {
	release := make(chan struct{})
	w := funcWorker{name: "stubborn", run: func(context.Context) error {
		<-release
		return nil
	}}
	r := worker.NewRunner(w, logger())
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := r.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("stop = %v, want deadline exceeded", err)
	}
	close(release)
	<-r.Done()
}

func TestRunnerReportsFailuresAndPanics(t *testing.T) {
	boom := errors.New("boom")
	for name, run := range map[string]func(context.Context) error{
		"error": func(context.Context) error { return boom },
		"panic": func(context.Context) error { panic("kaboom") },
	} {
		r := worker.NewRunner(funcWorker{name: name, run: run}, logger())
		if err := r.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		<-r.Done()
		if err := r.Stop(context.Background()); err == nil {
			t.Errorf("%s: failure not reported on stop", name)
		}
	}
	unstarted := worker.NewRunner(funcWorker{name: "idle"}, logger())
	if err := unstarted.Stop(context.Background()); err != nil {
		t.Errorf("stopping an unstarted runner = %v", err)
	}
}

func TestLoopBacksOffOnErrorsAndDrainsBacklog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var (
		calls  atomic.Int32
		errs   atomic.Int32
		result = make(chan error, 1)
	)
	go func() {
		result <- worker.Loop(ctx, worker.LoopSettings{
			Interval: time.Hour, MaxBackoff: time.Hour,
			OnError: func(error) { errs.Add(1) },
		}, func(context.Context) (bool, error) {
			n := calls.Add(1)
			if n < 5 {
				return true, nil
			}
			if n == 5 {
				cancel()
				return false, errors.New("transient")
			}
			return false, nil
		})
	}()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("loop = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("loop did not drain backlog without waiting or did not stop on cancellation")
	}
	if calls.Load() != 5 {
		t.Errorf("calls = %d, want 5 (4 immediate re-runs while more work, then stop)", calls.Load())
	}
}
