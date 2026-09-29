// Command fact-checker runs the sourced-claims fact-checking engine.
//
// Subcommands:
//
//	fact-checker serve            apply migrations, then serve the HTTP surface
//	fact-checker migrate          apply migrations and exit
//	fact-checker integrity        run integrity checks; exit 1 on error findings
//	fact-checker extract run      run the cite-or-drop extraction stage
//	fact-checker extract status   summarize the extraction ledger
//	fact-checker validate run     run the adversarial + deterministic validation stage
//	fact-checker validate status  summarize the verdict ledger and open flags
//	fact-checker version          print the build version
//
// Later pipeline stages (factcheck, review) land as they are built; see
// docs/adr.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/madeofpendletonwool/fact-checker/adapters"
	_ "github.com/madeofpendletonwool/fact-checker/adapters/arda"
	"github.com/madeofpendletonwool/fact-checker/extract"
	"github.com/madeofpendletonwool/fact-checker/internal/config"
	"github.com/madeofpendletonwool/fact-checker/internal/database"
	"github.com/madeofpendletonwool/fact-checker/internal/integrity"
	"github.com/madeofpendletonwool/fact-checker/internal/llm"
	"github.com/madeofpendletonwool/fact-checker/internal/migrations"
	"github.com/madeofpendletonwool/fact-checker/internal/server"
	"github.com/madeofpendletonwool/fact-checker/validate"
)

var version = "dev"

const usage = `fact-checker — the sourced-claims fact-checking engine

Usage:

  fact-checker serve            apply migrations, then serve the HTTP surface
  fact-checker migrate          apply migrations and exit
  fact-checker integrity        run integrity checks; exit 1 on error findings
  fact-checker extract run      run the cite-or-drop extraction stage
  fact-checker extract status   summarize the extraction ledger
  fact-checker validate run     run the adversarial + deterministic validation stage
  fact-checker validate status  summarize the verdict ledger and open flags
  fact-checker version          print the build version

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
	case "extract":
		return extractCmd(args[1:])
	case "validate":
		return validateCmd(args[1:])
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

// extractCmd drives the extraction stage: `extract run` executes a run,
// `extract status` summarizes the ledger.
func extractCmd(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "run":
		return extractRunCmd(args[1:])
	case "status":
		return extractStatusCmd()
	default:
		_, _ = fmt.Fprint(os.Stderr, `usage: fact-checker extract run    run the cite-or-drop extraction stage
       fact-checker extract status summarize the extraction ledger
`)
		return errors.New("missing or unknown extract subcommand")
	}
}

func extractRunCmd(args []string) error {
	flags := flag.NewFlagSet("extract run", flag.ContinueOnError)
	flags.Usage = func() {
		_, _ = fmt.Fprint(flags.Output(), `usage: fact-checker extract run --adapter <manifest.json> [flags]

