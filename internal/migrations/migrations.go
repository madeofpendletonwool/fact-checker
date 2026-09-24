// Package migrations holds the embedded, versioned database migrations.
//
// Files are named NNNN_description.up.sql / NNNN_description.down.sql and are
// applied in order by golang-migrate, which also creates and tracks the
// schema_migrations table. Add a new pair per migration; never edit an
// applied migration.
//
// Each entry point opens its own connection from the DSN and closes it when
// done; callers that hold a pool keep theirs untouched.
package migrations

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	_ "github.com/jackc/pgx/v5/stdlib" // register the pgx database/sql driver
)

//go:embed *.sql
var migrationsFS embed.FS

// Up applies every pending up migration. A database already at head is not an
// error.
func Up(dsn string) error {
	m, err := newMigrate(dsn)
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// Down rolls back every applied migration, leaving an empty database (only
// schema_migrations remains, at zero). A database already at zero is not an
// error.
func Down(dsn string) error {
	m, err := newMigrate(dsn)
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("roll back migrations: %w", err)
	}
	return nil
}

// Version reports the currently applied migration version and whether the
// database is dirty. An empty database reports version 0.
func Version(dsn string) (uint, bool, error) {
	m, err := newMigrate(dsn)
	if err != nil {
		return 0, false, err
	}
	defer func() { _, _ = m.Close() }()
	v, dirty, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read migration version: %w", err)
	}
	return v, dirty, nil
}

func newMigrate(dsn string) (*migrate.Migrate, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	src, err := iofs.New(migrationsFS, ".")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("load migrations: %w", err)
	}
	drv, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init migration driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", drv)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init migrate: %w", err)
	}
	return m, nil
}
