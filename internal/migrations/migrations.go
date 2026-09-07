// Package migrations holds the embedded, versioned database migrations.
//
// Files are named NNNN_description.up.sql / NNNN_description.down.sql and are
// applied in order by golang-migrate, which also creates and tracks the
// schema_migrations table. Add a new pair per migration; never edit an
// applied migration.
package migrations

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed *.sql
var migrationsFS embed.FS

// Up applies every pending up migration. A database already at head is not an
// error.
func Up(db *sql.DB) error {
	src, err := iofs.New(migrationsFS, ".")
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	drv, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		return fmt.Errorf("init migration driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", drv)
	if err != nil {
		return fmt.Errorf("init migrate: %w", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
