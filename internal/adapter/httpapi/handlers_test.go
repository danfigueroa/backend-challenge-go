package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/danfigueroa/backend-challenge-go/internal/adapter/auth"
	"github.com/danfigueroa/backend-challenge-go/internal/adapter/httpapi"
	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/app/wageringapp"
	"github.com/danfigueroa/backend-challenge-go/internal/app/walletapp"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/money"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wallet"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/health"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/metrics"
)

var (
	t0       = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	playerID = uuid.MustParse("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")
	walletID = uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37")
)

type fakeVerifier map[string]auth.Principal

func (f fakeVerifier) Verify(_ context.Context, raw string) (auth.Principal, error) {
	switch raw {
	case "":
		return auth.Principal{}, auth.ErrMissingToken
	case "expired":
		return auth.Principal{}, auth.ErrExpiredToken
	case "idp-down":
		return auth.Principal{}, auth.ErrIDPUnavailable
	}
	p, ok := f[raw]
	if !ok {
		return auth.Principal{}, auth.ErrInvalidToken
	}
	return p, nil
}

var tokens = fakeVerifier{
	"provider-a": {Subject: "sa-a", ClientID: "provider-a", ProviderID: "provider-a", Permissions: []string{auth.PermissionTransactionsWrite, auth.PermissionTransactionsRead}},
	"internal":   {Subject: "sa-i", ClientID: "wallet-internal", Permissions: []string{auth.PermissionWalletsWrite, auth.PermissionWalletsRead, auth.PermissionWalletsReconcile, auth.PermissionTransactionsRead}},
	"no-perms":   {Subject: "sa-n", ClientID: "nobody", ProviderID: "provider-a"},
}

type fakeWallets struct {
	open      func(walletapp.OpenWalletCommand) (walletapp.WalletView, error)
	get       func(app.Actor, uuid.UUID) (walletapp.WalletView, error)
	ledger    func(walletapp.LedgerQuery) (walletapp.LedgerPage, error)
	reconcile func(app.Actor, uuid.UUID) (walletapp.ReconciliationReport, error)
}

func (f fakeWallets) OpenWallet(_ context.Context, c walletapp.OpenWalletCommand) (walletapp.WalletView, error) {
	return f.open(c)
}

func (f fakeWallets) GetWallet(_ context.Context, a app.Actor, id uuid.UUID) (walletapp.WalletView, error) {
	return f.get(a, id)
}

func (f fakeWallets) ListLedger(_ context.Context, q walletapp.LedgerQuery) (walletapp.LedgerPage, error) {
	return f.ledger(q)
}

func (f fakeWallets) Reconcile(_ context.Context, a app.Actor, id uuid.UUID) (walletapp.ReconciliationReport, error) {
	return f.reconcile(a, id)
}

type fakeWagering struct {
	process func(wageringapp.ProcessCommand) (wageringapp.ProcessResult, error)
	get     func(app.Actor, uuid.UUID) (*wagering.Transaction, error)
	byExt   func(app.Actor, string, string) (*wagering.Transaction, error)
}

func (f fakeWagering) Process(_ context.Context, c wageringapp.ProcessCommand) (wageringapp.ProcessResult, error) {
	return f.process(c)
}

func (f fakeWagering) GetTransaction(_ context.Context, a app.Actor, id uuid.UUID) (*wagering.Transaction, error) {
	return f.get(a, id)
}

func (f fakeWagering) GetByExternalID(_ context.Context, a app.Actor, p, e string) (*wagering.Transaction, error) {
	return f.byExt(a, p, e)
}

type server struct {
	handler http.Handler
	metrics *metrics.Metrics
}

