// Validation orchestration: adversarial model + deterministic engine.
//
// Every run is recorded in pipeline_runs (kind 'validate'). The
// adversarial pass is idempotent and resumable via the verdict ledger
// (claims already verdicted under the same prompt version are skipped);
// the deterministic engine always runs over the full graph snapshot.
// Machine actions are limited to confidence downgrades and contradiction
// registration — flags and review items carry the rest to a human.

package validate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/madeofpendletonwool/fact-checker/internal/llm"
)

// Adapter is the validation slice of the adapter contract: the skeptical
// framing, the reference-passage source, and the domain's extra
// deterministic checks. The full SDK (stage 4) folds this into the
// one-adapter contract.
type Adapter interface {
	// ValidationBrief supplies the domain framing rendered into the
	// adversarial prompt.
	ValidationBrief() Brief
	// ReferenceSource names the source whose url-locator citations point
	// at the passage a claim was extracted from.
	ReferenceSource() string
	// Checks returns the adapter's domain invariants, registered to run
	// alongside the core set (nil when the domain adds none).
	Checks() []Check
}

// Options bound one validation run.
type Options struct {
	// Offline runs the deterministic engine only; no model calls.
	Offline bool
	// DryRun plans the adversarial pass and reports engine findings
	// without model calls or any writes beyond the pipeline_runs row.
	DryRun bool
	// Limit bounds the run to N pending claims; the rest stay deferred.
	Limit int
	// BudgetUSD is the soft spend cap; with no prices configured the
	// budget falls back to the claim cap and cost is tracked in tokens.
	BudgetUSD float64
	// MaxClaims is the hard per-run adversarial claim cap.
	MaxClaims int
	// BatchSize is claims per model call.
	BatchSize int
	// AgreementThreshold is the score at or above which 'agree' stands.
	AgreementThreshold float64
	// MaxAttempts bounds model calls per batch, including JSON repairs.
	MaxAttempts int
	// MaxReferenceChars caps the reference passage sent per claim.
	MaxReferenceChars int
	// PriceIn/PriceOutMTok configure the cost tracker.
	PriceInMTok  float64
	PriceOutMTok float64
	// ReportDir, when set, publishes the report files there.
	ReportDir string
	// Log, when set, receives progress lines.
	Log func(format string, args ...any)
}

// DefaultOptions mirrors the reference implementation's defaults.
func DefaultOptions() Options {
	return Options{
		BudgetUSD:          10.0,
		MaxClaims:          2000,
		BatchSize:          10,
		AgreementThreshold: 0.8,
		MaxAttempts:        3,
		MaxReferenceChars:  DefaultReferenceChars,
	}
}

// ClaimsStats is the claim accounting sub-report.
type ClaimsStats struct {
	Total          int `json:"total"`
	VerdictedPrior int `json:"verdicted_before"`
	Planned        int `json:"planned"`
	Checked        int `json:"checked"`
	Blocked        int `json:"blocked"`
	Unverdicted    int `json:"unverdicted"`
	Deferred       int `json:"deferred"`
}

// AdversarialStats is the second-model accounting sub-report.
type AdversarialStats struct {
	Batches  int            `json:"batches"`
	Attempts int            `json:"attempts"`
	Repairs  int            `json:"repairs"`
	Verdicts map[string]int `json:"verdicts"`
}

// MachineActionsStats is the machine-action accounting sub-report:
// everything a machine pass is allowed to do.
type MachineActionsStats struct {
	DowngradesApplied     map[string]int `json:"downgrades_applied"`
	ClaimsContested       int            `json:"claims_contested"`
	ContradictionsCreated int            `json:"contradictions_registered"`
	ContradictionsReused  int            `json:"contradictions_reused"`
}

// FlagsStats is the findings accounting sub-report.
type FlagsStats struct {
	Total       int            `json:"total"`
	BySeverity  map[string]int `json:"by_severity"`
	ByCheckCode map[string]int `json:"by_check_code"`
	Cleared     int            `json:"cleared"`
}

// UsageStats is the model-spend accounting sub-report.
type UsageStats struct {
	InputTokens    int      `json:"input_tokens"`
	OutputTokens   int      `json:"output_tokens"`
	CostUSD        *float64 `json:"cost_usd"`
	AvgLatencySecs float64  `json:"avg_latency_seconds"`
	MaxLatencySecs float64  `json:"max_latency_seconds"`
}