Runs the extraction stage over the adapter's work units. Cite-or-drop:
records that cannot satisfy the citation contract are dropped and logged,
never kept.
`)
		flags.PrintDefaults()
	}
	adapterPath := flags.String("adapter", "", "path to the adapter manifest (required)")
	limit := flags.Int("limit", 0, "bound the run to N pending units; the rest stay deferred")
	dryRun := flags.Bool("dry-run", false, "plan the run and report estimates without model calls")
	budget := flags.Float64("budget", 0, "soft USD spend cap (0 = config default; unpriced runs fall back to the unit cap)")
	maxUnits := flags.Int("max-units", 0, "hard per-run unit cap (0 = config default)")
	concurrency := flags.Int("concurrency", 0, "in-flight model calls (0 = config default)")
	maxAttempts := flags.Int("max-attempts", 3, "model calls per unit, including JSON repairs")
	quiet := flags.Bool("quiet", false, "suppress per-unit progress lines")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *adapterPath == "" {
		flags.Usage()
		return errors.New("--adapter is required")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.ValidateDatabase(); err != nil {
		return err
	}

	loaded, err := adapters.LoadManifest(*adapterPath)
	if err != nil {
		return err
	}
	adapter, ok := loaded.(extract.Adapter)
	if !ok {
		return fmt.Errorf("adapter %s does not implement the extraction contract", *adapterPath)
	}

	var client llm.Completer
	if !*dryRun {
		if cfg.AIKey == "" || cfg.Models[config.StageExtract] == "" {
			return errors.New("extraction needs FACTCHECK_AI_API_KEY and FACTCHECK_MODEL_EXTRACT (or --dry-run)")
		}
		interval := cfg.RequestMinInterval
		if override, ok := cfg.StageIntervals[config.StageExtract]; ok && override > 0 {
			interval = override
		}
		client, err = llm.NewClient(cfg.AIKey, cfg.Models[config.StageExtract], cfg.AIBaseURL, llm.ClientOptions{
			RequestInterval: interval,
			Timeout:         cfg.AITimeout,
		})
		if err != nil {
			return err
		}
	}

	opts := extract.DefaultOptions()
	if *budget > 0 {
		opts.BudgetUSD = *budget
	} else if cfg.BudgetPerRunUSD > 0 {
		opts.BudgetUSD = cfg.BudgetPerRunUSD
	}
	if *maxUnits > 0 {
		opts.MaxUnits = *maxUnits
	} else {
		opts.MaxUnits = cfg.ExtractMaxUnits
	}
	if *concurrency > 0 {
		opts.Concurrency = *concurrency
	} else {
		opts.Concurrency = cfg.ExtractConcurrency
	}
	opts.Limit = *limit
	opts.DryRun = *dryRun
	opts.MaxAttempts = *maxAttempts
	opts.PriceInMTok = cfg.Prices[config.StageExtract].Input
	opts.PriceOutMTok = cfg.Prices[config.StageExtract].Output
	if !*quiet {
		opts.Log = func(format string, args ...any) {
			fmt.Printf(format+"\n", args...)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	stats, err := extract.Run(ctx, pool, adapter, client, opts)
	if stats != nil {
		printExtractStats(stats)
	}
	return err
}

func printExtractStats(stats *extract.Stats) {
	mode := ""
	if stats.DryRun {
		mode = " (dry run)"
	}
	fmt.Printf("extract run%s: %d unit(s) total, %d done, %d skipped, %d failed, %d deferred\n",
		mode, stats.UnitsTotal, stats.UnitsDone, stats.UnitsSkipped, stats.UnitsFailed, stats.UnitsDeferred)
	fmt.Printf("records: %d kept (%s), %d dropped\n",
		stats.KeptRecords, formatCounts(stats.Records), stats.DroppedRecords)
	if len(stats.DropLog) > 0 {
		fmt.Printf("drop log:\n")
		for _, reason := range sortedKeys(stats.DropLog) {
			fmt.Printf("  %-12d %s\n", stats.DropLog[reason], reason)
		}
	}
	fmt.Printf("tokens: %d in, %d out", stats.InputTokens, stats.OutputTokens)
	if stats.CostUSD != nil {
		fmt.Printf("; spend: $%.4f", *stats.CostUSD)
	}
	fmt.Printf("\n")
}

func formatCounts(counts map[string]int) string {
	parts := []string{}
	for _, name := range []string{"claims", "events", "entities", "relationships", "contradictions"} {
		if counts[name] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[name], name))
		}
	}
	return strings.Join(parts, ", ")
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func extractStatusCmd() error {
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

	rows, err := pool.Query(ctx, `
		SELECT unit_key, prompt_version, status, attempts, completed_at,
		       COALESCE(last_error, '')
		FROM extraction_units
		ORDER BY prompt_version, unit_key`)
	if err != nil {
		return err
	}
	defer rows.Close()

	fmt.Printf("%-40s %-14s %-8s %8s  %s\n", "UNIT", "PROMPT", "STATUS", "ATTEMPTS", "OUTCOME")
	hasRows := false
	for rows.Next() {
		hasRows = true
		var unitKey, promptVersion, status, lastError string
		var attempts int
		var completedAt *string
		if err := rows.Scan(&unitKey, &promptVersion, &status, &attempts, &completedAt, &lastError); err != nil {
			return err
		}
		outcome := ""
		switch {
		case lastError != "":
			outcome = lastError
		case completedAt != nil:
			outcome = *completedAt
		}
		fmt.Printf("%-40s %-14s %-8s %8d  %s\n", unitKey, promptVersion, status, attempts, outcome)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !hasRows {
		fmt.Println("extraction ledger is empty — no units recorded yet")
	}
	return nil
}

// validateCmd drives the validation stage: `validate run` executes a run,
// `validate status` summarizes the verdict ledger and open flags.
func validateCmd(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "run":
		return validateRunCmd(args[1:])
	case "status":
		return validateStatusCmd()
	default:
		_, _ = fmt.Fprint(os.Stderr, `usage: fact-checker validate run    run the adversarial + deterministic validation stage
        fact-checker validate status summarize the verdict ledger and open flags
`)
		return errors.New("missing or unknown validate subcommand")
	}
}

func validateRunCmd(args []string) error {
	flags := flag.NewFlagSet("validate run", flag.ContinueOnError)
	flags.Usage = func() {
		_, _ = fmt.Fprint(flags.Output(), `usage: fact-checker validate run --adapter <manifest.json> [flags]

