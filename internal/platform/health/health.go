package health

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

type CheckFunc func(ctx context.Context) error

type Status string

const (
	StatusUp   Status = "UP"
	StatusDown Status = "DOWN"
)

type CheckResult struct {
	Status    Status `json:"status"`
	Error     string `json:"error,omitempty"`
	LatencyMS int64  `json:"latencyMs"`
}

type Report struct {
	Status    Status                 `json:"status"`
	Checks    map[string]CheckResult `json:"checks,omitempty"`
	CheckedAt time.Time              `json:"checkedAt"`
}

type check struct {
	name string
	fn   CheckFunc
}

type Checker struct {
	timeout  time.Duration
	cacheTTL time.Duration
	now      func() time.Time

	mu       sync.Mutex
	checks   []check
	cached   Report
	cachedAt time.Time

	draining atomic.Bool
}

func NewChecker(timeout, cacheTTL time.Duration) *Checker {
	return &Checker{timeout: timeout, cacheTTL: cacheTTL, now: time.Now}
}

func (c *Checker) Register(name string, fn CheckFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checks = append(c.checks, check{name: name, fn: fn})
	c.cachedAt = time.Time{}
}

func (c *Checker) StartDraining() { c.draining.Store(true) }

func (c *Checker) Draining() bool { return c.draining.Load() }

func (c *Checker) Ready(ctx context.Context) Report {
	if c.draining.Load() {
		return Report{Status: StatusDown, Checks: map[string]CheckResult{"shutdown": {Status: StatusDown, Error: "instance is draining"}}, CheckedAt: c.now().UTC()}
	}

	c.mu.Lock()
	if !c.cachedAt.IsZero() && c.now().Sub(c.cachedAt) < c.cacheTTL {
		report := c.cached
		c.mu.Unlock()
		return report
	}
	checks := slices.Clone(c.checks)
	c.mu.Unlock()

	report := c.run(ctx, checks)

	c.mu.Lock()
	c.cached, c.cachedAt = report, c.now()
	c.mu.Unlock()
	return report
}

func (c *Checker) run(ctx context.Context, checks []check) Report {
	results := make([]CheckResult, len(checks))
	var wg sync.WaitGroup
	for i, chk := range checks {
		wg.Go(func() {
			checkCtx, cancel := context.WithTimeout(ctx, c.timeout)
			defer cancel()
			started := c.now()
			err := runCheck(checkCtx, chk.fn)
			result := CheckResult{Status: StatusUp, LatencyMS: c.now().Sub(started).Milliseconds()}
			if err != nil {
				result.Status, result.Error = StatusDown, err.Error()
			}
			results[i] = result
		})
	}
	wg.Wait()

	report := Report{Status: StatusUp, Checks: make(map[string]CheckResult, len(checks)), CheckedAt: c.now().UTC()}
	for i, chk := range checks {
		report.Checks[chk.name] = results[i]
		if results[i].Status == StatusDown {
			report.Status = StatusDown
		}
	}
	return report
}

func runCheck(ctx context.Context, fn CheckFunc) error {
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("health check timed out: %w", ctx.Err())
	}
}

func (c *Checker) LiveHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, Report{Status: StatusUp, CheckedAt: c.now().UTC()})
	})
}

func (c *Checker) ReadyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		report := c.Ready(r.Context())
		code := http.StatusOK
		if report.Status != StatusUp {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, report)
	})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