func newServer(t *testing.T, wallets fakeWallets, wagers fakeWagering) server {
	t.Helper()
	m := metrics.New()
	checker := health.NewChecker(time.Second, 0)
	checker.Register("postgres", func(context.Context) error { return nil })
	h, err := httpapi.NewHandler(httpapi.Deps{
		Settings: httpapi.Settings{RequestTimeout: time.Second, MaxBodyBytes: 1024},
		Wallets:  wallets, Wagering: wagers, Verifier: tokens, Health: checker, Metrics: m,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return server{handler: h, metrics: m}
}

type response struct {
	code   int
	header http.Header
	body   map[string]any
	raw    string
}

func (s server) do(t *testing.T, method, path, token, body string, headers map[string]string) response {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	res := response{code: rec.Code, header: rec.Header(), raw: rec.Body.String()}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &res.body); err != nil {
			t.Fatalf("response is not JSON: %q", rec.Body.String())
		}
	}
	return res
}

func (r response) expectProblem(t *testing.T, status int, code string) {
	t.Helper()
	if r.code != status || r.body["code"] != code {
		t.Fatalf("got %d %v, want %d %s (%s)", r.code, r.body["code"], status, code, r.raw)
	}
	if ct := r.header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("content type = %q", ct)
	}
	if r.body["status"] != float64(status) || r.body["correlationId"] == "" || r.body["type"] == "" {
		t.Errorf("incomplete problem: %s", r.raw)
	}
}

func brl(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.Parse(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

const betBody = `{"providerId":"provider-a","externalTransactionId":"transaction-123","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37","roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}`

func transaction(t *testing.T, status wagering.Status) *wagering.Transaction {
	t.Helper()
	in := wagering.RequestInput{
		ProviderID: "provider-a", ExternalTransactionID: "transaction-123", IdempotencyKey: "provider-a:transaction-123",
		PlayerID: playerID.String(), WalletID: walletID.String(), RoundID: "round-987", GameID: "fortune-chimp",
		Kind: "REFUND", Amount: "25.00", Currency: "BRL", ReferenceExternalTransactionID: "bet-1",
	}
	req, err := wagering.NewRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := wagering.NewExternal(uuid.MustParse("0192f298-345e-7e38-af88-e43f851a819d"), req, t0)
	if err != nil {
		t.Fatal(err)
	}
	switch status {
	case wagering.StatusProcessed:
		err = tx.MarkProcessed(wagering.Result{Balance: brl(t, "975.00"), WalletVersion: 2}, uuid.New(), t0)
	case wagering.StatusRejected:
		err = tx.Reject(wagering.Rejection{Code: wagering.CodeReferenceNotFound, ObservedBalance: brl(t, "1000.00"), ObservedWalletVersion: 1}, t0)
	case wagering.StatusPendingReference:
		err = tx.AwaitReference(t0.Add(time.Second), t0.Add(30*time.Minute), t0)
	case wagering.StatusPending, wagering.StatusFailed:
	}
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestProcessTransactionResponses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status wagering.Status
		replay bool
		code   int
	}{
		{"processed", wagering.StatusProcessed, false, http.StatusOK},
		{"processed replay", wagering.StatusProcessed, true, http.StatusOK},
		{"pending reference", wagering.StatusPendingReference, false, http.StatusAccepted},
		{"rejected", wagering.StatusRejected, false, http.StatusUnprocessableEntity},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var captured wageringapp.ProcessCommand
			s := newServer(t, fakeWallets{}, fakeWagering{process: func(c wageringapp.ProcessCommand) (wageringapp.ProcessResult, error) {
				captured = c
				return wageringapp.ProcessResult{Transaction: transaction(t, tc.status), IdempotentReplay: tc.replay}, nil
			}})
			res := s.do(t, http.MethodPost, "/wagering/transactions", "provider-a", betBody, map[string]string{
				"Idempotency-Key": "provider-a:transaction-123", "X-Correlation-Id": "corr-42",
			})

			if res.code != tc.code || res.body["status"] != string(tc.status) || res.body["idempotentReplay"] != tc.replay ||
				res.body["transactionId"] != "0192f298-345e-7e38-af88-e43f851a819d" {
				t.Fatalf("response = %d %s", res.code, res.raw)
			}
			if res.header.Get("X-Correlation-Id") != "corr-42" || captured.Meta.CorrelationID != "corr-42" || captured.Meta.Channel != app.ChannelHTTP {
				t.Errorf("correlation not propagated: header %q meta %+v", res.header.Get("X-Correlation-Id"), captured.Meta)
			}
			if captured.Input.IdempotencyKey != "provider-a:transaction-123" || captured.Input.Amount != "25.00" ||
				captured.Actor.Kind != app.ActorProvider || captured.Actor.ProviderID != "provider-a" {
				t.Errorf("command = %+v", captured)
			}

			switch tc.status {
			case wagering.StatusProcessed:
				if b := res.body["balance"].(map[string]any); b["amount"] != "975.00" || b["currency"] != "BRL" {
					t.Errorf("balance = %v", b)
				}
			case wagering.StatusRejected:
				if res.body["failureCode"] != "REFERENCE_NOT_FOUND" {
					t.Errorf("failure code = %v", res.body["failureCode"])
				}
			case wagering.StatusPendingReference:
				if res.header.Get("Location") == "" || res.body["expiresAt"] != "2026-09-08T12:30:00.000Z" || res.body["balance"] != nil {
					t.Errorf("pending response = %s (location %q)", res.raw, res.header.Get("Location"))
				}
			case wagering.StatusPending, wagering.StatusFailed:
			}
		})
	}
}

