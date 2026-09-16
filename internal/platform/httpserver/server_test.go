package httpserver_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/danfigueroa/backend-challenge-go/internal/platform/httpserver"
)

func settings(addr string) httpserver.Settings {
	return httpserver.Settings{Name: "test", Addr: addr, ReadHeaderTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: time.Second}
}

func TestServerLifecycleCompletesInFlightRequests(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"), goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"))

	started := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, "finished")
	})
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv := httpserver.New(settings("127.0.0.1:0"), handler, logger)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	body := make(chan string, 1)
	go func() {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+srv.Addr()+"/", nil)
		resp, err := client.Do(req)
		if err != nil {
			body <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		body <- string(data)
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("stop = %v", err)
	}
	if got := <-body; got != "finished" {
		t.Errorf("in-flight request = %q, want completion before shutdown", got)
	}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+srv.Addr()+"/", nil)
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
		t.Error("server accepted a request after shutdown")
	}
}

func TestServerStartFailsFastOnBusyAddress(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	first := httpserver.New(settings("127.0.0.1:0"), http.NotFoundHandler(), logger)
	if err := first.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Stop(context.Background()) }()

	second := httpserver.New(settings(first.Addr()), http.NotFoundHandler(), logger)
	err := second.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "listen") {
		t.Errorf("second start = %v, want listen error", err)
	}
	if err := second.Stop(context.Background()); err != nil {
		t.Errorf("stopping a server that never started = %v", err)
	}
}
