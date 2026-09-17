//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/danfigueroa/backend-challenge-go/internal/adapter/auth"
	"github.com/danfigueroa/backend-challenge-go/internal/adapter/httpapi"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/health"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/metrics"
	"github.com/danfigueroa/backend-challenge-go/internal/testsupport/apptest"
	"github.com/danfigueroa/backend-challenge-go/internal/testsupport/kctest"
	"github.com/danfigueroa/backend-challenge-go/internal/testsupport/pgtest"
)

var (
	pg *pgtest.Instance
	kc *kctest.Instance
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	var g errgroup.Group
	g.Go(func() (err error) { pg, err = pgtest.Start(ctx); return err })
	g.Go(func() (err error) { kc, err = kctest.Start(ctx); return err })
	err := g.Wait()
	defer func() {
		stop, cancelStop := context.WithTimeout(context.Background(), time.Minute)
		defer cancelStop()
		if pg != nil {
			_ = pg.Terminate(stop)
		}
		if kc != nil {
			_ = kc.Terminate(stop)
		}
	}()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return m.Run()
}

type api struct {
	*apptest.Harness
	url    string
	client *http.Client
	tokens sync.Map
}

func newAPI(t *testing.T) *api {
	t.Helper()
	h := apptest.New(t, pg)
	verifier, err := auth.NewVerifier(auth.Settings{Issuer: kc.Issuer(), JWKSURL: kc.JWKSURL(), Audience: kctest.Audience})
	if err != nil {
		t.Fatal(err)
	}
	checker := health.NewChecker(time.Second, 0)
	handler, err := httpapi.NewHandler(httpapi.Deps{
		Settings: httpapi.Settings{RequestTimeout: 10 * time.Second, MaxBodyBytes: 64 << 10},
		Wallets:  h.Wallets, Wagering: h.Wagering, Verifier: verifier, Health: checker,
		Metrics: metrics.New(), Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &api{Harness: h, url: srv.URL, client: srv.Client()}
}

func (a *api) token(t *testing.T, client string) string {
	t.Helper()
	if cached, ok := a.tokens.Load(client); ok {
		return cached.(string)
	}
	token, err := kc.Token(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	a.tokens.Store(client, token)
	return token
}

type reply struct {
	status int
	body   map[string]any
	raw    string
	header http.Header
}

func (a *api) call(t *testing.T, method, path, bearer, body string, headers ...string) reply {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, a.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := a.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	r := reply{status: resp.StatusCode, raw: string(raw), header: resp.Header}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &r.body); err != nil {
			t.Fatalf("%s %s returned non-JSON: %s", method, path, raw)
		}
	}
	return r
}

func (r reply) expect(t *testing.T, status int, code string) reply {
	t.Helper()
	if r.status != status || (code != "" && r.body["code"] != code) {
		t.Fatalf("got %d %s, want %d %s", r.status, r.raw, status, code)
	}
	return r
}

func (a *api) openWallet(t *testing.T, amount string) (walletID, playerID string) {
	t.Helper()
	playerID = uuid.NewString()
	r := a.call(t, http.MethodPost, "/wallets", a.token(t, "wallet-internal"),
		fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":%q,"currency":"BRL"}}`, playerID, amount)).
		expect(t, http.StatusCreated, "")
	return r.body["id"].(string), playerID
}

func operation(externalID, walletID, playerID, kind, amount, reference string) string {
	body := map[string]any{
		"providerId": "provider-a", "externalTransactionId": externalID, "playerId": playerID, "walletId": walletID,
		"roundId": "round-987", "gameId": "fortune-chimp", "kind": kind,
		"money": map[string]string{"amount": amount, "currency": "BRL"},
	}
	if reference != "" {
		body["referenceExternalTransactionId"] = reference
	}
	data, _ := json.Marshal(body)
	return string(data)
}

func TestAuthenticationAgainstRealKeycloak(t *testing.T) {
	a := newAPI(t)
	walletID, playerID := a.openWallet(t, "100.00")
	bet := operation("auth-bet", walletID, playerID, "BET", "10.00", "")
	key := []string{"Idempotency-Key", "provider-a:auth-bet"}

	valid := a.token(t, "provider-a")
	parts := strings.Split(valid, ".")
	tamperedPayload := parts[0] + "." + parts[1][:len(parts[1])-2] + "xx." + parts[2]
	shortLived := a.token(t, "provider-a-short-lived")

	a.call(t, http.MethodPost, "/wagering/transactions", "", bet, key...).expect(t, http.StatusUnauthorized, httpapi.CodeUnauthenticated)
	a.call(t, http.MethodPost, "/wagering/transactions", "not-a-jwt", bet, key...).expect(t, http.StatusUnauthorized, httpapi.CodeInvalidToken)
	a.call(t, http.MethodPost, "/wagering/transactions", tamperedPayload, bet, key...).expect(t, http.StatusUnauthorized, httpapi.CodeInvalidToken)
	a.call(t, http.MethodPost, "/wagering/transactions", a.token(t, "foreign-audience"), bet, key...).expect(t, http.StatusUnauthorized, httpapi.CodeInvalidToken)
	a.call(t, http.MethodPost, "/wagering/transactions", a.token(t, "provider-unprivileged"), bet, key...).expect(t, http.StatusForbidden, httpapi.CodeInsufficientPermissions)

	time.Sleep(3 * time.Second)
	a.call(t, http.MethodPost, "/wagering/transactions", shortLived, bet, key...).expect(t, http.StatusUnauthorized, httpapi.CodeTokenExpired)

	if n := a.Count(t, "SELECT count(*) FROM wager_transactions WHERE origin = 'EXTERNAL'"); n != 0 {
		t.Fatalf("rejected requests had financial effects: %d transactions", n)
	}

	ok := a.call(t, http.MethodPost, "/wagering/transactions", valid, bet, key...).expect(t, http.StatusOK, "")
	if ok.body["status"] != "PROCESSED" || ok.body["balance"].(map[string]any)["amount"] != "90.00" {
		t.Errorf("valid token response = %s", ok.raw)
	}
}

func TestProviderIsolationOverHTTP(t *testing.T) {
	a := newAPI(t)
	internal, providerA, providerB := a.token(t, "wallet-internal"), a.token(t, "provider-a"), a.token(t, "provider-b")
	walletID, playerID := a.openWallet(t, "100.00")

	bet := operation("iso-bet", walletID, playerID, "BET", "10.00", "")
	created := a.call(t, http.MethodPost, "/wagering/transactions", providerA, bet, "Idempotency-Key", "provider-a:iso-bet").expect(t, http.StatusOK, "")
	txID := created.body["transactionId"].(string)

	a.call(t, http.MethodPost, "/wagering/transactions", providerB, bet, "Idempotency-Key", "provider-a:iso-bet").expect(t, http.StatusForbidden, httpapi.CodeForbidden)
	a.call(t, http.MethodGet, "/wagering/transactions/"+txID, providerB, "").expect(t, http.StatusNotFound, httpapi.CodeNotFound)
	a.call(t, http.MethodGet, "/providers/provider-a/wagering/transactions/iso-bet", providerB, "").expect(t, http.StatusForbidden, httpapi.CodeForbidden)
	a.call(t, http.MethodGet, "/providers/provider-b/wagering/transactions/iso-bet", providerB, "").expect(t, http.StatusNotFound, httpapi.CodeNotFound)

	for _, path := range []string{"/wallets/" + walletID, "/wallets/" + walletID + "/ledger"} {
		a.call(t, http.MethodGet, path, providerA, "").expect(t, http.StatusForbidden, httpapi.CodeInsufficientPermissions)
	}
	a.call(t, http.MethodPost, "/wallets/"+walletID+"/reconciliation", providerA, "").expect(t, http.StatusForbidden, httpapi.CodeInsufficientPermissions)
	a.call(t, http.MethodPost, "/wallets", providerA, `{"playerId":"`+uuid.NewString()+`","initialBalance":{"amount":"1.00","currency":"BRL"}}`).
		expect(t, http.StatusForbidden, httpapi.CodeInsufficientPermissions)
	a.call(t, http.MethodPost, "/wagering/transactions", internal, bet, "Idempotency-Key", "provider-a:iso-bet").
		expect(t, http.StatusForbidden, httpapi.CodeInsufficientPermissions)

	if r := a.call(t, http.MethodGet, "/wagering/transactions/"+txID, providerA, "").expect(t, http.StatusOK, ""); r.body["providerId"] != "provider-a" {
		t.Errorf("owner read = %s", r.raw)
	}
	a.call(t, http.MethodGet, "/providers/provider-a/wagering/transactions/iso-bet", providerA, "").expect(t, http.StatusOK, "")
	a.call(t, http.MethodGet, "/wagering/transactions/"+txID, internal, "").expect(t, http.StatusOK, "")

	if n := a.Count(t, "SELECT count(*) FROM wager_transactions WHERE origin = 'EXTERNAL'"); n != 1 {
		t.Errorf("transactions = %d, forbidden calls must not create any", n)
	}
	if n := a.Count(t, "SELECT count(*) FROM wallets"); n != 1 {
		t.Errorf("wallets = %d, forbidden wallet creation must not persist", n)
	}
}

func TestHTTPContractEndToEnd(t *testing.T) {
	a := newAPI(t)
	internal, provider := a.token(t, "wallet-internal"), a.token(t, "provider-a")
	playerID := "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"

	opened := a.call(t, http.MethodPost, "/wallets", internal, `{"playerId":"`+playerID+`","initialBalance":{"amount":"1000.00","currency":"BRL"}}`).
		expect(t, http.StatusCreated, "")
	walletID := opened.body["id"].(string)
	if opened.body["version"] != float64(1) || opened.body["balance"].(map[string]any)["amount"] != "1000.00" || opened.body["playerId"] != playerID {
		t.Errorf("open = %s", opened.raw)
	}
	a.call(t, http.MethodPost, "/wallets", internal, `{"playerId":"`+playerID+`","initialBalance":{"amount":"5.00","currency":"BRL"}}`).
		expect(t, http.StatusConflict, httpapi.CodeWalletAlreadyExists)

	bet := operation("transaction-123", walletID, playerID, "BET", "25.00", "")
	first := a.call(t, http.MethodPost, "/wagering/transactions", provider, bet, "Idempotency-Key", "provider-a:transaction-123").expect(t, http.StatusOK, "")
	if first.body["status"] != "PROCESSED" || first.body["idempotentReplay"] != false || first.body["balance"].(map[string]any)["amount"] != "975.00" {
		t.Errorf("bet = %s", first.raw)
	}
	a.call(t, http.MethodPost, "/wagering/transactions", provider, operation("win-1", walletID, playerID, "WIN", "5.00", ""), "Idempotency-Key", "provider-a:win-1").
		expect(t, http.StatusOK, "")
	replay := a.call(t, http.MethodPost, "/wagering/transactions", provider, bet, "Idempotency-Key", "provider-a:transaction-123").expect(t, http.StatusOK, "")
	if replay.body["idempotentReplay"] != true || replay.body["balance"].(map[string]any)["amount"] != "975.00" || replay.body["transactionId"] != first.body["transactionId"] {
		t.Errorf("replay = %s", replay.raw)
	}

	a.call(t, http.MethodPost, "/wagering/transactions", provider, operation("transaction-123", walletID, playerID, "BET", "26.00", ""), "Idempotency-Key", "provider-a:transaction-123").
		expect(t, http.StatusConflict, "IDEMPOTENCY_KEY_CONFLICT")
	a.call(t, http.MethodPost, "/wagering/transactions", provider, bet, "Idempotency-Key", "another-key").
		expect(t, http.StatusConflict, "EXTERNAL_TRANSACTION_CONFLICT")
	a.call(t, http.MethodPost, "/wagering/transactions", provider, strings.Replace(bet, `"25.00"`, `"25"`, 1), "Idempotency-Key", "provider-a:bad-amount").
		expect(t, http.StatusBadRequest, "INVALID_AMOUNT")
	a.call(t, http.MethodPost, "/wagering/transactions", provider, bet).
		expect(t, http.StatusBadRequest, "MISSING_FIELD")

	rejected := a.call(t, http.MethodPost, "/wagering/transactions", provider, operation("too-big", walletID, playerID, "BET", "5000.00", ""), "Idempotency-Key", "provider-a:too-big").
		expect(t, http.StatusUnprocessableEntity, "")
	if rejected.body["status"] != "REJECTED" || rejected.body["failureCode"] != "INSUFFICIENT_FUNDS" {
		t.Errorf("rejection = %s", rejected.raw)
	}

	pending := a.call(t, http.MethodPost, "/wagering/transactions", provider, operation("refund-early", walletID, playerID, "REFUND", "40.00", "bet-late"), "Idempotency-Key", "provider-a:refund-early").
		expect(t, http.StatusAccepted, "")
	if pending.body["status"] != "PENDING_REFERENCE" || pending.header.Get("Location") == "" {
		t.Errorf("pending = %s", pending.raw)
	}
	a.call(t, http.MethodPost, "/wagering/transactions", provider, operation("bet-late", walletID, playerID, "BET", "40.00", ""), "Idempotency-Key", "provider-a:bet-late").
		expect(t, http.StatusOK, "")
	if _, err := a.Wagering.ResolveDuePending(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	resolved := a.call(t, http.MethodGet, pending.header.Get("Location"), provider, "").expect(t, http.StatusOK, "")
	if resolved.body["status"] != "PROCESSED" || resolved.body["referenceTransactionId"] == nil {
		t.Errorf("resolved = %s", resolved.raw)
	}

	var (
		entries int
		cursor  string
	)
	for {
		path := "/wallets/" + walletID + "/ledger?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		page := a.call(t, http.MethodGet, path, internal, "").expect(t, http.StatusOK, "")
		entries += len(page.body["entries"].([]any))
		next, _ := page.body["nextCursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if entries != 5 {
		t.Errorf("ledger entries = %d, want opening + bet + win + bet-late + refund", entries)
	}

	rec := a.call(t, http.MethodPost, "/wallets/"+walletID+"/reconciliation", internal, "").expect(t, http.StatusOK, "")
	if rec.body["consistent"] != true || rec.body["checkedEntries"] != float64(5) || rec.body["storedBalance"].(map[string]any)["amount"] != "980.00" ||
		rec.body["difference"].(map[string]any)["amount"] != "0.00" {
		t.Errorf("reconciliation = %s", rec.raw)
	}
	wallet := a.call(t, http.MethodGet, "/wallets/"+walletID, internal, "").expect(t, http.StatusOK, "")
	if wallet.body["balance"].(map[string]any)["amount"] != "980.00" || wallet.body["version"] != float64(5) {
		t.Errorf("wallet = %s", wallet.raw)
	}
	a.call(t, http.MethodGet, "/health/live", "", "").expect(t, http.StatusOK, "")
}

func TestConcurrentHTTPRequests(t *testing.T) {
	a := newAPI(t)
	provider := a.token(t, "provider-a")

	walletID, playerID := a.openWallet(t, "1000.00")
	bet := operation("http-50x", walletID, playerID, "BET", "25.00", "")

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		statuses = map[int]int{}
		replays  int
	)
	for range 50 {
		wg.Go(func() {
			r := a.call(t, http.MethodPost, "/wagering/transactions", provider, bet, "Idempotency-Key", "provider-a:http-50x")
			mu.Lock()
			defer mu.Unlock()
			statuses[r.status]++
			if r.body["idempotentReplay"] == true {
				replays++
			}
		})
	}
	wg.Wait()
	if statuses[http.StatusOK] != 50 || replays != 49 {
		t.Errorf("statuses = %v, replays = %d", statuses, replays)
	}
	if n := a.Count(t, "SELECT count(*) FROM ledger_entries WHERE direction = 'DEBIT'"); n != 1 {
		t.Errorf("debits = %d", n)
	}

	raceWallet, racePlayer := a.openWallet(t, "100.00")
	results := make(chan reply, 2)
	for _, id := range []string{"race-80-a", "race-80-b"} {
		wg.Go(func() {
			results <- a.call(t, http.MethodPost, "/wagering/transactions", provider, operation(id, raceWallet, racePlayer, "BET", "80.00", ""), "Idempotency-Key", "provider-a:"+id)
		})
	}
	wg.Wait()
	close(results)
	codes := map[int]int{}
	for r := range results {
		codes[r.status]++
	}
	if codes[http.StatusOK] != 1 || codes[http.StatusUnprocessableEntity] != 1 {
		t.Errorf("80+80 on 100 = %v", codes)
	}
	a.AssertAllWalletsReconcile(t)
}
