//go:build integration

package pgtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/danfigueroa/backend-challenge-go/internal/adapter/postgres"
)

const (
	Image         = "postgres:18.6-alpine3.24"
	OwnerUser     = "wallet_owner"
	OwnerPassword = "owner-test-password"
	AppUser       = "wallet_service"
	AppPassword   = "service-test-password"
	TemplateDB    = "wallet_template"
)

type Instance struct {
	container *tcpostgres.PostgresContainer
	baseURL   *url.URL
	admin     *pgxpool.Pool
	sequence  atomic.Int64
}

type Database struct {
	Name      string
	OwnerDSN  string
	AppDSN    string
	OwnerPool *pgxpool.Pool
	AppPool   *pgxpool.Pool
}

func RepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

func Start(ctx context.Context) (*Instance, error) {
	container, err := tcpostgres.Run(ctx, Image,
		tcpostgres.WithDatabase(TemplateDB),
		tcpostgres.WithUsername(OwnerUser),
		tcpostgres.WithPassword(OwnerPassword),
		tcpostgres.WithInitScripts(filepath.Join(RepoRoot(), "deploy", "postgres", "init", "01-create-app-user.sh")),
		testcontainers.WithEnv(map[string]string{"APP_DB_USER": AppUser, "APP_DB_PASSWORD": AppPassword}),
		testcontainers.WithCmd("postgres", "-c", "fsync=off", "-c", "max_connections=1000"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		return nil, fmt.Errorf("start postgres: %w", err)
	}

	instance := &Instance{container: container}
	if err := instance.init(ctx); err != nil {
		_ = testcontainers.TerminateContainer(container)
		return nil, err
	}
	return instance, nil
}

func (i *Instance) init(ctx context.Context) error {
	dsn, err := i.container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return fmt.Errorf("postgres connection string: %w", err)
	}
	if i.baseURL, err = url.Parse(dsn); err != nil {
		return fmt.Errorf("parse postgres DSN: %w", err)
	}

	migrator, err := postgres.NewMigrator(i.DSN(TemplateDB, OwnerUser, OwnerPassword))
	if err != nil {
		return err
	}
	if err := migrator.Up(); err != nil {
		_ = migrator.Close()
		return err
	}
	if err := migrator.Close(); err != nil {
		return fmt.Errorf("close migrator: %w", err)
	}

	i.admin, err = pgxpool.New(ctx, i.DSN("postgres", OwnerUser, OwnerPassword))
	if err != nil {
		return fmt.Errorf("connect admin pool: %w", err)
	}
	return nil
}

func (i *Instance) DSN(database, user, password string) string {
	u := *i.baseURL
	u.User = url.UserPassword(user, password)
	u.Path = "/" + database
	return u.String()
}

func (i *Instance) Terminate(ctx context.Context) error {
	if i.admin != nil {
		i.admin.Close()
	}
	return testcontainers.TerminateContainer(i.container, testcontainers.StopContext(ctx))
}

func (i *Instance) NewDatabase(tb testing.TB) *Database {
	tb.Helper()
	return i.createDatabase(tb, TemplateDB)
}

func (i *Instance) NewEmptyDatabase(tb testing.TB) *Database {
	tb.Helper()
	return i.createDatabase(tb, "template0")
}

func (i *Instance) createDatabase(tb testing.TB, template string) *Database {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	name := fmt.Sprintf("test_%d_%d", os.Getpid(), i.sequence.Add(1))
	if _, err := i.admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", pgx.Identifier{name}.Sanitize(), pgx.Identifier{template}.Sanitize())); err != nil {
		tb.Fatalf("create database %s: %v", name, err)
	}

	db := &Database{
		Name:     name,
		OwnerDSN: i.DSN(name, OwnerUser, OwnerPassword),
		AppDSN:   i.DSN(name, AppUser, AppPassword),
	}
	var err error
	if db.OwnerPool, err = pgxpool.New(ctx, db.OwnerDSN); err != nil {
		tb.Fatalf("connect owner pool: %v", err)
	}
	if db.AppPool, err = pgxpool.New(ctx, db.AppDSN); err != nil {
		tb.Fatalf("connect app pool: %v", err)
	}

	tb.Cleanup(func() {
		db.AppPool.Close()
		db.OwnerPool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), time.Minute)
		defer dropCancel()
		if _, err := i.admin.Exec(dropCtx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", pgx.Identifier{name}.Sanitize())); err != nil {
			tb.Errorf("drop database %s: %v", name, err)
		}
	})
	return db
}

func RunMain(m *testing.M, instance **Instance) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	started, err := Start(ctx)
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: %v\n", err)
		return 1
	}
	*instance = started

	code := m.Run()

	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Minute)
	defer stopCancel()
	if err := started.Terminate(stopCtx); err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: terminate: %v\n", err)
	}
	return code
}
