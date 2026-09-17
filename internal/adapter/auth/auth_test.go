package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/danfigueroa/backend-challenge-go/internal/adapter/auth"
	"github.com/danfigueroa/backend-challenge-go/internal/app"
)

const issuer = "http://idp.test/realms/wagering"

type idp struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	kid    string
}

func newIDP(t *testing.T) *idp {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	i := &idp{key: key, kid: "test-key"}
	i.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: i.kid, Algorithm: "RS256", Use: "sig"}}})
	}))
	t.Cleanup(i.server.Close)
	return i
}

func (i *idp) sign(t *testing.T, key *rsa.PrivateKey, alg jose.SignatureAlgorithm, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", i.kid).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func validClaims(overrides map[string]any) map[string]any {
	now := time.Now()
	c := map[string]any{
		"iss": issuer, "aud": "wallet-api", "sub": "sa-provider-a", "azp": "provider-a",
		"provider_id": "provider-a", "exp": now.Add(5 * time.Minute).Unix(), "iat": now.Unix(),
		"realm_access": map[string]any{"roles": []string{auth.PermissionTransactionsWrite, auth.PermissionTransactionsRead}},
	}
	for k, v := range overrides {
		if v == nil {
			delete(c, k)
			continue
		}
		c[k] = v
	}
	return c
}

func verifier(t *testing.T, jwksURL string) *auth.Verifier {
	t.Helper()
	v, err := auth.NewVerifier(auth.Settings{Issuer: issuer, JWKSURL: jwksURL, Audience: "wallet-api"})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVerifyValidProviderToken(t *testing.T) {
	t.Parallel()
	i := newIDP(t)

	p, err := verifier(t, i.server.URL).Verify(context.Background(), i.sign(t, i.key, jose.RS256, validClaims(nil)))
	if err != nil {
		t.Fatal(err)
	}
	if p.ClientID != "provider-a" || p.ProviderID != "provider-a" || p.Subject != "sa-provider-a" ||
		!p.Has(auth.PermissionTransactionsWrite) || p.Has(auth.PermissionWalletsWrite) || p.ExpiresAt.IsZero() {
		t.Errorf("principal = %+v", p)
	}
	if actor := p.Actor(); actor.Kind != app.ActorProvider || actor.ProviderID != "provider-a" {
		t.Errorf("actor = %+v", actor)
	}
}

func TestVerifyRejectsInvalidTokens(t *testing.T) {
	t.Parallel()
	i := newIDP(t)
	v := verifier(t, i.server.URL)
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	tests := map[string]struct {
		token string
		want  error
	}{
		"empty":            {"", auth.ErrMissingToken},
		"garbage":          {"not.a.jwt", auth.ErrInvalidToken},
		"expired":          {i.sign(t, i.key, jose.RS256, validClaims(map[string]any{"exp": time.Now().Add(-time.Minute).Unix()})), auth.ErrExpiredToken},
		"wrong issuer":     {i.sign(t, i.key, jose.RS256, validClaims(map[string]any{"iss": "http://evil.test/realms/wagering"})), auth.ErrInvalidToken},
		"wrong audience":   {i.sign(t, i.key, jose.RS256, validClaims(map[string]any{"aud": "another-api"})), auth.ErrInvalidToken},
		"missing audience": {i.sign(t, i.key, jose.RS256, validClaims(map[string]any{"aud": nil})), auth.ErrInvalidToken},
		"foreign key":      {i.sign(t, otherKey, jose.RS256, validClaims(nil)), auth.ErrInvalidToken},
		"weak algorithm":   {i.sign(t, i.key, jose.PS256, validClaims(nil)), auth.ErrInvalidToken},
		"missing subject":  {i.sign(t, i.key, jose.RS256, validClaims(map[string]any{"sub": nil})), auth.ErrInvalidToken},
		"missing client":   {i.sign(t, i.key, jose.RS256, validClaims(map[string]any{"azp": nil})), auth.ErrInvalidToken},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := v.Verify(context.Background(), tc.token); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}

	valid := i.sign(t, i.key, jose.RS256, validClaims(nil))
	parts := strings.Split(valid, ".")
	tampered := parts[0] + "." + strings.TrimRight(parts[1], "=") + "x." + parts[2]
	if _, err := v.Verify(context.Background(), tampered); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("tampered payload: %v", err)
	}
}

func TestVerifyReportsUnavailableIdentityProvider(t *testing.T) {
	t.Parallel()
	i := newIDP(t)
	token := i.sign(t, i.key, jose.RS256, validClaims(nil))

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	down.Close()

	if _, err := verifier(t, down.URL).Verify(context.Background(), token); !errors.Is(err, auth.ErrIDPUnavailable) {
		t.Errorf("error = %v, want ErrIDPUnavailable", err)
	}
}

func TestPrincipalActorMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		p    auth.Principal
		kind app.ActorKind
	}{
		{"provider", auth.Principal{ClientID: "provider-a", ProviderID: "provider-a", Permissions: []string{auth.PermissionTransactionsWrite}}, app.ActorProvider},
		{"internal service", auth.Principal{ClientID: "wallet-internal", Permissions: []string{auth.PermissionWalletsRead}}, app.ActorService},
		{"provider claim wins over wallet roles", auth.Principal{ClientID: "x", ProviderID: "provider-a", Permissions: []string{auth.PermissionWalletsWrite}}, app.ActorProvider},
		{"no permissions", auth.Principal{ClientID: "nobody"}, ""},
		{"transaction reader without provider", auth.Principal{ClientID: "reader", Permissions: []string{auth.PermissionTransactionsRead}}, ""},
	}
	for _, tc := range tests {
		if got := tc.p.Actor().Kind; got != tc.kind {
			t.Errorf("%s: actor kind = %q, want %q", tc.name, got, tc.kind)
		}
	}

	for _, s := range []auth.Settings{
		{JWKSURL: "x", Audience: "a"},
		{Issuer: "i", Audience: "a"},
		{Issuer: "i", JWKSURL: "x"},
	} {
		if _, err := auth.NewVerifier(s); err == nil {
			t.Errorf("NewVerifier(%+v) accepted incomplete settings", s)
		}
	}
}
