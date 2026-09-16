package walletapp

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wallet"
)

const (
	DefaultLedgerPageSize = 50
	MaxLedgerPageSize     = 200
	cursorVersion         = 1
)

func (s *Service) GetWallet(ctx context.Context, actor app.Actor, walletID uuid.UUID) (WalletView, error) {
	if err := actor.RequireInternalService(); err != nil {
		return WalletView{}, err
	}
	w, err := s.wallets.Get(ctx, walletID)
	if err != nil {
		return WalletView{}, err
	}
	return viewOf(w), nil
}

type LedgerQuery struct {
	Actor    app.Actor
	WalletID uuid.UUID
	Cursor   string
	Limit    int
}

type LedgerPage struct {
	Entries    []wallet.LedgerEntry
	NextCursor string
}

type ledgerCursor struct {
	Version      int    `json:"v"`
	WalletID     string `json:"w"`
	AfterVersion int64  `json:"a"`
}

func (s *Service) ListLedger(ctx context.Context, q LedgerQuery) (LedgerPage, error) {
	if err := q.Actor.RequireInternalService(); err != nil {
		return LedgerPage{}, err
	}
	limit := q.Limit
	switch {
	case limit == 0:
		limit = DefaultLedgerPageSize
	case limit < 1 || limit > MaxLedgerPageSize:
		return LedgerPage{}, &app.ValidationError{Code: "INVALID_FIELD", Field: "limit", Reason: "must be between 1 and 200"}
	}
	after, err := decodeCursor(q.Cursor, q.WalletID)
	if err != nil {
		return LedgerPage{}, err
	}

	var page LedgerPage
	err = s.tx.WithinSnapshot(ctx, func(ctx context.Context) error {
		if _, err := s.wallets.Get(ctx, q.WalletID); err != nil {
			return err
		}
		entries, err := s.ledger.List(ctx, q.WalletID, after, limit+1)
		if err != nil {
			return err
		}
		if len(entries) > limit {
			entries = entries[:limit]
			page.NextCursor = encodeCursor(q.WalletID, entries[len(entries)-1].WalletVersion())
		}
		page.Entries = entries
		return nil
	})
	if err != nil {
		return LedgerPage{}, err
	}
	return page, nil
}

func encodeCursor(walletID uuid.UUID, afterVersion int64) string {
	data, _ := json.Marshal(ledgerCursor{Version: cursorVersion, WalletID: walletID.String(), AfterVersion: afterVersion})
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeCursor(cursor string, walletID uuid.UUID) (int64, error) {
	if cursor == "" {
		return 0, nil
	}
	invalid := &app.ValidationError{Code: "INVALID_CURSOR", Field: "cursor", Reason: "is malformed or belongs to another wallet"}
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, invalid
	}
	var c ledgerCursor
	if err := json.Unmarshal(data, &c); err != nil {
		return 0, invalid
	}
	if c.Version != cursorVersion || c.WalletID != walletID.String() || c.AfterVersion < 1 {
		return 0, invalid
	}
	return c.AfterVersion, nil
}
