// Extraction orchestration: units -> prompts -> model -> validation -> DB.
//
// Every run is recorded in pipeline_runs (kind 'extract'). Runs are
// idempotent and resumable: a unit already done in the extraction ledger
// under the same prompt version and unchanged input hash is skipped, each
// unit commits in its own transaction, and re-running after an
// interruption (or a budget stop) continues where the previous run
// stopped. DryRun plans the whole run and reports estimates without
// calling the model or writing anything beyond the pipeline_runs row.
//
// Model calls run on a worker pool; everything that touches the database
// — ledger rows, model outputs, validation against the current graph,
// record loading — stays on the main goroutine, so per-unit transactions
// keep their resume semantics (the reference implementation's ADR-0023
// pattern). The client's minimum request interval is one shared gate, so
// a concurrent run still honours a single global rate.

package extract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/madeofpendletonwool/fact-checker/internal/llm"
)

// Adapter is the stage-2 slice of the adapter contract: the prompt framing
// for the domain plus the ingester that turns documents into work units.
// The full SDK (stage 4) extends this with source registries, ontologies,
// check sets, and hooks.
type Adapter interface {
	// Brief supplies the domain framing rendered into every prompt.
	Brief() DomainBrief
	// BuildUnits turns the ingested documents into extraction units, each
	// with its citation candidate set.
	BuildUnits(ctx context.Context, s Store) ([]Unit, error)
}

// Options bound one extraction run.
type Options struct {
	// DryRun plans the run without model calls or record writes.
	DryRun bool
	// Limit bounds the run to N pending units; the rest stay deferred.
	Limit int
	// BudgetUSD is the soft spend cap; with no prices configured the
	// budget falls back to the unit cap and cost is tracked in tokens.
	BudgetUSD float64
	// MaxUnits is the hard per-run unit cap.
	MaxUnits int
	// MaxAttempts bounds model calls per unit, including JSON repairs.
	MaxAttempts int
	// Concurrency sets how many model calls are in flight at once.
	Concurrency int
	// PriceIn/PriceOutMTok configure the cost tracker.
	PriceInMTok  float64
	PriceOutMTok float64
	// Log, when set, receives progress lines.
	Log func(format string, args ...any)
}

// DefaultOptions mirrors the reference implementation's defaults.
func DefaultOptions() Options {
	return Options{
		BudgetUSD:   25.0,
		MaxUnits:    500,
		MaxAttempts: 3,
		Concurrency: 1,
	}
}

// Stats is the run summary persisted into pipeline_runs.stats.
type Stats struct {
	DryRun         bool           `json:"dry_run"`
	PromptVersion  string         `json:"prompt_version"`
	Model          string         `json:"model"`
	UnitsTotal     int            `json:"units_total"`
	UnitsDone      int            `json:"units_done"`
	UnitsSkipped   int            `json:"units_skipped"`
	UnitsFailed    int            `json:"units_failed"`
	UnitsDeferred  int            `json:"units_deferred"`
	UnitsTruncated int            `json:"units_truncated"`
	Attempts       int            `json:"attempts"`
	Repairs        int            `json:"repairs"`
	Records        map[string]int `json:"records"`
	KeptRecords    int            `json:"kept_records"`
	DroppedRecords int            `json:"dropped_records"`
	DropLog        map[string]int `json:"drop_log"`
	InputTokens    int            `json:"input_tokens"`
	OutputTokens   int            `json:"output_tokens"`
	CostUSD        *float64       `json:"cost_usd"`
	AvgLatency     float64        `json:"avg_latency_seconds"`
	MaxLatency     float64        `json:"max_latency_seconds"`
	ElapsedSeconds float64        `json:"elapsed_seconds"`
}

func newStats(dryRun bool, model string) *Stats {
	return &Stats{
		DryRun:        dryRun,
		PromptVersion: PromptVersion,
		Model:         model,
		Records:       map[string]int{},
		DropLog:       map[string]int{},
	}
}

func (s *Stats) bump(key string) { s.DropLog[key]++ }

func (s *Stats) recordDrops(drops []Drop) {
	for _, drop := range drops {
		switch drop.Kind {
		case "claim", "event", "relationship":
			s.DroppedRecords++
		}
		s.bump(drop.Kind + ":" + drop.Reason)
	}
}

