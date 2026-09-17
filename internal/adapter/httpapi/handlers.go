package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/danfigueroa/backend-challenge-go/internal/adapter/auth"
	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/app/wageringapp"
	"github.com/danfigueroa/backend-challenge-go/internal/app/walletapp"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/health"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/logging"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/metrics"
)

type WalletService interface {
	OpenWallet(ctx context.Context, cmd walletapp.OpenWalletCommand) (walletapp.WalletView, error)
	GetWallet(ctx context.Context, actor app.Actor, walletID uuid.UUID) (walletapp.WalletView, error)
	ListLedger(ctx context.Context, q walletapp.LedgerQuery) (walletapp.LedgerPage, error)
	Reconcile(ctx context.Context, actor app.Actor, walletID uuid.UUID) (walletapp.ReconciliationReport, error)
}

type WageringService interface {
	Process(ctx context.Context, cmd wageringapp.ProcessCommand) (wageringapp.ProcessResult, error)
	GetTransaction(ctx context.Context, actor app.Actor, id uuid.UUID) (*wagering.Transaction, error)
	GetByExternalID(ctx context.Context, actor app.Actor, providerID, externalTransactionID string) (*wagering.Transaction, error)
}

type TokenVerifier interface {
	Verify(ctx context.Context, rawToken string) (auth.Principal, error)
}

type Settings struct {
	RequestTimeout time.Duration
	MaxBodyBytes   int64
}

type Deps struct {
	Settings Settings
	Wallets  WalletService
	Wagering WageringService
	Verifier TokenVerifier
	Health   *health.Checker
	Metrics  *metrics.Metrics
	Logger   *slog.Logger
}

type handlers struct {
	settings Settings
	wallets  WalletService
	wagering WageringService
	verifier TokenVerifier
	metrics  *metrics.Metrics
	logger   *slog.Logger
}

func NewHandler(d Deps) (http.Handler, error) {
	if d.Wallets == nil || d.Wagering == nil || d.Verifier == nil || d.Health == nil || d.Metrics == nil || d.Logger == nil {
		return nil, errors.New("httpapi: all dependencies are required")
	}
	if d.Settings.RequestTimeout <= 0 || d.Settings.MaxBodyBytes <= 0 {
		return nil, errors.New("httpapi: request timeout and body limit must be positive")
	}
	h := &handlers{
		settings: d.Settings, wallets: d.Wallets, wagering: d.Wagering, verifier: d.Verifier,
		metrics: d.Metrics, logger: d.Logger,
	}

	mux := http.NewServeMux()
	route := func(pattern, permission string, handler http.HandlerFunc, jsonBody bool) {
		var chain http.Handler = handler
		if jsonBody {
			chain = h.requireJSON(chain)
		}
		chain = h.withTimeout(chain)
		if permission != "" {
			chain = h.authenticate(permission, chain)
		}
		mux.Handle(pattern, otelhttp.NewHandler(h.instrument(pattern, chain), pattern))
	}

	route("POST /wallets", auth.PermissionWalletsWrite, h.openWallet, true)
	route("GET /wallets/{walletId}", auth.PermissionWalletsRead, h.getWallet, false)
	route("GET /wallets/{walletId}/ledger", auth.PermissionWalletsRead, h.listLedger, false)
	route("POST /wallets/{walletId}/reconciliation", auth.PermissionWalletsReconcile, h.reconcile, false)
	route("POST /wagering/transactions", auth.PermissionTransactionsWrite, h.processTransaction, true)
	route("GET /wagering/transactions/{transactionId}", auth.PermissionTransactionsRead, h.getTransaction, false)
	route("GET /providers/{providerId}/wagering/transactions/{externalTransactionId}", auth.PermissionTransactionsRead, h.getByExternalID, false)
	mux.Handle("GET /health/live", h.instrument("GET /health/live", d.Health.LiveHandler()))
	mux.Handle("GET /health/ready", h.instrument("GET /health/ready", d.Health.ReadyHandler()))
	mux.Handle("/", h.instrument("unmatched", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, r, newProblem(http.StatusNotFound, CodeNotFound, "no route matches "+r.Method+" "+r.URL.Path))
	})))

	return h.withRecovery(h.withCorrelation(mux)), nil
}

func decodeJSON(r *http.Request, into any) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if isBodyTooLarge(err) {
			return errBodyTooLarge
		}
		return malformed(fmt.Errorf("read body: %w", err))
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return malformed(err)
	}
	if dec.More() {
		return malformed(errors.New("unexpected data after the JSON object"))
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return malformed(errors.New("unexpected data after the JSON object"))
	}
	return nil
}

var errBodyTooLarge = errors.New("httpapi: body too large")

func malformed(err error) error {
	return wagering.NewValidationError(wagering.CodeMalformedRequest, "", "request body is not a valid JSON object for this operation: "+err.Error())
}