// Stats is the run summary persisted into pipeline_runs.stats.
type Stats struct {
	DryRun        bool                `json:"dry_run"`
	Offline       bool                `json:"offline"`
	PromptVersion string              `json:"prompt_version"`
	Model         string              `json:"model"`
	Claims        ClaimsStats         `json:"claims"`
	Adversarial   AdversarialStats    `json:"adversarial"`
	Machine       MachineActionsStats `json:"machine_actions"`
	Flags         FlagsStats          `json:"flags"`
	Usage         UsageStats          `json:"usage"`
	ElapsedSecs   float64             `json:"elapsed_seconds"`
	ReportDir     string              `json:"report_dir,omitempty"`
}

func newStats(offline, dryRun bool, model string) *Stats {
	return &Stats{
		DryRun:        dryRun,
		Offline:       offline,
		PromptVersion: PromptVersion,
		Model:         model,
		Adversarial:   AdversarialStats{Verdicts: map[string]int{}},
		Machine:       MachineActionsStats{DowngradesApplied: map[string]int{}},
		Flags: FlagsStats{
			BySeverity:  map[string]int{SeverityError: 0, SeverityWarning: 0, SeverityReview: 0},
			ByCheckCode: map[string]int{},
		},
	}
}

// ErrorCount is the error-severity finding count (the CI gate).
func (s *Stats) ErrorCount() int { return s.Flags.BySeverity[SeverityError] }

// WarningCount is the warning-severity finding count (the --strict gate).
func (s *Stats) WarningCount() int { return s.Flags.BySeverity[SeverityWarning] }

// Aborted marks a run that tripped an operational guardrail (persistence
// failure rather than per-batch model trouble).
type Aborted struct{ msg string }

func (a *Aborted) Error() string { return a.msg }

// Run executes one validation run and returns its stats. A returned
// error means the run itself failed (recorded as such in pipeline_runs);
// per-batch model failures land in stats instead.
func Run(ctx context.Context, pool *pgxpool.Pool, adapter Adapter, client llm.Completer, opts Options) (*Stats, error) {
	if !opts.Offline && !opts.DryRun && client == nil {
		return nil, errors.New("validate: client is required unless offline or dry-run")
	}
	model := ""
	if client != nil {
		model = client.Model()
	}
	if opts.BatchSize < 1 {
		opts.BatchSize = 1
	}
	if opts.MaxAttempts < 1 {
		opts.MaxAttempts = 1
	}
	if opts.MaxClaims < 1 {
		opts.MaxClaims = 1
	}
	stats := newStats(opts.Offline, opts.DryRun, model)
	started := time.Now()

	parameters, _ := json.Marshal(map[string]any{
		"prompt_version":      PromptVersion,
		"model":               model,
		"offline":             opts.Offline,
		"dry_run":             opts.DryRun,
		"limit":               opts.Limit,
		"budget_usd":          opts.BudgetUSD,
		"max_claims":          opts.MaxClaims,
		"batch_size":          opts.BatchSize,
		"agreement_threshold": opts.AgreementThreshold,
		"max_attempts":        opts.MaxAttempts,
		"price_in_mtok":       opts.PriceInMTok,
		"price_out_mtok":      opts.PriceOutMTok,
	})

	runID, err := beginRun(ctx, pool, parameters)
	if err != nil {
		return nil, err
	}

	stats, runErr := execute(ctx, pool, adapter, client, opts, stats, runID)
	stats.ElapsedSecs = time.Since(started).Seconds()
	if err := finishRun(ctx, pool, runID, stats, runErr); err != nil {
		return stats, fmt.Errorf("validate: record run outcome: %w", err)
	}
	return stats, runErr
}