// unitJob is one submitted model call.
type unitJob struct {
	unit   *Unit
	system string
	user   string
}

// unitOutcome is what one worker produced for one unit. Workers touch no
// database and no shared state.
type unitOutcome struct {
	job       *unitJob
	responses []llm.Response
	wire      *WireExtraction
	problems  []string
	err       string
	attempts  int
	repairs   int
}

// Aborted marks a run that tripped an operational guardrail (ledger or
// persistence failure rather than per-unit model trouble).
type Aborted struct{ msg string }

func (a *Aborted) Error() string { return a.msg }

// Run executes one extraction run and returns its stats. A returned error
// means the run itself failed (recorded as such in pipeline_runs);
// per-unit failures land in the ledger and stats instead.
func Run(ctx context.Context, pool *pgxpool.Pool, adapter Adapter, client llm.Completer, opts Options) (*Stats, error) {
	if !opts.DryRun && client == nil {
		return nil, errors.New("extract: client is required unless dry-run")
	}
	model := ""
	if client != nil {
		model = client.Model()
	}
	if opts.Concurrency < 1 {
		opts.Concurrency = 1
	}
	if opts.MaxAttempts < 1 {
		opts.MaxAttempts = 1
	}
	stats := newStats(opts.DryRun, model)
	started := time.Now()

	parameters, _ := json.Marshal(map[string]any{
		"prompt_version": PromptVersion,
		"model":          model,
		"dry_run":        opts.DryRun,
		"limit":          opts.Limit,
		"budget_usd":     opts.BudgetUSD,
		"max_units":      opts.MaxUnits,
		"max_attempts":   opts.MaxAttempts,
		"concurrency":    opts.Concurrency,
		"price_in_mtok":  opts.PriceInMTok,
		"price_out_mtok": opts.PriceOutMTok,
	})

	runID, err := beginRun(ctx, pool, parameters)
	if err != nil {
		return nil, err
	}

	stats, runErr := execute(ctx, pool, adapter, client, opts, stats, runID)
	stats.ElapsedSeconds = time.Since(started).Seconds()
	if err := finishRun(ctx, pool, runID, stats, runErr); err != nil {
		return stats, fmt.Errorf("extract: record run outcome: %w", err)
	}
	return stats, runErr
}

func beginRun(ctx context.Context, pool *pgxpool.Pool, parameters []byte) (int64, error) {
	var runID int64
	err := pool.QueryRow(ctx, `
		INSERT INTO pipeline_runs (run_kind, status, parameters)
		VALUES ('extract', 'running', $1::jsonb)
		RETURNING id`, string(parameters)).Scan(&runID)
	if err != nil {
		return 0, fmt.Errorf("extract: insert pipeline run: %w", err)
	}
	return runID, nil
}

func finishRun(ctx context.Context, pool *pgxpool.Pool, runID int64, stats *Stats, runErr error) error {
	status := "ok"
	var notes *string
	if runErr != nil {
		status = "failed"
		message := runErr.Error()
		if len(message) > 2000 {
			message = message[:2000]
		}
		notes = &message
	}
	payload, err := json.Marshal(stats)
	if err != nil {
		return fmt.Errorf("marshal stats: %w", err)
	}
	_, err = pool.Exec(ctx, `
		UPDATE pipeline_runs SET status = $1, finished_at = now(),
		       stats = $2::jsonb, notes = $3 WHERE id = $4`,
		status, string(payload), notes, runID)
	return err
}