func (h *handlers) decodeOrFail(w http.ResponseWriter, r *http.Request, into any) bool {
	err := decodeJSON(r, into)
	switch {
	case err == nil:
		return true
	case errors.Is(err, errBodyTooLarge):
		writeProblem(w, r, newProblem(http.StatusRequestEntityTooLarge, CodePayloadTooLarge, "request body exceeds the allowed size"))
	default:
		h.fail(w, r, err)
	}
	return false
}

func pathUUID(r *http.Request, name string) (uuid.UUID, error) {
	raw := r.PathValue(name)
	id, err := uuid.Parse(raw)
	if err != nil || id.String() != raw || id == uuid.Nil {
		return uuid.Nil, wagering.NewValidationError(wagering.CodeInvalidField, name, "must be a canonical lowercase UUID")
	}
	return id, nil
}

func metadata(r *http.Request) app.Metadata {
	return app.Metadata{Channel: app.ChannelHTTP, CorrelationID: correlationID(r.Context()), CausationID: correlationID(r.Context())}
}

func (h *handlers) openWallet(w http.ResponseWriter, r *http.Request) {
	var req openWalletRequest
	if !h.decodeOrFail(w, r, &req) {
		return
	}
	cmd := walletapp.OpenWalletCommand{Actor: actorFrom(r.Context()), Meta: metadata(r), PlayerID: req.PlayerID}
	if req.InitialBalance != nil {
		cmd.Amount, cmd.Currency = req.InitialBalance.Amount, req.InitialBalance.Currency
	}
	view, err := h.wallets.OpenWallet(r.Context(), cmd)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/wallets/"+view.ID.String())
	writeJSON(w, http.StatusCreated, walletBody(view))
}

func (h *handlers) getWallet(w http.ResponseWriter, r *http.Request) {
	walletID, err := pathUUID(r, "walletId")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	view, err := h.wallets.GetWallet(r.Context(), actorFrom(r.Context()), walletID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, walletBody(view))
}

func (h *handlers) listLedger(w http.ResponseWriter, r *http.Request) {
	walletID, err := pathUUID(r, "walletId")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if limit, err = strconv.Atoi(raw); err != nil {
			h.fail(w, r, wagering.NewValidationError(wagering.CodeInvalidField, "limit", "must be an integer"))
			return
		}
	}
	page, err := h.wallets.ListLedger(r.Context(), walletapp.LedgerQuery{
		Actor: actorFrom(r.Context()), WalletID: walletID, Cursor: r.URL.Query().Get("cursor"), Limit: limit,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ledgerBody(walletID, page))
}

func (h *handlers) reconcile(w http.ResponseWriter, r *http.Request) {
	walletID, err := pathUUID(r, "walletId")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ctx := logging.With(r.Context(), slog.String(logging.KeyWalletID, walletID.String()))
	report, err := h.wallets.Reconcile(ctx, actorFrom(ctx), walletID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, reconciliationBody(report))
}

func (h *handlers) processTransaction(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get(HeaderIdempotencyKey)
	if key == "" {
		h.fail(w, r, wagering.NewValidationError(wagering.CodeMissingField, "Idempotency-Key", "header is required"))
		return
	}
	var req transactionRequest
	if !h.decodeOrFail(w, r, &req) {
		return
	}

	ctx := logging.With(r.Context(), slog.String(logging.KeyWalletID, req.WalletID))
	result, err := h.wagering.Process(ctx, wageringapp.ProcessCommand{
		Actor: actorFrom(ctx), Meta: metadata(r), Input: req.input(key),
	})
	if err != nil {
		h.fail(w, r.WithContext(ctx), err)
		return
	}

	tx := result.Transaction
	h.logger.InfoContext(logging.With(ctx, slog.String(logging.KeyTransactionID, tx.ID().String())), "transaction handled",
		slog.String("status", string(tx.Status())), slog.Bool("idempotentReplay", result.IdempotentReplay))

	status := http.StatusOK
	switch tx.Status() {
	case wagering.StatusPendingReference, wagering.StatusPending:
		status = http.StatusAccepted
		w.Header().Set("Location", "/wagering/transactions/"+tx.ID().String())
	case wagering.StatusRejected, wagering.StatusFailed:
		status = http.StatusUnprocessableEntity
	case wagering.StatusProcessed:
	}
	writeJSON(w, status, processBody(tx, result.IdempotentReplay))
}

func (h *handlers) getTransaction(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "transactionId")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	tx, err := h.wagering.GetTransaction(r.Context(), actorFrom(r.Context()), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, transactionBody(tx))
}

func (h *handlers) getByExternalID(w http.ResponseWriter, r *http.Request) {
	tx, err := h.wagering.GetByExternalID(r.Context(), actorFrom(r.Context()), r.PathValue("providerId"), r.PathValue("externalTransactionId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, transactionBody(tx))
}
