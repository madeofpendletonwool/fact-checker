package migrations

import (
	"context"
	"io/fs"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/madeofpendletonwool/fact-checker/internal/testdb"
)

func TestUpDownUpCycle(t *testing.T) {
	url := testdb.Acquire(t, "migrations")

	ctx := context.Background()
	pool := newTestPool(ctx, t, url)

	expected := expectedVersion(t)

	if err := Down(url); err != nil {
		t.Fatalf("initial Down: %v", err)
	}
	assertVersion(t, url, 0, "empty database")

	if err := Up(url); err != nil {
		t.Fatalf("Up: %v", err)
	}
	assertVersion(t, url, expected, "head after Up")
	assertTableExists(ctx, t, pool, "sources", true)
	assertTableExists(ctx, t, pool, "review_items", true)

	if err := Down(url); err != nil {
		t.Fatalf("Down: %v", err)
	}
	assertVersion(t, url, 0, "empty database after Down")
	assertTableExists(ctx, t, pool, "sources", false)

	if err := Up(url); err != nil {
		t.Fatalf("Up again: %v", err)
	}
	assertVersion(t, url, expected, "head after re-Up")
}

func expectedVersion(t *testing.T) uint {
	t.Helper()
	matches, err := fs.Glob(migrationsFS, "*.up.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	return uint(len(matches))
}

func assertVersion(t *testing.T, dsn string, want uint, label string) {
	t.Helper()
	v, dirty, err := Version(dsn)
	if err != nil {
		t.Fatalf("Version (%s): %v", label, err)
	}
	if dirty {
		t.Fatalf("(%s) schema_migrations is dirty", label)
	}
	if v != want {
		t.Fatalf("(%s) version = %d, want %d", label, v, want)
	}
}

func assertTableExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table string, want bool) {
	t.Helper()
	var n int
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema = 'public' AND table_name = $1`, table).Scan(&n)
	if err != nil {
		t.Fatalf("probe table %s: %v", table, err)
	}
	if (n > 0) != want {
		t.Fatalf("table %s exists = %t, want %t", table, n > 0, want)
	}
}

func newTestPool(ctx context.Context, t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		t.Fatalf("ping scratch database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