func execute(ctx context.Context, pool *pgxpool.Pool, adapter Adapter, client llm.Completer, opts Options, stats *Stats, runID int64) (*Stats, error) {
	units, err := adapter.BuildUnits(ctx, NewStore(pool))
	if err != nil {
		return stats, fmt.Errorf("extract: build units: %w", err)
	}
	ledger, err := LedgerState(ctx, pool)
	if err != nil {
		return stats, err
	}
	stats.UnitsTotal = len(units)

	pending := []*Unit{}
	for i := range units {
		unit := &units[i]
		if row, exists := ledger[unit.UnitKey]; exists && row.Status == "done" && row.InputHash == unit.InputHash() {
			stats.UnitsSkipped++
			continue
		}
		pending = append(pending, unit)
	}

	if opts.Limit > 0 && len(pending) > opts.Limit {
		stats.UnitsDeferred += len(pending) - opts.Limit
		pending = pending[:opts.Limit]
	}
	if len(pending) > opts.MaxUnits {
		stats.UnitsDeferred += len(pending) - opts.MaxUnits
		pending = pending[:opts.MaxUnits]
	}
	for _, unit := range pending {
		if unit.Truncated {
			stats.UnitsTruncated++
		}
	}

	if opts.DryRun || client == nil {
		return stats, nil
	}

	tracker := &llm.CostTracker{PriceInMTok: opts.PriceInMTok, PriceOutMTok: opts.PriceOutMTok}
	entities, events, claims, err := IDSnapshots(ctx, pool)
	if err != nil {
		return stats, err
	}

	logf := opts.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	logf("extracting %d unit(s) with %d worker(s) (%d already done, %d deferred)",
		len(pending), opts.Concurrency, stats.UnitsSkipped, stats.UnitsDeferred)

	brief := adapter.Brief()
	// Both channels buffer a full window so workers never block on a send:
	// main owns the only receive points and therefore every DB write.
	jobs := make(chan *unitJob, opts.Concurrency)
	outcomes := make(chan unitOutcome, opts.Concurrency)

	var workers sync.WaitGroup
	for i := 0; i < opts.Concurrency; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobs {
				outcomes <- callUnit(ctx, client, job, opts.MaxAttempts)
			}
		}()
	}

	next := 0
	inflight := 0
	submitted := 0
	stopSubmitting := false
	var persistErr error
runLoop:
	for {
		// Fill the in-flight window: ledger-pend each unit on the main
		// goroutine, then hand only the model call to a worker.
		for !stopSubmitting && inflight < opts.Concurrency && next < len(pending) {
			if ctx.Err() != nil {
				stopSubmitting = true
				break
			}
			unit := pending[next]
			system := BuildSystemPrompt(brief, unit.Policy, unit.Reference)
			user := BuildUserPrompt(brief, unit)
			if !budgetAllows(tracker, opts.BudgetUSD, estimateCost(system+user, tracker)) {
				stopSubmitting = true
				break
			}
			if err := ledgerPend(ctx, pool, unit, client.Model(), runID); err != nil {
				persistErr = fmt.Errorf("extract: ledger unit %s: %w", unit.UnitKey, err)
				break
			}
			jobs <- &unitJob{unit: unit, system: system, user: user}
			next++
			inflight++
			submitted++
		}
		if persistErr != nil {
			break runLoop
		}
		if inflight == 0 {
			break runLoop
		}

		select {
		case <-ctx.Done():
			stopSubmitting = true
		case outcome := <-outcomes:
			inflight--
			if err := persistOutcome(ctx, pool, stats, tracker, entities, events, claims, &outcome, runID, logf); err != nil {
				persistErr = err
				break runLoop
			}
		}
	}
	close(jobs)
	workers.Wait()
	// Outcomes that completed during shutdown stay pending in the ledger
	// (counted deferred) so a later run resumes them.
drain:
	for {
		select {
		case <-outcomes:
			stats.UnitsDeferred++
		default:
			break drain
		}
	}

	if persistErr != nil {
		return stats, persistErr
	}
	// Units never submitted (budget stop or cancellation) are deferred,
	// not failed — a later run resumes them.
	stats.UnitsDeferred += len(pending) - submitted
	if tracker.Priced() {
		cost := tracker.CostUSD()
		stats.CostUSD = &cost
	}
	stats.InputTokens = tracker.InputTokens
	stats.OutputTokens = tracker.OutputTokens
	stats.AvgLatency = tracker.AvgLatency()
	stats.MaxLatency = tracker.MaxLatency()
	return stats, nil
}