func TestProcessTransactionErrors(t *testing.T) {
	t.Parallel()

	failWith := func(err error) fakeWagering {
		return fakeWagering{process: func(wageringapp.ProcessCommand) (wageringapp.ProcessResult, error) {
			return wageringapp.ProcessResult{}, err
		}}
	}
	key := map[string]string{"Idempotency-Key": "k"}

	tests := []struct {
		name    string
		svc     fakeWagering
		token   string
		body    string
		headers map[string]string
		status  int
		code    string
	}{
		{"missing token", failWith(nil), "", betBody, key, 401, httpapi.CodeUnauthenticated},
		{"invalid token", failWith(nil), "forged", betBody, key, 401, httpapi.CodeInvalidToken},
		{"expired token", failWith(nil), "expired", betBody, key, 401, httpapi.CodeTokenExpired},
		{"idp unavailable", failWith(nil), "idp-down", betBody, key, 503, httpapi.CodeServiceUnavailable},
		{"no permission", failWith(nil), "no-perms", betBody, key, 403, httpapi.CodeInsufficientPermissions},
		{"wallet token cannot post transactions", failWith(nil), "internal", betBody, key, 403, httpapi.CodeInsufficientPermissions},
		{"missing idempotency key", failWith(nil), "provider-a", betBody, nil, 400, "MISSING_FIELD"},
		{"not json content type", failWith(nil), "provider-a", betBody, map[string]string{"Idempotency-Key": "k", "Content-Type": "text/plain"}, 415, httpapi.CodeUnsupportedMediaType},
		{"unknown field", failWith(nil), "provider-a", strings.Replace(betBody, `"kind"`, `"unexpectedField":"1","kind"`, 1), key, 400, "MALFORMED_REQUEST"},
		{"number amount", failWith(nil), "provider-a", strings.Replace(betBody, `"amount":"25.00"`, `"amount":25.00`, 1), key, 400, "MALFORMED_REQUEST"},
		{"trailing data", failWith(nil), "provider-a", betBody + `{}`, key, 400, "MALFORMED_REQUEST"},
		{"array body", failWith(nil), "provider-a", `[` + betBody + `]`, key, 400, "MALFORMED_REQUEST"},
		{"body too large", failWith(nil), "provider-a", `{"providerId":"` + strings.Repeat("x", 2000) + `"}`, key, 413, httpapi.CodePayloadTooLarge},
		{"domain validation", failWith(wagering.NewValidationError(wagering.CodeOpeningNotAllowed, "kind", "reserved")), "provider-a", betBody, key, 400, "OPENING_NOT_ALLOWED"},
		{"wallet not found", failWith(wagering.NewValidationError(wagering.CodeWalletNotFound, "walletId", "missing")), "provider-a", betBody, key, 400, "WALLET_NOT_FOUND"},
		{"forbidden provider", failWith(fmt.Errorf("%w: other provider", app.ErrForbidden)), "provider-a", betBody, key, 403, httpapi.CodeForbidden},
		{"idempotency conflict", failWith(&wageringapp.IdempotencyConflictError{Code: wagering.CodeIdempotencyKeyConflict, ExistingTransactionID: uuid.New()}), "provider-a", betBody, key, 409, "IDEMPOTENCY_KEY_CONFLICT"},
		{"external conflict", failWith(&wageringapp.IdempotencyConflictError{Code: wagering.CodeExternalTransactionConflict}), "provider-a", betBody, key, 409, "EXTERNAL_TRANSACTION_CONFLICT"},
		{"transient", failWith(fmt.Errorf("%w: db", app.ErrTransient)), "provider-a", betBody, key, 503, httpapi.CodeServiceUnavailable},
		{"timeout", failWith(context.DeadlineExceeded), "provider-a", betBody, key, 503, httpapi.CodeServiceUnavailable},
		{"unexpected", failWith(errors.New("boom")), "provider-a", betBody, key, 500, httpapi.CodeInternalError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			called := false
			svc := tc.svc
			inner := svc.process
			svc.process = func(c wageringapp.ProcessCommand) (wageringapp.ProcessResult, error) {
				called = true
				return inner(c)
			}
			res := newServer(t, fakeWallets{}, svc).do(t, http.MethodPost, "/wagering/transactions", tc.token, tc.body, tc.headers)
			res.expectProblem(t, tc.status, tc.code)

			if tc.status == 401 && !strings.HasPrefix(res.header.Get("WWW-Authenticate"), "Bearer") {
				t.Error("401 without WWW-Authenticate")
			}
			if tc.status == 503 && res.header.Get("Retry-After") == "" {
				t.Error("503 without Retry-After")
			}
			if (tc.status == 401 || tc.status == 403 && tc.code == httpapi.CodeInsufficientPermissions || tc.status == 415 || tc.status == 413) && called {
				t.Error("use case was invoked for a request rejected at the edge")
			}
			if tc.code == "IDEMPOTENCY_KEY_CONFLICT" && res.body["existingTransactionId"] == "" {
				t.Error("conflict without existing transaction id")
			}
		})
	}
}

