package fxapp

import (
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/fx"

	"github.com/danfigueroa/backend-challenge-go/internal/platform/config"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/health"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/httpserver"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/metrics"
)

type AdminServer struct {
	*httpserver.Server
}

var AdminModule = fx.Module("admin",
	fx.Provide(NewAdminServer),
	fx.Invoke(func(*AdminServer) {}),
)

func NewAdminServer(lc fx.Lifecycle, cfg config.Config, m *metrics.Metrics, checker *health.Checker, logger *slog.Logger) *AdminServer {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{Registry: m.Registry}))
	mux.Handle("GET /health/live", checker.LiveHandler())
	mux.Handle("GET /health/ready", checker.ReadyHandler())

	server := httpserver.New(httpserver.Settings{
		Name: "admin", Addr: cfg.Admin.Addr,
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout, ReadTimeout: cfg.HTTP.ReadTimeout,
		WriteTimeout: cfg.HTTP.WriteTimeout, IdleTimeout: cfg.HTTP.IdleTimeout,
	}, mux, logger)
	lc.Append(fx.StartStopHook(server.Start, server.Stop))
	return &AdminServer{Server: server}
}
