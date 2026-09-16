package postgres

import (
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/danfigueroa/backend-challenge-go/migrations"
)

type Migrator struct {
	m *migrate.Migrate
}

type MigrationStatus struct {
	Version uint
	Dirty   bool
	Empty   bool
}

func NewMigrator(dsn string) (*Migrator, error) {
	connCfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("migrate: parse DSN: %w", err)
	}
	db := stdlib.OpenDB(*connCfg)

	driver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("migrate: connect: %w", err), db.Close())
	}
	source, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, errors.Join(fmt.Errorf("migrate: load embedded migrations: %w", err), driver.Close())
	}
	m, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("migrate: init: %w", err), driver.Close())
	}
	return &Migrator{m: m}, nil
}

func (m *Migrator) Up() error {
	if err := m.m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

func (m *Migrator) Down(steps int) error {
	if steps < 1 {
		return fmt.Errorf("migrate down: steps must be positive, got %d", steps)
	}
	if err := m.m.Steps(-steps); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate down: %w", err)
	}
	return nil
}

func (m *Migrator) Status() (MigrationStatus, error) {
	version, dirty, err := m.m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return MigrationStatus{Empty: true}, nil
	}
	if err != nil {
		return MigrationStatus{}, fmt.Errorf("migrate version: %w", err)
	}
	return MigrationStatus{Version: version, Dirty: dirty}, nil
}

func (m *Migrator) Close() error {
	sourceErr, dbErr := m.m.Close()
	return errors.Join(sourceErr, dbErr)
}