func beginRun(ctx context.Context, pool *pgxpool.Pool, parameters []byte) (int64, error) {
	var runID int64
	err := pool.QueryRow(ctx, `
		INSERT INTO pipeline_runs (run_kind, status, parameters)
		VALUES ('validate', 'running', $1::jsonb)
		RETURNING id`, string(parameters)).Scan(&runID)
	if err != nil {
		return 0, fmt.Errorf("validate: insert pipeline run: %w", err)
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
	logf := opts.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}

	state, err := LoadGraphState(ctx, pool)
	if err != nil {
		return stats, err
	}
	stats.Claims.Total = len(state.Claims)

	adversarialFlags := []Finding{}
	if !opts.Offline {
		verdicted, err := VerdictedClaimIDs(ctx, pool, PromptVersion)
		if err != nil {
			return stats, err
		}
		pending := make([]*GraphClaim, 0, len(state.Claims))
		for i := range state.Claims {
			if !verdicted[state.Claims[i].ID] {
				pending = append(pending, &state.Claims[i])
			} else {
				stats.Claims.VerdictedPrior++
			}
		}
		if opts.Limit > 0 && len(pending) > opts.Limit {
			stats.Claims.Deferred += len(pending) - opts.Limit
			pending = pending[:opts.Limit]
		}
		if len(pending) > opts.MaxClaims {
			stats.Claims.Deferred += len(pending) - opts.MaxClaims
			pending = pending[:opts.MaxClaims]
		}

		claimValues := make([]GraphClaim, 0, len(pending))
		for _, claim := range pending {
			claimValues = append(claimValues, *claim)
		}
		plan, err := BuildReviewPlan(ctx, pool, claimValues, adapter.ReferenceSource(), opts.MaxReferenceChars)
		if err != nil {
			return stats, err
		}
		stats.Claims.Planned = len(plan.Reviews)
		stats.Claims.Blocked = len(plan.Blocked)
		adversarialFlags = append(adversarialFlags, plan.Blocked...)

		if opts.DryRun || client == nil {
			logf("dry run: would adversarially check %d claim(s) (%d already verdicted, %d blocked, %d deferred)",
				len(plan.Reviews), stats.Claims.VerdictedPrior, len(plan.Blocked), stats.Claims.Deferred)
		} else {
			logf("adversarial pass: %d claim(s) to check (%d already verdicted, %d blocked, %d deferred)",
				len(plan.Reviews), stats.Claims.VerdictedPrior, len(plan.Blocked), stats.Claims.Deferred)
			reviewFlags, err := runAdversarial(ctx, pool, adapter.ValidationBrief(), state, plan, client, opts, stats, runID, logf)
			if err != nil {
				return stats, err
			}
			adversarialFlags = append(adversarialFlags, reviewFlags...)
		}
	}

	if !opts.DryRun {
		logf("deterministic engine: checking graph consistency")
	}
	engine := RunEngine(state, adapter.Checks()...)

	if !opts.DryRun {
		if err := applyEngineActions(ctx, pool, engine, stats); err != nil {
			return stats, err
		}
	}

	allFlags := append(adversarialFlags, engine.Flags...)
	stats.Flags.Total = len(allFlags)
	for _, flag := range allFlags {
		stats.Flags.ByCheckCode[flag.CheckCode]++
		stats.Flags.BySeverity[flag.Severity]++
	}
	if opts.DryRun {
		return stats, nil
	}

	if err := writeRunFlags(ctx, pool, runID, allFlags, stats); err != nil {
		return stats, err
	}

	if opts.ReportDir != "" {
		written, err := WriteReports(ctx, pool, runID, opts.ReportDir, stats)
		if err != nil {
			return stats, err
		}
		stats.ReportDir = opts.ReportDir
		logf("reports: %s (%s)", opts.ReportDir, joinNames(written))
	}
	return stats, nil
}

