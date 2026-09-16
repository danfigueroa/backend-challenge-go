package wageringapp

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
)

func (s *Service) GetTransaction(ctx context.Context, actor app.Actor, id uuid.UUID) (*wagering.Transaction, error) {
	t, err := s.transactions.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !actor.IsInternalService() && (t.Origin() != wagering.OriginExternal || !actor.CanReadProvider(t.ProviderID())) {
		return nil, fmt.Errorf("%w: transaction %s", app.ErrNotFound, id)
	}
	return t, nil
}

func (s *Service) GetByExternalID(ctx context.Context, actor app.Actor, providerID, externalTransactionID string) (*wagering.Transaction, error) {
	if !actor.CanReadProvider(providerID) {
		return nil, fmt.Errorf("%w: %s %q cannot read provider %q", app.ErrForbidden, actor.Kind, actor.Subject, providerID)
	}
	return s.transactions.GetByExternalID(ctx, providerID, externalTransactionID)
}
