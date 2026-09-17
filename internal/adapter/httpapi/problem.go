package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/danfigueroa/backend-challenge-go/internal/adapter/auth"
	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/app/wageringapp"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
)

const (
	contentTypeJSON    = "application/json"
	contentTypeProblem = "application/problem+json"
)

const (
	CodeUnauthenticated         = "UNAUTHENTICATED"
	CodeInvalidToken            = "INVALID_TOKEN"
	CodeTokenExpired            = "TOKEN_EXPIRED"
	CodeInsufficientPermissions = "INSUFFICIENT_PERMISSIONS"
	CodeForbidden               = "FORBIDDEN"
	CodeNotFound                = "NOT_FOUND"
	CodeWalletAlreadyExists     = "WALLET_ALREADY_EXISTS"
	CodeUnsupportedMediaType    = "UNSUPPORTED_MEDIA_TYPE"
	CodePayloadTooLarge         = "PAYLOAD_TOO_LARGE"
	CodeServiceUnavailable      = "SERVICE_UNAVAILABLE"
	CodeInternalError           = "INTERNAL_ERROR"
	CodeMethodNotAllowed        = "METHOD_NOT_ALLOWED"
)

type Problem struct {
	Type          string `json:"type"`
	Title         string `json:"title"`
	Status        int    `json:"status"`
	Code          string `json:"code"`
	Detail        string `json:"detail,omitempty"`
	Field         string `json:"field,omitempty"`
	Instance      string `json:"instance,omitempty"`
	CorrelationID string `json:"correlationId,omitempty"`
	TransactionID string `json:"existingTransactionId,omitempty"`
}

func newProblem(status int, code, detail string) Problem {
	return Problem{
		Type:   "urn:wallet:problem:" + strings.ToLower(strings.ReplaceAll(code, "_", "-")),
		Title:  http.StatusText(status),
		Status: status,
		Code:   code,
		Detail: detail,
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	p.Instance = r.URL.Path
	p.CorrelationID = correlationID(r.Context())
	if p.Status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="wallet-api", error="invalid_token"`)
	}
	if p.Status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "1")
	}
	w.Header().Set("Content-Type", contentTypeProblem)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

func problemFor(err error) Problem {
	if verr, ok := errors.AsType[*wagering.ValidationError](err); ok {
		status := http.StatusBadRequest
		p := newProblem(status, string(verr.Code), verr.Reason)
		p.Field = verr.Field
		return p
	}
	if verr, ok := errors.AsType[*app.ValidationError](err); ok {
		p := newProblem(http.StatusBadRequest, verr.Code, verr.Reason)
		p.Field = verr.Field
		return p
	}
	if conflict, ok := errors.AsType[*wageringapp.IdempotencyConflictError](err); ok {
		p := newProblem(http.StatusConflict, string(conflict.Code), "the operation was already registered with different content or another idempotency key")
		p.TransactionID = conflict.ExistingTransactionID.String()
		return p
	}
	if kind, ok := app.ConflictKindOf(err); ok && (kind == app.ConflictWalletExists || kind == app.ConflictOpeningExists) {
		return newProblem(http.StatusConflict, CodeWalletAlreadyExists, "a wallet already exists for this player and currency")
	}

	switch {
	case errors.Is(err, auth.ErrMissingToken):
		return newProblem(http.StatusUnauthorized, CodeUnauthenticated, "a bearer token is required")
	case errors.Is(err, auth.ErrExpiredToken):
		return newProblem(http.StatusUnauthorized, CodeTokenExpired, "the access token has expired")
	case errors.Is(err, auth.ErrInvalidToken):
		return newProblem(http.StatusUnauthorized, CodeInvalidToken, "the access token is invalid for this API")
	case errors.Is(err, auth.ErrIDPUnavailable):
		return newProblem(http.StatusServiceUnavailable, CodeServiceUnavailable, "the identity provider is temporarily unavailable")
	case errors.Is(err, app.ErrForbidden):
		return newProblem(http.StatusForbidden, CodeForbidden, "the authenticated client is not allowed to perform this operation")
	case errors.Is(err, app.ErrNotFound):
		return newProblem(http.StatusNotFound, CodeNotFound, "the requested resource does not exist")
	case errors.Is(err, app.ErrInvalidInput):
		return newProblem(http.StatusBadRequest, string(wagering.CodeInvalidField), "invalid input")
	case errors.Is(err, app.ErrTransient), errors.Is(err, context.DeadlineExceeded):
		return newProblem(http.StatusServiceUnavailable, CodeServiceUnavailable, "temporarily unavailable, retry with the same idempotency key")
	}
	return newProblem(http.StatusInternalServerError, CodeInternalError, "unexpected error")
}

func (h *handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	p := problemFor(err)
	switch {
	case p.Status >= http.StatusInternalServerError:
		h.logger.ErrorContext(r.Context(), "request failed", slog.Int("status", p.Status), slog.String("code", p.Code), slog.Any("error", err))
	case errors.Is(err, context.Canceled):
		h.logger.InfoContext(r.Context(), "request cancelled by client", slog.Any("error", err))
	default:
		h.logger.InfoContext(r.Context(), "request rejected", slog.Int("status", p.Status), slog.String("code", p.Code), slog.String("detail", p.Detail))
	}
	writeProblem(w, r, p)
}