// runAdversarial drives the batched second-model pass and returns the
// review flags it produced. Verdict rows are first-write-wins; downgrades
// apply only when still strictly downward versus the live row, so the
// adversarial and deterministic passes compose safely.
func runAdversarial(ctx context.Context, pool *pgxpool.Pool, brief Brief, state *GraphState, plan *ReviewPlan, client llm.Completer, opts Options, stats *Stats, runID int64, logf func(string, ...any)) ([]Finding, error) {
	tracker := &llm.CostTracker{PriceInMTok: opts.PriceInMTok, PriceOutMTok: opts.PriceOutMTok}
	claimsByID := state.ClaimByID()
	reviewFlags := []Finding{}

	batches := batchReviews(plan.Reviews, opts.BatchSize)
	for batchIndex, batch := range batches {
		system := BuildSystemPrompt(brief)
		user := BuildUserPrompt(batch)
		estimate := estimateCost(system+user, tracker)
		if !budgetAllows(tracker, opts.BudgetUSD, estimate) {
			for _, remaining := range batches[batchIndex:] {
				stats.Claims.Deferred += len(remaining)
			}
			logf("budget stop: %d claim(s) deferred", stats.Claims.Deferred)
			break
		}
		stats.Adversarial.Batches++

		verdicts, responses, attempts, repairs, batchErr := callBatch(ctx, client, system, user, opts.MaxAttempts)
		stats.Adversarial.Attempts += attempts
		stats.Adversarial.Repairs += repairs
		for _, response := range responses {
			tracker.Add(response)
			inputRef := "claims:" + joinClaimIDs(batch, 3)
			if err := WriteModelOutput(ctx, pool, runID, response.Model, inputRef,
				response.Text, response.InputTokens, response.OutputTokens); err != nil {
				return nil, err
			}
		}
		if batchErr != "" || len(verdicts) == 0 {
			stats.Claims.Unverdicted += len(batch)
			continue
		}

		validIDs := map[string]bool{}
		for _, review := range batch {
			validIDs[review.ClaimID] = true
		}
		byClaim := map[string]WireVerdict{}
		for _, verdict := range verdicts {
			if validIDs[verdict.ClaimID] && byClaim[verdict.ClaimID].ClaimID == "" {
				byClaim[verdict.ClaimID] = verdict
			}
		}
		for _, review := range batch {
			if byClaim[review.ClaimID].ClaimID == "" {
				stats.Claims.Unverdicted++
			}
		}

		rows := []VerdictRow{}
		batchFlags := map[string]Finding{}
		batchDowngrades := map[string]Downgrade{}
		for _, review := range batch {
			verdict, ok := byClaim[review.ClaimID]
			if !ok {
				continue
			}
			claim := claimsByID[review.ClaimID]
			if claim == nil {
				continue
			}
			rows = append(rows, VerdictRow{
				ClaimID:          review.ClaimID,
				Verdict:          verdict.Verdict,
				Agreement:        verdict.EffectiveAgreement(),
				TargetConfidence: verdict.TargetConfidence,
				Rationale:        verdict.Rationale,
			})
			stats.Adversarial.Verdicts[verdict.Verdict]++
			downgrade, flag := verdictOutcome(claim, verdict, opts.AgreementThreshold)
			if downgrade.ClaimID != "" {
				batchDowngrades[review.ClaimID] = downgrade
			}
			if flag != nil {
				batchFlags[review.ClaimID] = *flag
			}
		}
		if len(rows) == 0 {
			continue
		}

		applied := map[string]string{}
		err := func() error {
			tx, err := pool.Begin(ctx)
			if err != nil {
				return fmt.Errorf("begin verdict tx: %w", err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if err := WriteVerdicts(ctx, tx, runID, PromptVersion, client.Model(), rows); err != nil {
				return err
			}
			for claimID, downgrade := range batchDowngrades {
				ok, err := ApplyDowngrade(ctx, tx, downgrade)
				if err != nil {
					return err
				}
				if ok {
					stats.Machine.DowngradesApplied[downgrade.CheckCode]++
					if err := MarkVerdictApplied(ctx, tx, claimID, PromptVersion); err != nil {
						return err
					}
					applied[claimID] = downgrade.Target
				}
			}
			if err := tx.Commit(ctx); err != nil {
				return fmt.Errorf("commit verdict tx: %w", err)
			}
			return nil
		}()
		if err != nil {
			return nil, err
		}
		for claimID, target := range applied {
			if claim := claimsByID[claimID]; claim != nil {
				claim.Confidence = target
			}
		}
		// An applied downgrade IS the machine action; only the remaining
		// disagreements surface as review flags.
		for claimID, flag := range batchFlags {
			if _, wasApplied := applied[claimID]; wasApplied {
				continue
			}
			reviewFlags = append(reviewFlags, flag)
		}
		stats.Claims.Checked += len(rows)
		logf("[batch %d] %d/%d verdict(s) (%d call(s), %d in-tokens)",
			stats.Adversarial.Batches, len(rows), len(batch), tracker.Calls, tracker.InputTokens)
	}

	stats.Usage.InputTokens = tracker.InputTokens
	stats.Usage.OutputTokens = tracker.OutputTokens
	if tracker.Priced() {
		cost := tracker.CostUSD()
		stats.Usage.CostUSD = &cost
	}
	stats.Usage.AvgLatencySecs = tracker.AvgLatency()
	stats.Usage.MaxLatencySecs = tracker.MaxLatency()
	return reviewFlags, nil
}

// batchReviews splits reviews into model-call batches.
func batchReviews(reviews []ClaimReview, size int) [][]ClaimReview {
	if size <= 0 {
		return [][]ClaimReview{reviews}
	}
	batches := [][]ClaimReview{}
	for start := 0; start < len(reviews); start += size {
		end := start + size
		if end > len(reviews) {
			end = len(reviews)
		}
		batches = append(batches, reviews[start:end])
	}
	return batches
}

// verdictOutcome maps one verdict to its machine action: an agree at or
// above the threshold stands; a downgrade with a valid strictly-lower
// target downgrades; everything else is flagged for review.
func verdictOutcome(claim *GraphClaim, verdict WireVerdict, agreementThreshold float64) (Downgrade, *Finding) {
	agreement := verdict.EffectiveAgreement()
	if verdict.Verdict == "agree" && agreement >= agreementThreshold {
		return Downgrade{}, nil
	}
	if verdict.Verdict == "downgrade" &&
		verdict.TargetConfidence != "" &&
		ConfidenceOrder[verdict.TargetConfidence] > ConfidenceOrder[claim.Confidence] {
		return Downgrade{
			ClaimID:   claim.ID,
			Current:   claim.Confidence,
			Target:    verdict.TargetConfidence,
			CheckCode: "adversarial_downgrade",
		}, nil
	}
	return Downgrade{}, &Finding{
		CheckCode:  "adversarial_review",
		RecordKind: "claim",
		RecordID:   claim.ID,
		Severity:   SeverityReview,
		Message: fmt.Sprintf("adversarial verdict %q (agreement %.2f) on a %s claim",
			verdict.Verdict, agreement, claim.Confidence),
		Details: map[string]any{
			"verdict":           verdict.Verdict,
			"agreement":         agreement,
			"target_confidence": orDefault(verdict.TargetConfidence, ""),
			"rationale":         verdict.Rationale,
		},
	}
}

// applyEngineActions lands the deterministic engine's machine actions.
func applyEngineActions(ctx context.Context, pool *pgxpool.Pool, engine *Report, stats *Stats) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("validate: begin engine tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, downgrade := range engine.Downgrades {
		ok, err := ApplyDowngrade(ctx, tx, downgrade)
		if err != nil {
			return err
		}
		if ok {
			stats.Machine.DowngradesApplied[downgrade.CheckCode]++
			if downgrade.Target == ConfidenceContested {
				stats.Machine.ClaimsContested++
			}
		}
	}
	outcome, err := RegisterContradictions(ctx, tx, engine.Registrations)
	if err != nil {
		return err
	}
	stats.Machine.ContradictionsCreated = outcome.Created
	stats.Machine.ContradictionsReused = outcome.Reused

	return tx.Commit(ctx)
}

func writeRunFlags(ctx context.Context, pool *pgxpool.Pool, runID int64, flags []Finding, stats *Stats) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("validate: begin flags tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := WriteFlags(ctx, tx, runID, flags); err != nil {
		return err
	}
	cleared, err := ClearStaleFlags(ctx, tx, runID)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("validate: commit flags tx: %w", err)
	}
	stats.Flags.Cleared = cleared
	return nil
}

