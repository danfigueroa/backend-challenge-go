//go:build integration

package fxapp_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/goleak"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/app/wageringapp"
	"github.com/danfigueroa/backend-challenge-go/internal/app/walletapp"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
	"github.com/danfigueroa/backend-challenge-go/internal/fxapp"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/config"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/health"
	"github.com/danfigueroa/backend-challenge-go/internal/testsupport/pgtest"
)

var pg *pgtest.Instance

func TestMain(m *testing.M) {
	os.Exit(pgtest.RunMain(m, &pg))
}

func integrationConfig(t *testing.T, databaseURL string) config.Config {
	t.Setenv("DATABASE_URL", databaseURL)
	t.Setenv("APP_INSTANCE_ID", "fx-integration")
	t.Setenv("HTTP_ADDR", "localhost:0")
	t.Setenv("ADMIN_ADDR", "127.0.0.1:0")
	t.Setenv("PENDING_POLL_INTERVAL", "50ms")
	t.Setenv("PENDING_ERROR_BACKOFF", "200ms")
	t.Setenv("PENDING_ITERATION_TIMEOUT", "5s")
	t.Setenv("APP_SHUTDOWN_TIMEOUT", "10s")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func quietLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func getJSON(t *testing.T, client *http.Client, url string, into any) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if into != nil {
		if err := json.Unmarshal(body, into); err != nil {
			if s, ok := into.(*string); ok {
				*s = string(body)
				return resp.StatusCode
			}
			t.Fatalf("decode %s: %v (%s)", url, err, body)
		}
	}
	return resp.StatusCode
}

func TestApplicationStartsServesAndStopsCleanly(t *testing.T) {
	db := pg.NewDatabase(t)
	cfg := integrationConfig(t, db.AppDSN)

	leaks := goleak.IgnoreCurrent()

	var (
		admin       *fxapp.AdminServer
		pool        *pgxpool.Pool
		checker     *health.Checker
		wallets     *walletapp.Service
		wageringSvc *wageringapp.Service
	)
	application := fx.New(
		fxapp.Options(cfg, fx.Decorate(quietLogger)),
		fx.Populate(&admin, &pool, &checker, &wallets, &wageringSvc),
		fx.NopLogger,
	)
	if err := application.Err(); err != nil {
		t.Fatal(err)
	}

	startCtx, cancelStart := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelStart()
	if err := application.Start(startCtx); err != nil {
		t.Fatalf("start: %v", err)
	}

	transport := &http.Transport{DisableKeepAlives: true}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	base := "http://" + admin.Addr()

	var live health.Report
	if code := getJSON(t, client, base+"/health/live", &live); code != http.StatusOK || live.Status != health.StatusUp {
		t.Errorf("live = %d %+v", code, live)
	}
	var ready health.Report
	if code := getJSON(t, client, base+"/health/ready", &ready); code != http.StatusOK || ready.Checks["postgres"].Status != health.StatusUp {
		t.Errorf("ready = %d %+v", code, ready)
	}

	ctx := context.Background()
	internal := app.ServiceActor("fx-test")
	w, err := wallets.OpenWallet(ctx, walletapp.OpenWalletCommand{Actor: internal, PlayerID: "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1", Amount: "100.00", Currency: "BRL"})
	if err != nil {
		t.Fatal(err)
	}
	provider := app.ProviderActor("provider-a", "provider-a")
	input := func(kind, id, reference string) wagering.RequestInput {
		return wagering.RequestInput{
			ProviderID: "provider-a", ExternalTransactionID: id, IdempotencyKey: "provider-a:" + id,
			PlayerID: w.PlayerID.String(), WalletID: w.ID.String(), RoundID: "r", GameID: "g",
			Kind: kind, Amount: "10.00", Currency: "BRL", ReferenceExternalTransactionID: reference,
		}
	}
	refund, err := wageringSvc.Process(ctx, wageringapp.ProcessCommand{Actor: provider, Input: input("REFUND", "refund-1", "bet-1")})
	if err != nil || refund.Transaction.Status() != wagering.StatusPendingReference {
		t.Fatalf("refund = %+v, %v", refund, err)
	}
	if _, err := wageringSvc.Process(ctx, wageringapp.ProcessCommand{Actor: provider, Input: input("BET", "bet-1", "")}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		tx, err := wageringSvc.GetByExternalID(ctx, provider, "provider-a", "refund-1")
		if err == nil && tx.Status() == wagering.StatusProcessed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background worker did not resolve the pending refund: %v %v", tx.Status(), err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	var metricsBody string
	getJSON(t, client, base+"/metrics", &metricsBody)
	for _, series := range []string{
		`wallet_wagering_transactions_total{channel="WORKER",failure_code="",kind="REFUND",status="PROCESSED"} 1`,
		"wallet_outbox_pending_events",
		"wallet_outbox_lag_seconds",
		"go_goroutines",
	} {
		if !strings.Contains(metricsBody, series) {
			t.Errorf("metrics missing %q", series)
		}
	}
	transport.CloseIdleConnections()

	stopCtx, cancelStop := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelStop()
	if err := application.Stop(stopCtx); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if !checker.Draining() {
		t.Error("readiness was not switched to draining during shutdown")
	}
	if err := pool.Ping(context.Background()); err == nil {
		t.Error("database pool still usable after shutdown")
	}
	afterStop, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/health/live", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := client.Do(afterStop); err == nil {
		resp.Body.Close()
		t.Error("admin server still accepting connections after shutdown")
	}
	transport.CloseIdleConnections()

	goleak.VerifyNone(t, leaks)
}

func TestApplicationFailsToStartWithoutDatabase(t *testing.T) {
	cfg := integrationConfig(t, "postgres://nobody:nothing@127.0.0.1:1/void?connect_timeout=1")
	application := fx.New(
		fxapp.Options(cfg, fx.Decorate(quietLogger)),
		fx.NopLogger,
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := application.Start(ctx); err == nil {
		_ = application.Stop(context.Background())
		t.Fatal("application started without a reachable database")
	}
}
