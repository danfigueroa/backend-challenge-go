package app_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
)

var fastRetry = app.RetryPolicy{Attempts: 4, BaseDelay: time.Millisecond, MaxDelay: 4 * time.Millisecond}

func TestRetryStopsOnSuccess(t *testing.T) {
	t.Parallel()

	calls := 0
	err := app.Retry(context.Background(), fastRetry, nil, func(context.Context) error {
		calls++
		if calls < 3 {
			return fmt.Errorf("%w: flaky", app.ErrTransient)
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Errorf("err = %v, calls = %d", err, calls)
	}
}

func TestRetryGivesUpAfterAttempts(t *testing.T) {
	t.Parallel()

	calls, observed := 0, 0
	err := app.Retry(context.Background(), fastRetry, func(int, error) { observed++ }, func(context.Context) error {
		calls++
		return app.ErrConcurrentUpdate
	})
	if !errors.Is(err, app.ErrConcurrentUpdate) || calls != 4 || observed != 3 {
		t.Errorf("err = %v, calls = %d, observed = %d", err, calls, observed)
	}
}

func TestRetryDoesNotRetryPermanentErrors(t *testing.T) {
	t.Parallel()

	permanent := []error{
		errors.New("boom"),
		app.ErrNotFound,
		app.ErrForbidden,
		app.ErrIntegrityViolation,
		&app.ConflictError{Kind: app.ConflictWalletExists},
		&app.ValidationError{Code: "X"},
	}
	for _, perm := range permanent {
		calls := 0
		err := app.Retry(context.Background(), fastRetry, nil, func(context.Context) error {
			calls++
			return perm
		})
		if !errors.Is(err, perm) || calls != 1 {
			t.Errorf("%v: calls = %d", perm, calls)
		}
	}
}

func TestRetryRespectsCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	slow := app.RetryPolicy{Attempts: 10, BaseDelay: time.Hour, MaxDelay: time.Hour}
	calls := 0
	done := make(chan error, 1)
	go func() {
		done <- app.Retry(ctx, slow, nil, func(context.Context) error {
			calls++
			return app.ErrTransient
		})
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !errors.Is(err, app.ErrTransient) || calls != 1 {
			t.Errorf("err = %v, calls = %d", err, calls)
		}
	case <-time.After(time.Second):
		t.Fatal("retry did not stop on cancellation")
	}
}

func TestIsRetryableConflictKinds(t *testing.T) {
	t.Parallel()

	tests := map[app.ConflictKind]bool{
		app.ConflictIdempotencyKey:      true,
		app.ConflictExternalTransaction: true,
		app.ConflictReferenceReversed:   true,
		app.ConflictInboxMessage:        true,
		app.ConflictLedgerEntry:         true,
		app.ConflictWalletExists:        false,
		app.ConflictOpeningExists:       false,
		app.ConflictDuplicateID:         false,
		app.ConflictUnknown:             false,
	}
	for kind, want := range tests {
		err := fmt.Errorf("wrapped: %w", &app.ConflictError{Kind: kind})
		if got := app.IsRetryable(err); got != want {
			t.Errorf("IsRetryable(%s) = %v, want %v", kind, got, want)
		}
		if !errors.Is(err, app.ErrConflict) {
			t.Errorf("%s is not ErrConflict", kind)
		}
	}
}

func TestRetryPolicyValidate(t *testing.T) {
	t.Parallel()

	if err := fastRetry.Validate(); err != nil {
		t.Error(err)
	}
	for _, bad := range []app.RetryPolicy{
		{Attempts: 0, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
		{Attempts: 1, BaseDelay: 0, MaxDelay: time.Millisecond},
		{Attempts: 1, BaseDelay: time.Second, MaxDelay: time.Millisecond},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("Validate(%+v) = nil", bad)
		}
	}
}

func TestActorAuthorization(t *testing.T) {
	t.Parallel()

	providerA := app.ProviderActor("svc-provider-a", "provider-a")
	service := app.ServiceActor("wallet-internal")
	broker := app.BrokerActor("wager-consumer")
	anonymous := app.Actor{}

	tests := []struct {
		name  string
		actor app.Actor
		act   map[string]bool
		read  map[string]bool
		inner bool
	}{
		{"provider", providerA, map[string]bool{"provider-a": true, "provider-b": false, "": false}, map[string]bool{"provider-a": true, "provider-b": false}, false},
		{"service", service, map[string]bool{"provider-a": true, "": false}, map[string]bool{"provider-a": true, "provider-b": true}, true},
		{"broker", broker, map[string]bool{"provider-a": true, "": false}, map[string]bool{"provider-a": false}, false},
		{"anonymous", anonymous, map[string]bool{"provider-a": false}, map[string]bool{"provider-a": false}, false},
		{"provider without id", app.ProviderActor("x", ""), map[string]bool{"": false}, map[string]bool{"": false}, false},
	}
	for _, tc := range tests {
		for provider, want := range tc.act {
			if got := tc.actor.CanActForProvider(provider); got != want {
				t.Errorf("%s CanActForProvider(%q) = %v", tc.name, provider, got)
			}
		}
		for provider, want := range tc.read {
			if got := tc.actor.CanReadProvider(provider); got != want {
				t.Errorf("%s CanReadProvider(%q) = %v", tc.name, provider, got)
			}
		}
		if err := tc.actor.RequireInternalService(); (err == nil) != tc.inner || (err != nil && !errors.Is(err, app.ErrForbidden)) {
			t.Errorf("%s RequireInternalService = %v", tc.name, err)
		}
	}
}

func TestMetadataDefaults(t *testing.T) {
	t.Parallel()

	m := app.Metadata{}.WithDefaults(app.UUIDv7Generator{})
	if m.CorrelationID == "" || m.Channel != app.ChannelInternal {
		t.Errorf("defaults = %+v", m)
	}
	kept := app.Metadata{Channel: app.ChannelSQS, CorrelationID: "corr"}.WithDefaults(app.UUIDv7Generator{})
	if kept.CorrelationID != "corr" || kept.Channel != app.ChannelSQS {
		t.Errorf("explicit values overwritten: %+v", kept)
	}
	if now := (app.SystemClock{}).Now(); now.Location() != time.UTC || now.Nanosecond()%1000 != 0 {
		t.Errorf("system clock must be UTC with microsecond precision: %v", now)
	}
}
