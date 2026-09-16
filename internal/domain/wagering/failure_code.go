package wagering

import (
	"fmt"
	"maps"
)

type FailureCode string

type FailureCategory string

const (
	CategoryCorrectable    FailureCategory = "CORRECTABLE"
	CategoryDefinitive     FailureCategory = "DEFINITIVE"
	CategoryConflict       FailureCategory = "CONFLICT"
	CategoryInfrastructure FailureCategory = "INFRASTRUCTURE"
)

const (
	CodeMalformedRequest       FailureCode = "MALFORMED_REQUEST"
	CodeMissingField           FailureCode = "MISSING_FIELD"
	CodeInvalidField           FailureCode = "INVALID_FIELD"
	CodeInvalidAmount          FailureCode = "INVALID_AMOUNT"
	CodeInvalidCurrency        FailureCode = "INVALID_CURRENCY"
	CodeUnsupportedKind        FailureCode = "UNSUPPORTED_KIND"
	CodeOpeningNotAllowed      FailureCode = "OPENING_NOT_ALLOWED"
	CodeAmountMustBePositive   FailureCode = "AMOUNT_MUST_BE_POSITIVE"
	CodeLossAmountMustBeZero   FailureCode = "LOSS_AMOUNT_MUST_BE_ZERO"
	CodeReferenceRequired      FailureCode = "REFERENCE_REQUIRED"
	CodeReferenceNotAllowed    FailureCode = "REFERENCE_NOT_ALLOWED"
	CodeSelfReference          FailureCode = "SELF_REFERENCE"
	CodeWalletNotFound         FailureCode = "WALLET_NOT_FOUND"
	CodeWalletPlayerMismatch   FailureCode = "WALLET_PLAYER_MISMATCH"
	CodeWalletCurrencyMismatch FailureCode = "WALLET_CURRENCY_MISMATCH"

	CodeInsufficientFunds         FailureCode = "INSUFFICIENT_FUNDS"
	CodeReversalInsufficientFunds FailureCode = "REVERSAL_INSUFFICIENT_FUNDS"
	CodeBalanceLimitExceeded      FailureCode = "BALANCE_LIMIT_EXCEEDED"
	CodeReferenceNotFound         FailureCode = "REFERENCE_NOT_FOUND"
	CodeReferenceNotProcessed     FailureCode = "REFERENCE_NOT_PROCESSED"
	CodeReferenceAlreadyReversed  FailureCode = "REFERENCE_ALREADY_REVERSED"
	CodeReferenceKindInvalid      FailureCode = "REFERENCE_KIND_INVALID"
	CodeReferenceProviderMismatch FailureCode = "REFERENCE_PROVIDER_MISMATCH"
	CodeReferencePlayerMismatch   FailureCode = "REFERENCE_PLAYER_MISMATCH"
	CodeReferenceWalletMismatch   FailureCode = "REFERENCE_WALLET_MISMATCH"
	CodeReferenceRoundMismatch    FailureCode = "REFERENCE_ROUND_MISMATCH"
	CodeReferenceCurrencyMismatch FailureCode = "REFERENCE_CURRENCY_MISMATCH"
	CodeReferenceAmountMismatch   FailureCode = "REFERENCE_AMOUNT_MISMATCH"

	CodeIdempotencyKeyConflict      FailureCode = "IDEMPOTENCY_KEY_CONFLICT"
	CodeExternalTransactionConflict FailureCode = "EXTERNAL_TRANSACTION_CONFLICT"

	CodeInternalProcessingFailed FailureCode = "INTERNAL_PROCESSING_FAILED"
)

var failureCatalog = map[FailureCode]FailureCategory{
	CodeMalformedRequest:       CategoryCorrectable,
	CodeMissingField:           CategoryCorrectable,
	CodeInvalidField:           CategoryCorrectable,
	CodeInvalidAmount:          CategoryCorrectable,
	CodeInvalidCurrency:        CategoryCorrectable,
	CodeUnsupportedKind:        CategoryCorrectable,
	CodeOpeningNotAllowed:      CategoryCorrectable,
	CodeAmountMustBePositive:   CategoryCorrectable,
	CodeLossAmountMustBeZero:   CategoryCorrectable,
	CodeReferenceRequired:      CategoryCorrectable,
	CodeReferenceNotAllowed:    CategoryCorrectable,
	CodeSelfReference:          CategoryCorrectable,
	CodeWalletNotFound:         CategoryCorrectable,
	CodeWalletPlayerMismatch:   CategoryCorrectable,
	CodeWalletCurrencyMismatch: CategoryCorrectable,

	CodeInsufficientFunds:         CategoryDefinitive,
	CodeReversalInsufficientFunds: CategoryDefinitive,
	CodeBalanceLimitExceeded:      CategoryDefinitive,
	CodeReferenceNotFound:         CategoryDefinitive,
	CodeReferenceNotProcessed:     CategoryDefinitive,
	CodeReferenceAlreadyReversed:  CategoryDefinitive,
	CodeReferenceKindInvalid:      CategoryDefinitive,
	CodeReferenceProviderMismatch: CategoryDefinitive,
	CodeReferencePlayerMismatch:   CategoryDefinitive,
	CodeReferenceWalletMismatch:   CategoryDefinitive,
	CodeReferenceRoundMismatch:    CategoryDefinitive,
	CodeReferenceCurrencyMismatch: CategoryDefinitive,
	CodeReferenceAmountMismatch:   CategoryDefinitive,

	CodeIdempotencyKeyConflict:      CategoryConflict,
	CodeExternalTransactionConflict: CategoryConflict,

	CodeInternalProcessingFailed: CategoryInfrastructure,
}

func ParseFailureCode(s string) (FailureCode, error) {
	code := FailureCode(s)
	if _, ok := failureCatalog[code]; !ok {
		return "", fmt.Errorf("%w: unknown failure code %q", ErrInvalidState, s)
	}
	return code, nil
}

func (c FailureCode) Category() FailureCategory { return failureCatalog[c] }

func (c FailureCode) String() string { return string(c) }

func FailureCodes() map[FailureCode]FailureCategory {
	return maps.Clone(failureCatalog)
}
