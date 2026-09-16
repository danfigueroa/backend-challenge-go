package wagering

import (
	"errors"
	"fmt"
)

var (
	ErrValidation        = errors.New("wagering: validation failed")
	ErrInvalidTransition = errors.New("wagering: invalid state transition")
	ErrInvalidState      = errors.New("wagering: invalid transaction state")
	ErrWalletMismatch    = errors.New("wagering: wallet does not match transaction")
)

type ValidationError struct {
	Code   FailureCode
	Field  string
	Reason string
}

func NewValidationError(code FailureCode, field, reason string) *ValidationError {
	return &ValidationError{Code: code, Field: field, Reason: reason}
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("wagering: %s: %s", e.Code, e.Reason)
	}
	return fmt.Sprintf("wagering: %s: %s: %s", e.Code, e.Field, e.Reason)
}

func (e *ValidationError) Is(target error) bool {
	return target == ErrValidation
}
