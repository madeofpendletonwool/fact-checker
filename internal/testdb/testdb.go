// Package testdb provisions per-package scratch databases for DB-backed
// tests, derived from FACTCHECK_TEST_DATABASE_URL. Each package gets its own
// database so package tests can run in parallel against one Postgres.
package testdb

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Acquire drops and recreates a scratch database named after the base
// database plus the suffix, and returns its URL. Callers do not need to
// clean up beyond what migrations.Down does; the next run recreates it.
// Skips the test when FACTCHECK_TEST_DATABASE_URL is unset.
func Acquire(t testing.TB, suffix string) string {
	t.Helper()

	base := os.Getenv("FACTCHECK_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("FACTCHECK_TEST_DATABASE_URL not set; skipping scratch-database test")
	}

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse FACTCHECK_TEST_DATABASE_URL: %v", err)
	}
	name := strings.TrimPrefix(u.Path, "/")
	if name == "" {
		t.Fatalf("FACTCHECK_TEST_DATABASE_URL must name a database")
	}
	scratch := name + "_" + suffix
	u.Path = "/" + scratch

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg, err := pgx.ParseConfig(base)
	if err != nil {
		t.Fatalf("parse connection config: %v", err)
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect scratch server: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	ident := pgx.Identifier{scratch}.Sanitize()
	if _, err := conn.Exec(ctx,
		fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", ident)); err != nil {
		t.Fatalf("drop scratch database: %v", err)
	}
	if _, err := conn.Exec(ctx,
		fmt.Sprintf("CREATE DATABASE %s", ident)); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	return u.String()
}