Runs the validation stage: the adversarial second model re-checks claims
against their cited evidence, then the deterministic consistency engine
checks the graph. Machine actions may only downgrade or flag — never
upgrade, edit, or delete. Exit code 1 on error-severity findings
(--strict extends that to warnings).
`)
		flags.PrintDefaults()
	}
	adapterPath := flags.String("adapter", "", "path to the adapter manifest (required)")
	offline := flags.Bool("offline", false, "deterministic engine only; no adversarial model calls")
	dryRun := flags.Bool("dry-run", false, "plan the adversarial pass and report engine findings; no model calls, no writes")
	limit := flags.Int("limit", 0, "adversarially check at most N claims this run (remainder deferred)")
	budget := flags.Float64("budget", 0, "soft USD spend cap (0 = config default; unpriced runs fall back to the claim cap)")
	maxClaims := flags.Int("max-claims", 0, "hard per-run adversarial claim cap (0 = config default)")
	batchSize := flags.Int("batch-size", 0, "claims per adversarial model call (0 = config default)")
	agreementThreshold := flags.Float64("agreement-threshold", 0, "score at or above which 'agree' stands (0 = config default)")
	maxAttempts := flags.Int("max-attempts", 0, "model calls per batch, including JSON repairs (0 = config default)")
	reportDir := flags.String("report-dir", "reports/validation", "directory for flag-rate, contradiction, and review-queue reports")
	noReport := flags.Bool("no-report", false, "do not write report files")
	strict := flags.Bool("strict", false, "exit 1 on warning-severity findings, not just errors")
	quiet := flags.Bool("quiet", false, "suppress progress lines")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *adapterPath == "" {
		flags.Usage()
		return errors.New("--adapter is required")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.ValidateDatabase(); err != nil {
		return err
	}

	loaded, err := adapters.LoadManifest(*adapterPath)
	if err != nil {
		return err
	}
	adapter, ok := loaded.(validate.Adapter)
	if !ok {
		return fmt.Errorf("adapter %s does not implement the validation contract", *adapterPath)
	}

	var client llm.Completer
	if !*offline && !*dryRun {
		if cfg.AIKey == "" || cfg.Models[config.StageValidate] == "" {
			return errors.New("validation needs FACTCHECK_AI_API_KEY and FACTCHECK_MODEL_VALIDATE (or --offline / --dry-run)")
		}
		interval := cfg.RequestMinInterval
		if override, ok := cfg.StageIntervals[config.StageValidate]; ok && override > 0 {
			interval = override
		}
		client, err = llm.NewClient(cfg.AIKey, cfg.Models[config.StageValidate], cfg.AIBaseURL, llm.ClientOptions{
			RequestInterval: interval,
			Timeout:         cfg.AITimeout,
		})
		if err != nil {
			return err
		}
	}

	opts := validate.DefaultOptions()
	if *budget > 0 {
		opts.BudgetUSD = *budget
	} else if cfg.BudgetPerRunUSD > 0 {
		opts.BudgetUSD = cfg.BudgetPerRunUSD
	}
	if *maxClaims > 0 {
		opts.MaxClaims = *maxClaims
	} else {
		opts.MaxClaims = cfg.ValidateMaxClaims
	}
	if *batchSize > 0 {
		opts.BatchSize = *batchSize
	} else {
		opts.BatchSize = cfg.ValidateBatchSize
	}
	if *agreementThreshold > 0 {
		opts.AgreementThreshold = *agreementThreshold
	} else {
		opts.AgreementThreshold = cfg.ValidateAgreementThreshold
	}
	if *maxAttempts > 0 {
		opts.MaxAttempts = *maxAttempts
	} else {
		opts.MaxAttempts = cfg.ValidateMaxAttempts
	}
	opts.Limit = *limit
	opts.Offline = *offline
	opts.DryRun = *dryRun
	opts.PriceInMTok = cfg.Prices[config.StageValidate].Input
	opts.PriceOutMTok = cfg.Prices[config.StageValidate].Output
	opts.ReportDir = ""
	if !*noReport {
		opts.ReportDir = *reportDir
	}
	if !*quiet {
		opts.Log = func(format string, args ...any) {
			fmt.Printf(format+"\n", args...)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	stats, err := validate.Run(ctx, pool, adapter, client, opts)
	if stats != nil {
		printValidateStats(stats)
	}
	if err != nil {
		return err
	}
	if stats.ErrorCount() > 0 || (*strict && stats.WarningCount() > 0) {
		return fmt.Errorf("%d error(s), %d warning(s) found; see report above",
			stats.ErrorCount(), stats.WarningCount())
	}
	return nil
}

func printValidateStats(stats *validate.Stats) {
	mode := ""
	switch {
	case stats.DryRun:
		mode = " (dry run)"
	case stats.Offline:
		mode = " (offline)"
	}
	model := stats.Model
	if model == "" {
		model = "(none)"
	}
	fmt.Printf("validate run%s: model %s | prompt %s\n", mode, model, stats.PromptVersion)
	fmt.Printf("claims: %d in graph, %d verdicted before, %d checked this run "+
		"(%d blocked, %d unverdicted, %d deferred)\n",
		stats.Claims.Total, stats.Claims.VerdictedPrior, stats.Claims.Checked,
		stats.Claims.Blocked, stats.Claims.Unverdicted, stats.Claims.Deferred)
	if stats.DryRun {
		fmt.Printf("dry run: no model calls, nothing written (engine findings: %d)\n", stats.Flags.Total)
		printFlagLog(stats)
		return
	}
	fmt.Printf("verdicts: %d agree, %d downgrade, %d flag-review "+
		"(%d batch(es), %d call(s), %d repair(s))\n",
		stats.Adversarial.Verdicts["agree"], stats.Adversarial.Verdicts["downgrade"],
		stats.Adversarial.Verdicts["flag_review"],
		stats.Adversarial.Batches, stats.Adversarial.Attempts, stats.Adversarial.Repairs)
	fmt.Printf("machine actions: %d downgrade(s) applied (%s), %d claim(s) contested, "+
		"%d contradiction(s) registered (%d register entries reused)\n",
		sumCounts(stats.Machine.DowngradesApplied), formatDowngrades(stats.Machine.DowngradesApplied),
		stats.Machine.ClaimsContested, stats.Machine.ContradictionsCreated, stats.Machine.ContradictionsReused)
	fmt.Printf("flags: %d (%d error, %d warning, %d review; %d cleared)\n",
		stats.Flags.Total, stats.Flags.BySeverity[validate.SeverityError],
		stats.Flags.BySeverity[validate.SeverityWarning],
		stats.Flags.BySeverity[validate.SeverityReview], stats.Flags.Cleared)
	printFlagLog(stats)
	cost := "unpriced (set FACTCHECK_PRICE_MTOK_VALIDATE_INPUT/OUTPUT for USD estimates)"
	if stats.Usage.CostUSD != nil {
		cost = fmt.Sprintf("$%.4f", *stats.Usage.CostUSD)
	}
	fmt.Printf("usage: %d in / %d out tokens, est. cost %s, %.0fs total\n",
		stats.Usage.InputTokens, stats.Usage.OutputTokens, cost, stats.ElapsedSecs)
	if stats.ReportDir != "" {
		fmt.Printf("reports: %s\n", stats.ReportDir)
	}
}

func printFlagLog(stats *validate.Stats) {
	if len(stats.Flags.ByCheckCode) == 0 {
		return
	}
	fmt.Println("by check:")
	codes := sortedKeys(stats.Flags.ByCheckCode)
	sort.SliceStable(codes, func(a, b int) bool {
		countA := stats.Flags.ByCheckCode[codes[a]]
		countB := stats.Flags.ByCheckCode[codes[b]]
		if countA != countB {
			return countA > countB
		}
		return codes[a] < codes[b]
	})
	for _, code := range codes {
		fmt.Printf("  %-4d  %s\n", stats.Flags.ByCheckCode[code], code)
	}
}

func sumCounts(counts map[string]int) int {
	total := 0
	for _, count := range counts {
		total += count
	}
	return total
}

func formatDowngrades(counts map[string]int) string {
	if len(counts) == 0 {
		return "none"
	}
	parts := []string{}
	for _, name := range sortedKeys(counts) {
		parts = append(parts, fmt.Sprintf("%d %s", counts[name], name))
	}
	return strings.Join(parts, ", ")
}

func validateStatusCmd() error {
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

	ledger, err := validate.VerdictLedger(ctx, pool, validate.PromptVersion)
	if err != nil {
		return err
	}
	open, err := validate.OpenFlagCounts(ctx, pool)
	if err != nil {
		return err
	}

	fmt.Printf("verdict ledger (prompt %s):\n", validate.PromptVersion)
	total := 0
	for _, verdict := range []string{"agree", "downgrade", "flag_review"} {
		count := ledger[verdict]
		total += count
		fmt.Printf("  %-12d %s\n", count, verdict)
	}
	fmt.Printf("  %-12d total\n", total)

	fmt.Printf("open flags: %d error, %d warning, %d review\n",
		open[validate.SeverityError], open[validate.SeverityWarning], open[validate.SeverityReview])
	return nil
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
