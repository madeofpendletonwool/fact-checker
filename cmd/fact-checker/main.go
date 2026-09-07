// Command fact-checker runs the sourced-claims fact-checking engine.
//
// Subcommands:
//
//	fact-checker serve    apply migrations, then serve the HTTP surface
//	fact-checker migrate  apply migrations and exit
//	fact-checker version  print the build version
//
// Pipeline stage subcommands (extract, validate, factcheck, review) land in
// later stages of the build-out; see docs/adr.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/madeofpendletonwool/fact-checker/internal/config"
	"github.com/madeofpendletonwool/fact-checker/internal/database"
	"github.com/madeofpendletonwool/fact-checker/internal/migrations"
	"github.com/madeofpendletonwool/fact-checker/internal/server"
)

var version = "dev"

const usage = `fact-checker — the sourced-claims fact-checking engine

Usage:

  fact-checker serve    apply migrations, then serve the HTTP surface
  fact-checker migrate  apply migrations and exit
  fact-checker version  print the build version

Configuration comes from the environment; see .env.example for every
variable and its default.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fact-checker:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "serve":
		return serve()
	case "migrate":
		return migrateOnly()
	case "version":
		fmt.Println(version)
		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return errors.New("missing or unknown command")
	}
}

func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.ValidateDatabase(); err != nil {
		return err
	}
	logger := newLogger(cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := applyMigrations(pool); err != nil {
		return err
	}

	logger.Info("listening", "addr", cfg.ListenAddr, "version", version)
	return server.Run(ctx, pool, cfg.ListenAddr)
}

func migrateOnly() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.ValidateDatabase(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	return applyMigrations(pool)
}

func applyMigrations(pool *pgxpool.Pool) error {
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()
	return migrations.Up(sqlDB)
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