func TestWalletEndpoints(t *testing.T) {
	t.Parallel()

	view := walletapp.WalletView{ID: walletID, PlayerID: playerID, Balance: brl(t, "1000.00"), Version: 1, CreatedAt: t0, UpdatedAt: t0}
	entry, err := wallet.RehydrateLedgerEntry(wallet.LedgerEntryParams{
		ID: uuid.New(), WalletID: walletID, TransactionID: uuid.New(), Direction: wallet.DirectionCredit,
		Amount: brl(t, "1000.00"), BalanceBefore: brl(t, "0.00"), BalanceAfter: brl(t, "1000.00"), WalletVersion: 1, CreatedAt: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	var (
		openCmd     walletapp.OpenWalletCommand
		ledgerQuery walletapp.LedgerQuery
	)
	s := newServer(t, fakeWallets{
		open: func(c walletapp.OpenWalletCommand) (walletapp.WalletView, error) {
			openCmd = c
			if c.PlayerID == "duplicate" {
				return walletapp.WalletView{}, &app.ConflictError{Kind: app.ConflictWalletExists}
			}
			return view, nil
		},
		get: func(_ app.Actor, id uuid.UUID) (walletapp.WalletView, error) {
			if id != walletID {
				return walletapp.WalletView{}, app.ErrNotFound
			}
			return view, nil
		},
		ledger: func(q walletapp.LedgerQuery) (walletapp.LedgerPage, error) {
			ledgerQuery = q
			return walletapp.LedgerPage{Entries: []wallet.LedgerEntry{entry}, NextCursor: "next"}, nil
		},
		reconcile: func(app.Actor, uuid.UUID) (walletapp.ReconciliationReport, error) {
			return walletapp.ReconciliationReport{
				WalletID: walletID, StoredBalance: brl(t, "975.00"), CalculatedBalance: brl(t, "975.00"),
				Difference: brl(t, "0.00"), Consistent: true, CheckedEntries: 2, CheckedAt: t0,
			}, nil
		},
	}, fakeWagering{})

	created := s.do(t, http.MethodPost, "/wallets", "internal", `{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"1000.00","currency":"BRL"}}`, nil)
	if created.code != http.StatusCreated || created.body["version"] != float64(1) || created.header.Get("Location") != "/wallets/"+walletID.String() ||
		created.body["balance"].(map[string]any)["amount"] != "1000.00" || openCmd.Amount != "1000.00" || openCmd.Actor.Kind != app.ActorService {
		t.Errorf("create = %d %s, cmd %+v", created.code, created.raw, openCmd)
	}
	s.do(t, http.MethodPost, "/wallets", "internal", `{"playerId":"duplicate","initialBalance":{"amount":"1.00","currency":"BRL"}}`, nil).
		expectProblem(t, http.StatusConflict, httpapi.CodeWalletAlreadyExists)
	s.do(t, http.MethodPost, "/wallets", "provider-a", `{"playerId":"x"}`, nil).
		expectProblem(t, http.StatusForbidden, httpapi.CodeInsufficientPermissions)

	if got := s.do(t, http.MethodGet, "/wallets/"+walletID.String(), "internal", "", nil); got.code != http.StatusOK || got.body["id"] != walletID.String() {
		t.Errorf("get = %d %s", got.code, got.raw)
	}
	s.do(t, http.MethodGet, "/wallets/"+uuid.NewString(), "internal", "", nil).expectProblem(t, http.StatusNotFound, httpapi.CodeNotFound)
	s.do(t, http.MethodGet, "/wallets/not-a-uuid", "internal", "", nil).expectProblem(t, http.StatusBadRequest, "INVALID_FIELD")
	s.do(t, http.MethodGet, "/wallets/"+walletID.String(), "provider-a", "", nil).expectProblem(t, http.StatusForbidden, httpapi.CodeInsufficientPermissions)

	page := s.do(t, http.MethodGet, "/wallets/"+walletID.String()+"/ledger?cursor=abc&limit=10", "internal", "", nil)
	if page.code != http.StatusOK || page.body["nextCursor"] != "next" || len(page.body["entries"].([]any)) != 1 ||
		ledgerQuery.Cursor != "abc" || ledgerQuery.Limit != 10 {
		t.Errorf("ledger = %d %s, query %+v", page.code, page.raw, ledgerQuery)
	}
	s.do(t, http.MethodGet, "/wallets/"+walletID.String()+"/ledger?limit=ten", "internal", "", nil).expectProblem(t, http.StatusBadRequest, "INVALID_FIELD")

	rec := s.do(t, http.MethodPost, "/wallets/"+walletID.String()+"/reconciliation", "internal", "", nil)
	if rec.code != http.StatusOK || rec.body["consistent"] != true || rec.body["checkedEntries"] != float64(2) ||
		rec.body["difference"].(map[string]any)["amount"] != "0.00" {
		t.Errorf("reconciliation = %d %s", rec.code, rec.raw)
	}
}

func TestTransactionQueries(t *testing.T) {
	t.Parallel()

	var seen []app.Actor
	s := newServer(t, fakeWallets{}, fakeWagering{
		get: func(a app.Actor, id uuid.UUID) (*wagering.Transaction, error) {
			seen = append(seen, a)
			if id == uuid.Nil {
				return nil, app.ErrNotFound
			}
			return transaction(t, wagering.StatusProcessed), nil
		},
		byExt: func(a app.Actor, provider, _ string) (*wagering.Transaction, error) {
			seen = append(seen, a)
			if provider != "provider-a" {
				return nil, app.ErrForbidden
			}
			return transaction(t, wagering.StatusRejected), nil
		},
	})

	got := s.do(t, http.MethodGet, "/wagering/transactions/0192f298-345e-7e38-af88-e43f851a819d", "provider-a", "", nil)
	if got.code != http.StatusOK || got.body["kind"] != "REFUND" || got.body["referenceExternalTransactionId"] != "bet-1" ||
		got.body["walletVersion"] != float64(2) || got.body["origin"] != "EXTERNAL" {
		t.Errorf("get = %d %s", got.code, got.raw)
	}
	byExt := s.do(t, http.MethodGet, "/providers/provider-a/wagering/transactions/transaction-123", "provider-a", "", nil)
	if byExt.code != http.StatusOK || byExt.body["failureCode"] != "REFERENCE_NOT_FOUND" || byExt.body["failureCategory"] != "DEFINITIVE" {
		t.Errorf("by external = %d %s", byExt.code, byExt.raw)
	}
	s.do(t, http.MethodGet, "/providers/provider-b/wagering/transactions/transaction-123", "provider-a", "", nil).
		expectProblem(t, http.StatusForbidden, httpapi.CodeForbidden)
	s.do(t, http.MethodGet, "/wagering/transactions/00000000-0000-0000-0000-000000000000", "provider-a", "", nil).
		expectProblem(t, http.StatusBadRequest, "INVALID_FIELD")
	if len(seen) == 0 || seen[0].Kind != app.ActorProvider {
		t.Errorf("actors = %+v", seen)
	}
}

func TestPublicHealthAndRoutingAndRecovery(t *testing.T) {
	t.Parallel()

	s := newServer(t, fakeWallets{}, fakeWagering{process: func(wageringapp.ProcessCommand) (wageringapp.ProcessResult, error) {
		panic("unexpected")
	}})

	if live := s.do(t, http.MethodGet, "/health/live", "", "", nil); live.code != http.StatusOK || live.body["status"] != "UP" {
		t.Errorf("live = %d %s", live.code, live.raw)
	}
	if ready := s.do(t, http.MethodGet, "/health/ready", "", "", nil); ready.code != http.StatusOK {
		t.Errorf("ready = %d %s", ready.code, ready.raw)
	}
	s.do(t, http.MethodGet, "/nope", "", "", nil).expectProblem(t, http.StatusNotFound, httpapi.CodeNotFound)

	panicked := s.do(t, http.MethodPost, "/wagering/transactions", "provider-a", betBody, map[string]string{"Idempotency-Key": "k", "X-Correlation-Id": "bad id with spaces"})
	panicked.expectProblem(t, http.StatusInternalServerError, httpapi.CodeInternalError)
	if id := panicked.header.Get("X-Correlation-Id"); id == "" || strings.Contains(id, " ") {
		t.Errorf("invalid correlation id must be replaced, got %q", id)
	}

	if v := testutil.ToFloat64(s.metrics.HTTPRequestsTotal.WithLabelValues("POST /wagering/transactions", "POST", "500")); v != 1 {
		t.Errorf("http metric = %v", v)
	}
	if v := testutil.ToFloat64(s.metrics.HTTPRequestsTotal.WithLabelValues("GET /health/live", "GET", "200")); v != 1 {
		t.Errorf("health metric = %v", v)
	}
}

func TestBearerParsing(t *testing.T) {
	t.Parallel()

	s := newServer(t, fakeWallets{}, fakeWagering{get: func(app.Actor, uuid.UUID) (*wagering.Transaction, error) {
		return transaction(t, wagering.StatusProcessed), nil
	}})
	path := "/wagering/transactions/0192f298-345e-7e38-af88-e43f851a819d"
	for header, want := range map[string]int{
		"Bearer provider-a":   http.StatusOK,
		"bearer provider-a":   http.StatusOK,
		"Basic provider-a":    http.StatusUnauthorized,
		"Bearer":              http.StatusUnauthorized,
		"Bearer    ":          http.StatusUnauthorized,
		"provider-a":          http.StatusUnauthorized,
		"Bearer forged-token": http.StatusUnauthorized,
	} {
		res := s.do(t, http.MethodGet, path, "", "", map[string]string{"Authorization": header})
		if res.code != want {
			t.Errorf("Authorization %q = %d, want %d", header, res.code, want)
		}
	}
}
