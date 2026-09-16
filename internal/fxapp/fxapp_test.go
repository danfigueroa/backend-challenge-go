package fxapp_test

import (
	"testing"

	"go.uber.org/fx"

	"github.com/danfigueroa/backend-challenge-go/internal/fxapp"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/config"
)

func baseConfig(t *testing.T, roles ...config.Role) config.Config {
	t.Setenv("DATABASE_URL", "postgres://user:pass@127.0.0.1:1/wallet")
	t.Setenv("APP_INSTANCE_ID", "validate")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.App.Roles = roles
	return cfg
}

func TestApplicationGraphIsValidForEveryRoleCombination(t *testing.T) {
	combinations := [][]config.Role{
		{config.RoleAPI, config.RoleConsumer, config.RoleOutbox, config.RolePendingRef},
		{config.RoleAPI},
		{config.RoleConsumer},
		{config.RoleOutbox},
		{config.RolePendingRef},
		{config.RoleAPI, config.RolePendingRef},
	}
	for _, roles := range combinations {
		if err := fx.ValidateApp(fxapp.Options(baseConfig(t, roles...))); err != nil {
			t.Errorf("roles %v: %v", roles, err)
		}
	}
}
