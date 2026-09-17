//go:build integration

package kctest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/danfigueroa/backend-challenge-go/internal/testsupport/pgtest"
)

const (
	Image    = "quay.io/keycloak/keycloak:26.7.4"
	Realm    = "wagering"
	Audience = "wallet-api"
)

var Secrets = map[string]string{
	"provider-a":             "provider-a-local-secret",
	"provider-b":             "provider-b-local-secret",
	"wallet-internal":        "wallet-internal-local-secret",
	"provider-a-short-lived": "provider-a-short-lived-local-secret",
	"provider-unprivileged":  "provider-unprivileged-local-secret",
	"foreign-audience":       "foreign-audience-local-secret",
}

type Instance struct {
	container testcontainers.Container
	BaseURL   string
	client    *http.Client
}

func Start(ctx context.Context) (*Instance, error) {
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        Image,
			ExposedPorts: []string{"8080/tcp"},
			Env: map[string]string{
				"KC_BOOTSTRAP_ADMIN_USERNAME": "admin",
				"KC_BOOTSTRAP_ADMIN_PASSWORD": "admin",
			},
			Cmd: []string{"start-dev", "--import-realm"},
			Files: []testcontainers.ContainerFile{{
				HostFilePath:      filepath.Join(pgtest.RepoRoot(), "deploy", "keycloak", "realm-wagering.json"),
				ContainerFilePath: "/opt/keycloak/data/import/realm-wagering.json",
				FileMode:          0o644,
			}},
			WaitingFor: wait.ForHTTP("/realms/" + Realm + "/.well-known/openid-configuration").
				WithPort("8080/tcp").WithStartupTimeout(4 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		return nil, fmt.Errorf("start keycloak: %w", err)
	}
	endpoint, err := container.PortEndpoint(ctx, "8080/tcp", "http")
	if err != nil {
		_ = testcontainers.TerminateContainer(container)
		return nil, fmt.Errorf("keycloak endpoint: %w", err)
	}
	return &Instance{container: container, BaseURL: endpoint, client: &http.Client{Timeout: 10 * time.Second}}, nil
}

func (i *Instance) Issuer() string { return i.BaseURL + "/realms/" + Realm }

func (i *Instance) JWKSURL() string { return i.Issuer() + "/protocol/openid-connect/certs" }

func (i *Instance) Token(ctx context.Context, clientID string) (string, error) {
	secret, ok := Secrets[clientID]
	if !ok {
		return "", fmt.Errorf("unknown test client %q", clientID)
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {secret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, i.Issuer()+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := i.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request token: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || body.AccessToken == "" {
		return "", fmt.Errorf("token request for %s failed: %d %s", clientID, resp.StatusCode, body.Error)
	}
	return body.AccessToken, nil
}

func (i *Instance) Terminate(ctx context.Context) error {
	return testcontainers.TerminateContainer(i.container, testcontainers.StopContext(ctx))
}
