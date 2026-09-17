//go:build integration

package fxapp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/goleak"
	"golang.org/x/sync/errgroup"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/app/wageringapp"
	"github.com/danfigueroa/backend-challenge-go/internal/app/walletapp"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
	"github.com/danfigueroa/backend-challenge-go/internal/fxapp"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/config"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/health"
	"github.com/danfigueroa/backend-challenge-go/internal/testsupport/lstest"
	"github.com/danfigueroa/backend-challenge-go/internal/testsupport/pgtest"
)

var (
	pg *pgtest.Instance
	ls *lstest.Instance
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	var g errgroup.Group
	g.Go(func() (err error) { pg, err = pgtest.Start(ctx); return err })
	g.Go(func() (err error) { ls, err = lstest.Start(ctx); return err })
	err := g.Wait()
	defer func() {
		stop, cancelStop := context.WithTimeout(context.Background(), time.Minute)
		defer cancelStop()
		if pg != nil {
			_ = pg.Terminate(stop)
		}
		if ls != nil {
			_ = ls.Terminate(stop)
		}
	}()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return m.Run()
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
	t.Setenv("AUTH_ISSUER", "http://keycloak.test/realms/wagering")
	t.Setenv("AUTH_JWKS_URL", "http://127.0.0.1:1/certs")
	t.Setenv("APP_ROLES", "api,pendingref")
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

func TestAllRolesProcessSQSMessagesAndPublishEvents(t *testing.T) {
	db := pg.NewDatabase(t)
	integrationConfig(t, db.AppDSN)
	t.Setenv("APP_ROLES", "api,consumer,outbox,pendingref")
	t.Setenv("AWS_ENDPOINT_URL", ls.Endpoint)
	t.Setenv("SQS_CONSUMER_ACCESS_KEY_ID", "wager-transactions-consumer")
	t.Setenv("SQS_CONSUMER_SECRET_ACCESS_KEY", "local-consumer-secret")
	t.Setenv("SNS_PUBLISHER_ACCESS_KEY_ID", "wallet-events-publisher")
	t.Setenv("SNS_PUBLISHER_SECRET_ACCESS_KEY", "local-publisher-secret")
	t.Setenv("SQS_INPUT_QUEUE_URL", ls.QueueURL("wager-transactions.fifo"))
	t.Setenv("SQS_DLQ_URL", ls.QueueURL("wager-transactions-dlq.fifo"))
	t.Setenv("SNS_EVENTS_TOPIC_ARN", "arn:aws:sns:us-east-1:000000000000:wallet-events.fifo")
	t.Setenv("SQS_WAIT_TIME", "1s")
	t.Setenv("OUTBOX_POLL_INTERVAL", "100ms")
	t.Setenv("SQS_PROCESSING_TIMEOUT", "5s")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	leaks := goleak.IgnoreCurrent()
	var (
		admin   *fxapp.AdminServer
		wallets *walletapp.Service
	)
	application := fx.New(
		fxapp.Options(cfg, fx.Decorate(quietLogger)),
		fx.Populate(&admin, &wallets),
		fx.NopLogger,
	)
	startCtx, cancelStart := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelStart()
	if err := application.Start(startCtx); err != nil {
		t.Fatalf("start: %v", err)
	}

	transport := &http.Transport{DisableKeepAlives: true}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	var ready health.Report
	if code := getJSON(t, client, "http://"+admin.Addr()+"/health/ready", &ready); code != http.StatusOK ||
		ready.Checks["sqs"].Status != health.StatusUp || ready.Checks["sns"].Status != health.StatusUp || ready.Checks["postgres"].Status != health.StatusUp {
		t.Errorf("ready = %d %+v", code, ready)
	}

	ctx := context.Background()
	w, err := wallets.OpenWallet(ctx, walletapp.OpenWalletCommand{Actor: app.ServiceActor("fx"), PlayerID: "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1", Amount: "1000.00", Currency: "BRL"})
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"messageId":"msg-e2e","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00.000Z","data":{"providerId":"provider-a","externalTransactionId":"transaction-e2e","idempotencyKey":"provider-a:transaction-e2e","playerId":%q,"walletId":%q,"roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}}`,
		w.PlayerID.String(), w.ID.String())
	ls.Send(t, ls.QueueURL("wager-transactions.fifo"), w.ID.String(), "msg-e2e", body, nil)

	messages := ls.Drain(t, ls.QueueURL("wallet-events-audit.fifo"), 4, 30*time.Second)
	types := map[string]int{}
	for _, m := range messages {
		var e struct {
			EventType string `json:"eventType"`
		}
		if err := json.Unmarshal([]byte(aws.ToString(m.Body)), &e); err != nil {
			t.Fatal(err)
		}
		types[e.EventType]++
	}
	if types["WagerTransactionProcessed"] != 2 || types["WalletBalanceChanged"] != 2 {
		t.Errorf("events delivered to subscribers = %v, want opening and bet events", types)
	}
	view, err := wallets.GetWallet(ctx, app.ServiceActor("fx"), w.ID)
	if err != nil || view.Balance.Amount() != "975.00" {
		t.Errorf("wallet after SQS message = %+v, %v", view, err)
	}

	var metricsBody string
	getJSON(t, client, "http://"+admin.Addr()+"/metrics", &metricsBody)
	for _, series := range []string{
		`wallet_sqs_messages_total{queue="wager-transactions-consumer",result="PROCESSED"} 1`,
		`wallet_outbox_publish_attempts_total{event_type="WalletBalanceChanged",result="published"} 2`,
		`wallet_outbox_pending_events 0`,
	} {
		if !strings.Contains(metricsBody, series) {
			t.Errorf("metrics missing %q", series)
		}
	}
	transport.CloseIdleConnections()

	stopCtx, cancelStop := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelStop()
	if err := application.Stop(stopCtx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	transport.CloseIdleConnections()
	ls.CloseIdleConnections()
	goleak.VerifyNone(t, leaks)
}
