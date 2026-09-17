package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/adapter/auth"
	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/logging"
)

const (
	HeaderCorrelationID  = "X-Correlation-Id"
	HeaderIdempotencyKey = "Idempotency-Key"
	maxCorrelationLength = 128
)

type ctxKey int

const (
	correlationKey ctxKey = iota
	principalKey
	requestInfoKey
)

type requestInfo struct {
	principal *auth.Principal
}

func correlationID(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey).(string)
	return id
}

func principalFrom(ctx context.Context) (auth.Principal, bool) {
	p, ok := ctx.Value(principalKey).(auth.Principal)
	return p, ok
}

func actorFrom(ctx context.Context) app.Actor {
	if p, ok := principalFrom(ctx); ok {
		return p.Actor()
	}
	return app.Actor{}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.status, r.wrote = http.StatusOK, true
	}
	n, err := r.ResponseWriter.Write(b)
	if err != nil {
		return n, fmt.Errorf("write response: %w", err)
	}
	return n, nil
}

func isAbort(recovered any) bool {
	err, ok := recovered.(error)
	return ok && errors.Is(err, http.ErrAbortHandler)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func validCorrelationID(id string) bool {
	if id == "" || len(id) > maxCorrelationLength {
		return false
	}
	for i := range len(id) {
		if id[i] < '!' || id[i] > '~' {
			return false
		}
	}
	return true
}

func (h *handlers) withCorrelation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderCorrelationID)
		if !validCorrelationID(id) {
			id = uuid.Must(uuid.NewV7()).String()
		}
		w.Header().Set(HeaderCorrelationID, id)
		ctx := context.WithValue(r.Context(), correlationKey, id)
		ctx = logging.With(ctx, slog.String(logging.KeyCorrelationID, id))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (h *handlers) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				if isAbort(recovered) {
					panic(recovered)
				}
				h.logger.ErrorContext(r.Context(), "handler panicked", slog.Any("panic", recovered), slog.String("stack", string(debug.Stack())))
				writeProblem(w, r, newProblem(http.StatusInternalServerError, CodeInternalError, "unexpected error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (h *handlers) instrument(route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		info := &requestInfo{}
		r = r.WithContext(context.WithValue(r.Context(), requestInfoKey, info))
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.serveRecovering(rec, r, next)
		elapsed := time.Since(started)

		h.metrics.HTTPRequestsTotal.WithLabelValues(route, r.Method, strconv.Itoa(rec.status)).Inc()
		h.metrics.HTTPRequestDuration.WithLabelValues(route, r.Method).Observe(elapsed.Seconds())

		attrs := []slog.Attr{
			slog.String("method", r.Method), slog.String("route", route), slog.Int("status", rec.status),
			slog.Duration("duration", elapsed),
		}
		if p := info.principal; p != nil {
			attrs = append(attrs, slog.String("clientId", p.ClientID), slog.String(logging.KeyProviderID, p.ProviderID))
		}
		level := slog.LevelInfo
		if rec.status >= http.StatusInternalServerError {
			level = slog.LevelError
		}
		h.logger.LogAttrs(r.Context(), level, "http request", attrs...)
	})
}

func (h *handlers) serveRecovering(w *statusRecorder, r *http.Request, next http.Handler) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		if isAbort(recovered) {
			panic(recovered)
		}
		h.logger.ErrorContext(r.Context(), "handler panicked", slog.Any("panic", recovered), slog.String("stack", string(debug.Stack())))
		if !w.wrote {
			writeProblem(w, r, newProblem(http.StatusInternalServerError, CodeInternalError, "unexpected error"))
		}
	}()
	next.ServeHTTP(w, r)
}

func (h *handlers) authenticate(permission string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := bearerToken(r)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		principal, err := h.verifier.Verify(r.Context(), raw)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		if info, ok := r.Context().Value(requestInfoKey).(*requestInfo); ok {
			info.principal = &principal
		}
		ctx := context.WithValue(r.Context(), principalKey, principal)
		ctx = logging.With(ctx, slog.String("clientId", principal.ClientID), slog.String(logging.KeyProviderID, principal.ProviderID))
		r = r.WithContext(ctx)

		if !principal.Has(permission) {
			p := newProblem(http.StatusForbidden, CodeInsufficientPermissions, "the token lacks the "+permission+" permission")
			h.logger.InfoContext(ctx, "permission denied", slog.String("permission", permission))
			writeProblem(w, r, p)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) (string, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", auth.ErrMissingToken
	}
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", auth.ErrInvalidToken
	}
	return strings.TrimSpace(token), nil
}

func (h *handlers) withTimeout(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), h.settings.RequestTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (h *handlers) requireJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != contentTypeJSON {
			writeProblem(w, r, newProblem(http.StatusUnsupportedMediaType, CodeUnsupportedMediaType, "Content-Type must be application/json"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, h.settings.MaxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

func isBodyTooLarge(err error) bool {
	_, ok := errors.AsType[*http.MaxBytesError](err)
	return ok
}
