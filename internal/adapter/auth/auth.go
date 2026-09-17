package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
)

const (
	PermissionTransactionsWrite = "wagering:transactions:write"
	PermissionTransactionsRead  = "wagering:transactions:read"
	PermissionWalletsWrite      = "wallets:write"
	PermissionWalletsRead       = "wallets:read"
	PermissionWalletsReconcile  = "wallets:reconcile"
)

var (
	ErrMissingToken    = errors.New("auth: missing bearer token")
	ErrInvalidToken    = errors.New("auth: invalid token")
	ErrExpiredToken    = errors.New("auth: token expired")
	ErrIDPUnavailable  = errors.New("auth: identity provider keys unavailable")
	internalPermission = []string{PermissionWalletsWrite, PermissionWalletsRead, PermissionWalletsReconcile}
)

type Principal struct {
	Subject     string
	ClientID    string
	ProviderID  string
	Permissions []string
	ExpiresAt   time.Time
}

func (p Principal) Has(permission string) bool {
	return slices.Contains(p.Permissions, permission)
}

func (p Principal) Actor() app.Actor {
	switch {
	case p.ProviderID != "":
		return app.ProviderActor(p.ClientID, p.ProviderID)
	case slices.ContainsFunc(internalPermission, p.Has):
		return app.ServiceActor(p.ClientID)
	default:
		return app.Actor{Subject: p.ClientID}
	}
}

type Settings struct {
	Issuer   string
	JWKSURL  string
	Audience string
	Now      func() time.Time
}

type Verifier struct {
	verifier *oidc.IDTokenVerifier
}

func NewVerifier(s Settings) (*Verifier, error) {
	switch {
	case s.Issuer == "":
		return nil, errors.New("auth: issuer is required")
	case s.JWKSURL == "":
		return nil, errors.New("auth: JWKS URL is required")
	case s.Audience == "":
		return nil, errors.New("auth: audience is required")
	}
	keySet := &trackingKeySet{inner: oidc.NewRemoteKeySet(context.Background(), s.JWKSURL)}
	return &Verifier{
		verifier: oidc.NewVerifier(s.Issuer, keySet, &oidc.Config{
			ClientID:             s.Audience,
			SupportedSigningAlgs: []string{oidc.RS256},
			Now:                  s.Now,
		}),
	}, nil
}

type claims struct {
	Subject         string `json:"sub"`
	AuthorizedParty string `json:"azp"`
	ClientID        string `json:"client_id"`
	ProviderID      string `json:"provider_id"`
	RealmAccess     struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

func (v *Verifier) Verify(ctx context.Context, rawToken string) (Principal, error) {
	if rawToken == "" {
		return Principal{}, ErrMissingToken
	}
	probe := &keyFetchProbe{}
	token, err := v.verifier.Verify(context.WithValue(ctx, keyFetchProbeKey{}, probe), rawToken)
	if err != nil {
		if _, expired := errors.AsType[*oidc.TokenExpiredError](err); expired {
			return Principal{}, fmt.Errorf("%w: %w", ErrExpiredToken, err)
		}
		if probe.unavailable.Load() {
			return Principal{}, fmt.Errorf("%w: %w", ErrIDPUnavailable, err)
		}
		return Principal{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}

	var c claims
	if err := token.Claims(&c); err != nil {
		return Principal{}, fmt.Errorf("%w: decode claims: %w", ErrInvalidToken, err)
	}
	clientID := c.AuthorizedParty
	if clientID == "" {
		clientID = c.ClientID
	}
	if clientID == "" || c.Subject == "" {
		return Principal{}, fmt.Errorf("%w: token has no subject or client", ErrInvalidToken)
	}
	return Principal{
		Subject:     c.Subject,
		ClientID:    clientID,
		ProviderID:  c.ProviderID,
		Permissions: c.RealmAccess.Roles,
		ExpiresAt:   token.Expiry,
	}, nil
}

type keyFetchProbeKey struct{}

type keyFetchProbe struct {
	unavailable atomic.Bool
}

type trackingKeySet struct {
	inner oidc.KeySet
}

func (k *trackingKeySet) VerifySignature(ctx context.Context, jwt string) ([]byte, error) {
	payload, err := k.inner.VerifySignature(ctx, jwt)
	if err != nil && strings.HasPrefix(err.Error(), "fetching keys") {
		if probe, ok := ctx.Value(keyFetchProbeKey{}).(*keyFetchProbe); ok {
			probe.unavailable.Store(true)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("auth: verify signature: %w", err)
	}
	return payload, nil
}
