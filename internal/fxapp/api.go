package fxapp

import (
	"log/slog"

	"go.uber.org/fx"

	"github.com/danfigueroa/backend-challenge-go/internal/adapter/auth"
	"github.com/danfigueroa/backend-challenge-go/internal/adapter/httpapi"
	"github.com/danfigueroa/backend-challenge-go/internal/app/wageringapp"
	"github.com/danfigueroa/backend-challenge-go/internal/app/walletapp"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/config"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/health"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/httpserver"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/metrics"
)

type APIServer struct {
	*httpserver.Server
}

var APIModule = fx.Module("api",
	fx.Provide(
		NewTokenVerifier,
		NewAPIServer,
	),
	fx.Invoke(func(*APIServer) {}),
)

func NewTokenVerifier(cfg config.Config) (*auth.Verifier, error) {
	return auth.NewVerifier(auth.Settings{Issuer: cfg.Auth.Issuer, JWKSURL: cfg.Auth.JWKSURL, Audience: cfg.Auth.Audience})
}

type apiDeps struct {
	fx.In

	Lifecycle fx.Lifecycle
	Config    config.Config
	Wallets   *walletapp.Service
	Wagering  *wageringapp.Service
	Verifier  *auth.Verifier
	Health    *health.Checker
	Metrics   *metrics.Metrics
	Logger    *slog.Logger
}

func NewAPIServer(d apiDeps) (*APIServer, error) {
	handler, err := httpapi.NewHandler(httpapi.Deps{
		Settings: httpapi.Settings{RequestTimeout: d.Config.HTTP.RequestTimeout, MaxBodyBytes: d.Config.HTTP.MaxBodyBytes},
		Wallets:  d.Wallets, Wagering: d.Wagering, Verifier: d.Verifier,
		Health: d.Health, Metrics: d.Metrics, Logger: d.Logger,
	})
	if err != nil {
		return nil, err
	}
	server := httpserver.New(httpserver.Settings{
		Name: "api", Addr: d.Config.HTTP.Addr,
		ReadHeaderTimeout: d.Config.HTTP.ReadHeaderTimeout, ReadTimeout: d.Config.HTTP.ReadTimeout,
		WriteTimeout: d.Config.HTTP.WriteTimeout, IdleTimeout: d.Config.HTTP.IdleTimeout,
	}, handler, d.Logger)
	d.Lifecycle.Append(fx.StartStopHook(server.Start, server.Stop))
	return &APIServer{Server: server}, nil
}
