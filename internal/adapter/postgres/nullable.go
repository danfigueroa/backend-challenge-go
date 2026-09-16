package postgres

import (
	"time"

	"github.com/google/uuid"
)

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func valueOr[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