// callBatch performs the model calls and wire parsing for one batch.
func callBatch(ctx context.Context, client llm.Completer, system, user string, maxAttempts int) (verdicts []WireVerdict, responses []llm.Response, attempts, repairs int, batchErr string) {
	prompt := user
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		attempts++
		response, err := client.Complete(ctx, system, prompt)
		if err != nil {
			var overload *llm.OverloadError
			if errors.As(err, &overload) {
				batchErr = "input too large: " + err.Error()
				break
			}
			batchErr = err.Error()
			continue
		}
		responses = append(responses, response)
		parsed, _, parseErr := ParseVerdicts(response.Text)
		if parseErr != nil {
			batchErr = "unparseable response: " + parseErr.Error()
			if attempt < maxAttempts {
				repairs++
				prompt = user + "\n\nYour previous reply was not valid JSON (" +
					parseErr.Error() + "). Return ONLY the JSON object."
			}
			continue
		}
		return parsed, responses, attempts, repairs, ""
	}
	return nil, responses, attempts, repairs, batchErr
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
		2000.0/1e6*tracker.PriceOutMTok
}

func budgetAllows(tracker *llm.CostTracker, budget float64, nextEstimate float64) bool {
	if !tracker.Priced() {
		return true
	}
	return tracker.CostUSD()+nextEstimate <= budget
}

func joinClaimIDs(batch []ClaimReview, head int) string {
	ids := make([]string, 0, len(batch))
	for i, review := range batch {
		if head > 0 && i >= head {
			ids = append(ids, fmt.Sprintf("+%d", len(batch)-head))
			break
		}
		ids = append(ids, review.ClaimID)
	}
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ","
		}
		out += id
	}
	return out
}

func joinNames(names []string) string {
	out := ""
	for i, name := range names {
		if i > 0 {
			out += ", "
		}
		out += name
	}
	return out
}
