// Command fact-checker runs the sourced-claims fact-checking engine.
//
// Subcommands:
//
//	fact-checker serve      apply migrations, then serve the HTTP surface
//	fact-checker migrate    apply migrations and exit
//	fact-checker integrity  run integrity checks; exit 1 on error findings
//	fact-checker version    print the build version
//
// Pipeline stage subcommands (extract, validate, factcheck, review) land in
// later stages of the build-out; see docs/adr.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/madeofpendletonwool/fact-checker/internal/config"
	"github.com/madeofpendletonwool/fact-checker/internal/database"
	"github.com/madeofpendletonwool/fact-checker/internal/integrity"
	"github.com/madeofpendletonwool/fact-checker/internal/migrations"
	"github.com/madeofpendletonwool/fact-checker/internal/server"
)

var version = "dev"

const usage = `fact-checker — the sourced-claims fact-checking engine

Usage:

  fact-checker serve      apply migrations, then serve the HTTP surface
  fact-checker migrate    apply migrations and exit
  fact-checker integrity  run integrity checks; exit 1 on error findings
  fact-checker version    print the build version

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
	case "integrity":
		return integrityCmd()
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

	if err := applyMigrations(cfg.DatabaseURL); err != nil {
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

	return applyMigrations(cfg.DatabaseURL)
}

// integrityCmd audits the current database state. It applies no migrations:
// it reports what is, and exits 1 when any error-severity check finds data
// (uncited claims, invalid locators, dangling references).
func integrityCmd() error {
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

	results, err := integrity.Run(ctx, pool)
	if err != nil {
		return err
	}
	printIntegrityReport(results)
	if err := recordIntegrityRun(ctx, pool, results); err != nil {
		return fmt.Errorf("record integrity run: %w", err)
	}

	if n := integrity.ErrorCount(results); n > 0 {
		return fmt.Errorf("%d integrity error(s); see report above", n)
	}
	return nil
}

func printIntegrityReport(results []integrity.Result) {
	fmt.Printf("%-36s %-8s %5s  %s\n", "CHECK", "SEVERITY", "COUNT", "SAMPLES")
	for _, r := range results {
		samples := r.Samples
		if len(samples) > 5 {
			samples = samples[:5]
		}
		fmt.Printf("%-36s %-8s %5d  %s\n",
			r.Check.Code, r.Check.Severity, r.Count, strings.Join(samples, ", "))
	}
	fmt.Printf("errors: %d, warnings: %d\n",
		integrity.ErrorCount(results), integrity.WarningCount(results))
}

func recordIntegrityRun(ctx context.Context, pool *pgxpool.Pool, results []integrity.Result) error {
	checks := make(map[string]int, len(results))
	for _, r := range results {
		checks[r.Check.Code] = r.Count
	}
	stats, err := json.Marshal(map[string]any{
		"errors":   integrity.ErrorCount(results),
		"warnings": integrity.WarningCount(results),
		"checks":   checks,
	})
	if err != nil {
		return fmt.Errorf("marshal stats: %w", err)
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO pipeline_runs (run_kind, status, finished_at, stats)
		 VALUES ('integrity_check', $1, now(), $2::jsonb)`,
		statusFor(integrity.ErrorCount(results)), string(stats))
	return err
}

func statusFor(errCount int) string {
	if errCount > 0 {
		return "failed"
	}
	return "ok"
}

func applyMigrations(dsn string) error {
	return migrations.Up(dsn)
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
