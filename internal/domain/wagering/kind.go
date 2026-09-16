package wagering

import "fmt"

type Kind string

const (
	KindOpening  Kind = "OPENING"
	KindBet      Kind = "BET"
	KindWin      Kind = "WIN"
	KindLoss     Kind = "LOSS"
	KindRefund   Kind = "REFUND"
	KindRollback Kind = "ROLLBACK"
)

func ParseKind(s string) (Kind, error) {
	switch k := Kind(s); k {
	case KindOpening, KindBet, KindWin, KindLoss, KindRefund, KindRollback:
		return k, nil
	default:
		return "", fmt.Errorf("%w: unknown kind %q", ErrInvalidState, s)
	}
}

func parseExternalKind(s string) (Kind, error) {
	switch k := Kind(s); k {
	case KindBet, KindWin, KindLoss, KindRefund, KindRollback:
		return k, nil
	case KindOpening:
		return "", newValidationError(CodeOpeningNotAllowed, "kind", "OPENING is reserved for internal wallet opening")
	default:
		return "", newValidationError(CodeUnsupportedKind, "kind", fmt.Sprintf("unsupported kind %q", s))
	}
}

func (k Kind) String() string { return string(k) }

func (k Kind) RequiresReference() bool { return k == KindRefund || k == KindRollback }

func (k Kind) AcceptsReference() bool { return k.RequiresReference() || k == KindWin }

func (k Kind) IsReversal() bool { return k.RequiresReference() }

func (k Kind) canBeReferencedBy(referrer Kind) bool {
	switch referrer {
	case KindWin, KindRefund:
		return k == KindBet
	case KindRollback:
		return k == KindBet || k == KindWin || k == KindRefund
	default:
		return false
	}
}