// callUnit performs the model calls and wire parsing for one unit on a
// worker goroutine. It touches no database; everything else in the run
// stays on the main goroutine.
func callUnit(ctx context.Context, client llm.Completer, job *unitJob, maxAttempts int) unitOutcome {
	outcome := unitOutcome{job: job}
	user := job.user
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		outcome.attempts++
		response, err := client.Complete(ctx, job.system, user)
		if err != nil {
			var overload *llm.OverloadError
			if errors.As(err, &overload) {
				outcome.err = "input too large: " + err.Error()
				break
			}
			outcome.err = err.Error()
			continue
		}
		outcome.responses = append(outcome.responses, response)
		wire, problems, parseErr := ParseExtraction(response.Text)
		if parseErr != nil {
			outcome.err = "unparseable response: " + parseErr.Error()
			outcome.wire = nil
			if attempt < maxAttempts {
				outcome.repairs++
				user = user + "\n\nYour previous reply was not valid JSON (" +
					parseErr.Error() + "). Return ONLY the JSON object."
			}
			continue
		}
		outcome.wire = wire
		outcome.problems = problems
		outcome.err = ""
		break
	}
	return outcome
}

// persistOutcome validates and writes one unit's outcome. Main goroutine
// only; one transaction per unit.
func persistOutcome(
	ctx context.Context,
	pool *pgxpool.Pool,
	stats *Stats,
	tracker *llm.CostTracker,
	entities, events, claims map[string]bool,
	outcome *unitOutcome,
	runID int64,
	logf func(string, ...any),
) error {
	unit := outcome.job.unit
	stats.Attempts += outcome.attempts
	stats.Repairs += outcome.repairs

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin unit tx %s: %w", unit.UnitKey, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, response := range outcome.responses {
		tracker.Add(response)
		if err := WriteModelOutput(ctx, tx, runID, unit, response.Model, response.Text, response.InputTokens, response.OutputTokens); err != nil {
			return err
		}
	}

	if outcome.wire == nil {
		if err := FinishLedgerRow(ctx, tx, unit, "failed", map[string]int{}, nil, outcome.err); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit failed unit %s: %w", unit.UnitKey, err)
		}
		stats.UnitsFailed++
		return nil
	}

	for _, problem := range outcome.problems {
		stats.bump("parse:" + problem)
	}

	validated := Validate(outcome.wire, &ValidationContext{
		Unit:        unit,
		DBEntityIDs: entities,
		DBEventIDs:  events,
		DBClaimIDs:  claims,
	})

	if err := LoadRecords(ctx, tx, validated); err != nil {
		return err
	}
	dropped := append([]Drop(nil), validated.Drops...)
	if err := FinishLedgerRow(ctx, tx, unit, "done", validated.RecordCounts(), dropped, ""); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit unit %s: %w", unit.UnitKey, err)
	}

	stats.UnitsDone++
	stats.Records["entities"] += len(validated.Entities)
	stats.Records["events"] += len(validated.Events)
	stats.Records["claims"] += len(validated.Claims)
	stats.Records["relationships"] += len(validated.Relationships)
	stats.Records["contradictions"] += len(validated.Contradictions)
	stats.KeptRecords = stats.Records["entities"] + stats.Records["events"] +
		stats.Records["claims"] + stats.Records["relationships"]
	stats.recordDrops(validated.Drops)
	for _, entity := range validated.Entities {
		entities[entity.ID] = true
	}
	for _, event := range validated.Events {
		events[event.ID] = true
	}
	for _, claim := range validated.Claims {
		claims[claim.ID] = true
	}

	logf("[%s] %s: %dc %de %den %dr (%d call(s), %d in-tokens)",
		unit.UnitKey, unit.Title,
		len(validated.Claims), len(validated.Events),
		len(validated.Entities), len(validated.Relationships),
		tracker.Calls, tracker.InputTokens)
	return nil
}

func ledgerPend(ctx context.Context, pool *pgxpool.Pool, unit *Unit, model string, runID int64) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin ledger tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := UpsertLedgerRow(ctx, tx, unit, model, unit.InputHash(), runID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit ledger tx: %w", err)
	}
	return nil
}

// estimateTokens is the pre-call cost heuristic (chars/4) the budget gate
// uses; actual usage comes back with the response.
func estimateTokens(text string) int {
	tokens := len(text) / 4
	if tokens < 1 {
		return 1
	}
	return tokens
}

func estimateCost(prompt string, tracker *llm.CostTracker) float64 {
	return float64(estimateTokens(prompt))/1e6*tracker.PriceInMTok +
		4000.0/1e6*tracker.PriceOutMTok
}

func budgetAllows(tracker *llm.CostTracker, budget float64, nextEstimate float64) bool {
	if !tracker.Priced() {
		return true
	}
	return tracker.CostUSD()+nextEstimate <= budget
}
