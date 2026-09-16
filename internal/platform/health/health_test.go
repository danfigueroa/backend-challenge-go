package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danfigueroa/backend-challenge-go/internal/platform/health"
)

func TestReadyAggregatesChecks(t *testing.T) {
	t.Parallel()

	c := health.NewChecker(time.Second, 0)
	c.Register("postgres", func(context.Context) error { return nil })
	if r := c.Ready(context.Background()); r.Status != health.StatusUp || r.Checks["postgres"].Status != health.StatusUp {
		t.Fatalf("report = %+v", r)
	}

	c.Register("sqs", func(context.Context) error { return errors.New("queue unreachable") })
	r := c.Ready(context.Background())
	if r.Status != health.StatusDown || r.Checks["sqs"].Error != "queue unreachable" || r.Checks["postgres"].Status != health.StatusUp {
		t.Errorf("report = %+v", r)
	}
}

func TestReadyTimesOutSlowChecks(t *testing.T) {
	t.Parallel()

	c := health.NewChecker(50*time.Millisecond, 0)
	release := make(chan struct{})
	defer close(release)
	c.Register("stuck", func(context.Context) error { <-release; return nil })

	started := time.Now()
	r := c.Ready(context.Background())
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("readiness blocked for %v", elapsed)
	}
	if r.Status != health.StatusDown || r.Checks["stuck"].Status != health.StatusDown {
		t.Errorf("report = %+v", r)
	}
}

func TestReadyCachesResults(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	c := health.NewChecker(time.Second, time.Hour)
	c.Register("db", func(context.Context) error { calls.Add(1); return nil })
	for range 5 {
		c.Ready(context.Background())
	}
	if calls.Load() != 1 {
		t.Errorf("check executed %d times within cache TTL", calls.Load())
	}
}

func TestDrainingMakesInstanceNotReady(t *testing.T) {
	t.Parallel()

	c := health.NewChecker(time.Second, time.Hour)
	c.Register("db", func(context.Context) error { return nil })
	c.Ready(context.Background())
	c.StartDraining()
	if !c.Draining() || c.Ready(context.Background()).Status != health.StatusDown {
		t.Error("draining instance still reports ready")
	}
}

func TestHandlers(t *testing.T) {
	t.Parallel()

	c := health.NewChecker(time.Second, 0)
	down := atomic.Bool{}
	c.Register("db", func(context.Context) error {
		if down.Load() {
			return errors.New("down")
		}
		return nil
	})

	get := func(h http.Handler) (int, health.Report) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
		var r health.Report
		if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		if rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("content type = %q", rec.Header().Get("Content-Type"))
		}
		return rec.Code, r
	}

	if code, r := get(c.ReadyHandler()); code != http.StatusOK || r.Status != health.StatusUp {
		t.Errorf("ready = %d %+v", code, r)
	}
	down.Store(true)
	if code, r := get(c.ReadyHandler()); code != http.StatusServiceUnavailable || r.Status != health.StatusDown {
		t.Errorf("ready while down = %d %+v", code, r)
	}
	if code, r := get(c.LiveHandler()); code != http.StatusOK || r.Status != health.StatusUp {
		t.Errorf("live = %d %+v", code, r)
	}
}
