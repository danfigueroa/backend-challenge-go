package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"go.uber.org/fx"

	"github.com/danfigueroa/backend-challenge-go/internal/adapter/postgres"
	"github.com/danfigueroa/backend-challenge-go/internal/fxapp"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/config"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/logging"
)

const usage = `usage: wallet <command>

commands:
  serve                 run the service (default)
  migrate up            apply all pending migrations
  migrate down [N]      revert the last N migrations (default 1)
  migrate version       print the current migration version
  healthcheck           probe the local admin liveness endpoint (container healthcheck)
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	command := "serve"
	if len(args) > 0 {
		command, args = args[0], args[1:]
	}
	switch command {
	case "serve":
		return serve(stderr)
	case "migrate":
		return migrate(args, stdout, stderr)
	case "healthcheck":
		return healthcheck(stderr)
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n\n%s", command, usage)
		return 2
	}
}

func serve(stderr io.Writer) int {
	cfg, err := config.Load()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	logger := logging.New(os.Stdout, cfg.App.SlogLevel(), cfg.App.Name, cfg.App.InstanceID)

	application := fx.New(fxapp.Options(cfg))
	if err := application.Err(); err != nil {
		logger.Error("invalid application graph", slog.Any("error", err))
		return 1
	}

	startCtx, cancelStart := context.WithTimeout(context.Background(), cfg.App.StartTimeout)
	defer cancelStart()
	if err := application.Start(startCtx); err != nil {
		logger.Error("application failed to start", slog.Any("error", err))
		return 1
	}
	logger.Info("application started", slog.Any("roles", cfg.App.Roles))

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	exitCode := 0
	select {
	case sig := <-signals:
		logger.Info("shutdown signal received", slog.String("signal", sig.String()))
	case shutdown := <-application.Wait():
		exitCode = shutdown.ExitCode
		logger.Info("application requested shutdown", slog.Int("exitCode", exitCode))
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), cfg.App.ShutdownTimeout)
	defer cancelStop()
	if err := application.Stop(stopCtx); err != nil {
		logger.Error("graceful shutdown incomplete", slog.Any("error", err))
		return 1
	}
	logger.Info("application stopped")
	return exitCode
}

func migrate(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	dsn := os.Getenv("DATABASE_MIGRATIONS_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		_, _ = fmt.Fprintln(stderr, "DATABASE_MIGRATIONS_URL or DATABASE_URL is required")
		return 1
	}

	migrator, err := postgres.NewMigrator(dsn)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	defer func() { _ = migrator.Close() }()

	switch args[0] {
	case "up":
		err = migrator.Up()
	case "down":
		steps := 1
		if len(args) > 1 {
			if steps, err = strconv.Atoi(args[1]); err != nil {
				_, _ = fmt.Fprintf(stderr, "invalid step count %q\n", args[1])
				return 2
			}
		}
		err = migrator.Down(steps)
	case "version":
	default:
		_, _ = fmt.Fprintf(stderr, "unknown migrate command %q\n\n%s", args[0], usage)
		return 2
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}

	status, err := migrator.Status()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	switch {
	case status.Empty:
		_, _ = fmt.Fprintln(stdout, "migrations: none applied")
	default:
		_, _ = fmt.Fprintf(stdout, "migrations: version %d (dirty=%t)\n", status.Version, status.Dirty)
	}
	if status.Dirty {
		return 1
	}
	return 0
}

func healthcheck(stderr io.Writer) int {
	addr := os.Getenv("ADMIN_ADDR")
	if addr == "" {
		addr = ":9090"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/health/live", nil)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintln(stderr, errors.New("liveness probe returned "+resp.Status))
		return 1
	}
	return 0
}
