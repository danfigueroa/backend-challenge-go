package wagering

import (
	"fmt"
	"slices"
)

type Status string

const (
	StatusPending          Status = "PENDING"
	StatusPendingReference Status = "PENDING_REFERENCE"
	StatusProcessed        Status = "PROCESSED"
	StatusRejected         Status = "REJECTED"
	StatusFailed           Status = "FAILED"
)

var allowedTransitions = map[Status][]Status{
	StatusPending:          {StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed},
	StatusPendingReference: {StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed},
}

func ParseStatus(s string) (Status, error) {
	switch st := Status(s); st {
	case StatusPending, StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed:
		return st, nil
	default:
		return "", fmt.Errorf("%w: unknown status %q", ErrInvalidState, s)
	}
}

func (s Status) String() string { return string(s) }

func (s Status) IsTerminal() bool {
	return s == StatusProcessed || s == StatusRejected || s == StatusFailed
}

func (s Status) CanTransitionTo(next Status) bool {
	return slices.Contains(allowedTransitions[s], next)
}
