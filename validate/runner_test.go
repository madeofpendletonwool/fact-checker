package validate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/madeofpendletonwool/fact-checker/internal/llm"
	"github.com/madeofpendletonwool/fact-checker/internal/migrations"
	"github.com/madeofpendletonwool/fact-checker/internal/testdb"
)

// stubAdapter fixes the reference source and (optionally) registers a
// custom check; runner behaviour is what is under test.
type stubAdapter struct {
	referenceSource string
	checks          []Check
}

func (a *stubAdapter) ValidationBrief() Brief {
	return Brief{Name: "Test Corpus", Mission: "exercising the validation runner"}
}

func (a *stubAdapter) ReferenceSource() string { return a.referenceSource }

func (a *stubAdapter) Checks() []Check { return a.checks }

// fakeCompleter replays canned responses without any network.
type fakeCompleter struct {
	model   string
	respond func(call int, system, user string) (string, error)
	calls   atomic.Int32
	mu      sync.Mutex
	prompts []string
}

func (f *fakeCompleter) Model() string { return f.model }

func (f *fakeCompleter) Complete(_ context.Context, system, user string) (llm.Response, error) {
	call := int(f.calls.Add(1))
	f.mu.Lock()
	f.prompts = append(f.prompts, system+"\n---\n"+user)
	f.mu.Unlock()
	text, err := f.respond(call, system, user)
	if err != nil {
		return llm.Response{}, err
	}
	return llm.Response{
		Text: text, Model: f.model,
		InputTokens: 200, OutputTokens: 80, FinishReason: "stop",
		LatencySeconds: 0.01,
	}, nil
}

func (f *fakeCompleter) prompt(n int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prompts[n]
}

const seedSchema = `
INSERT INTO sources (id, tier, title, citation_form, locator_scheme) VALUES
  ('ref', 3, 'Reference Docs', 'Ref', '{"required_keys": ["url"]}'::jsonb),
  ('sil', 1, 'Primary One', 'Primary One', '{"required_keys": ["chapter"]}'::jsonb),
  ('ut', 1, 'Primary Two', 'Primary Two', '{"required_keys": ["part"]}'::jsonb);
INSERT INTO raw_documents (source_id, title, url, fetched_at, content_text) VALUES
  ('ref', 'Doc One', 'https://ref/doc-1', now(), 'Doc one body: the service listens on 8080.'),
  ('ref', 'Doc Two', 'https://ref/doc-2', now(), 'Doc two body: the region is us-east.');
INSERT INTO entities (id, kind, canonical_name) VALUES
  ('svc', 'service', 'svc'),
  ('other-svc', 'service', 'other-svc');
INSERT INTO relationship_types (rel_type, category, description) VALUES
  ('depends_on', 'deployment', 'subject depends on object');
`

// seedClaim inserts a claim with its citations. Citations are
// (sourceID, locator-json) pairs attached to the claim as a whole.
func seedClaim(t *testing.T, pool *pgxpool.Pool, id, statement, confidence, subject, predicate, objectEntity, objectValue string, citations ...[2]string) {
	t.Helper()
	var subjectPtr, predicatePtr, objectEntityPtr, objectValuePtr *string
	if subject != "" {
		subjectPtr = &subject
	}
	if predicate != "" {
		predicatePtr = &predicate
	}
	if objectEntity != "" {
		objectEntityPtr = &objectEntity
	}
	if objectValue != "" {
		objectValuePtr = &objectValue
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO claims (id, statement, subject_entity_id, predicate,
		                    object_entity_id, object_value, confidence)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, statement, subjectPtr, predicatePtr, objectEntityPtr, objectValuePtr, confidence); err != nil {
		t.Fatalf("seed claim %s: %v", id, err)
	}
	for _, citation := range citations {
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO claim_sources (claim_id, version_id, source_id, locator)
			VALUES ($1, NULL, $2, $3::jsonb)`,
			id, citation[0], citation[1]); err != nil {
			t.Fatalf("seed citation %s->%s: %v", id, citation[0], err)
		}
	}
}

func setupDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := testdb.Acquire(t, "validate_runner")
	if err := migrations.Up(url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), seedSchema); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return pool
}

func runOpts() Options { return DefaultOptions() }

func assertCount(t *testing.T, pool *pgxpool.Pool, want int, query string, args ...any) {
	t.Helper()
	var got int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&got); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	if got != want {
		t.Fatalf("query %q returned %d, want %d", query, got, want)
	}
}

func confidence(t *testing.T, pool *pgxpool.Pool, claimID string) string {
	t.Helper()
	var confidence string
	if err := pool.QueryRow(context.Background(),
		`SELECT confidence FROM claims WHERE id = $1`, claimID).Scan(&confidence); err != nil {
		t.Fatal(err)
	}
	return confidence
}

func TestRunOfflineEngineAppliesMachineActions(t *testing.T) {
	pool := setupDB(t)
	seedClaim(t, pool, "says-8080", "svc listens on 8080.", ConfidenceEstablished, "svc", "listen_port", "", "8080",
		[2]string{"ref", `{"url": "https://ref/doc-1"}`}, [2]string{"sil", `{"chapter": "Ports"}`})
	seedClaim(t, pool, "says-9090", "svc listens on 9090.", ConfidenceEstablished, "svc", "listen_port", "", "9090",
		[2]string{"ref", `{"url": "https://ref/doc-2"}`}, [2]string{"ut", `{"part": "Legacy"}`})
	seedClaim(t, pool, "tierless", "other-svc is deprecated.", ConfidenceEstablished, "other-svc", "status", "", "deprecated",
		[2]string{"ref", `{"url": "https://ref/doc-2"}`})
	seedClaim(t, pool, "dup-one", "svc serves traffic.", ConfidenceDerivedProbable, "svc", "serves", "", "traffic",
		[2]string{"sil", `{"chapter": "Ports"}`})
	seedClaim(t, pool, "dup-two", "svc serves traffic.", ConfidenceDerivedProbable, "svc", "serves", "", "traffic",
		[2]string{"ut", `{"part": "Legacy"}`})
	seedClaim(t, pool, "shapeless", "Something vague.", ConfidenceDerivedProbable, "svc", "", "", "",
		[2]string{"sil", `{"chapter": "Vague"}`})

	adapter := &stubAdapter{referenceSource: "ref"}
	stats, err := Run(context.Background(), pool, adapter, nil, func() Options {
		o := runOpts()
		o.Offline = true
		return o
	}())
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Offline || stats.Claims.Total != 6 {
		t.Fatalf("offline stats wrong: %+v", stats)
	}

	// Machine actions: conflicts contested, tierless established downgraded.
	if got := confidence(t, pool, "says-8080"); got != ConfidenceContested {
		t.Fatalf("conflict must contest says-8080, got %s", got)
	}
	if got := confidence(t, pool, "says-9090"); got != ConfidenceContested {
		t.Fatalf("conflict must contest says-9090, got %s", got)
	}
	if got := confidence(t, pool, "tierless"); got != ConfidenceDerivedProbable {
		t.Fatalf("tierless established must downgrade, got %s", got)
	}
	if stats.Machine.ContradictionsCreated != 1 {
		t.Fatalf("one contradiction must register: %+v", stats.Machine)
	}

	// Contradiction registered with per-source versions and pinned citations.
	assertCount(t, pool, 1, `SELECT count(*) FROM contradictions WHERE id = 'svc-listen-port-traditions' AND status = 'open'`)
	assertCount(t, pool, 2, `SELECT count(*) FROM contradiction_claims WHERE contradiction_id = 'svc-listen-port-traditions'`)
	assertCount(t, pool, 2, `SELECT count(*) FROM claim_versions`)
	// Every claim's whole-claim citations are pinned to its version (two
	// citations per claim here), so the traditions stay separable.
	assertCount(t, pool, 4, `SELECT count(*) FROM claim_sources cs JOIN claim_versions cv ON cv.id = cs.version_id`)
	assertCount(t, pool, 1, `SELECT count(*) FROM claim_sources cs
		JOIN claim_versions cv ON cv.id = cs.version_id
		WHERE cv.label = 'primary-one' AND cs.source_id = 'sil' AND cv.claim_id = 'says-8080'`)
	assertCount(t, pool, 1, `SELECT count(*) FROM claim_sources cs
		JOIN claim_versions cv ON cv.id = cs.version_id
		WHERE cv.label = 'primary-two' AND cs.source_id = 'ut' AND cv.claim_id = 'says-9090'`)

	// Findings landed for every deterministic path exercised here. (The
	// dangling_reference rule — defensive against snapshots the FKs
	// cannot guarantee — is pinned by the pure engine tests.)
	for _, code := range []string{
		"predicate_value_conflict", "established_without_tier1",
		"duplicate_claim", "claim_shape_invalid",
	} {
		assertCount(t, pool, 1, `SELECT count(*) FROM validation_flags WHERE check_code = $1`, code)
	}

	// Offline means no model traffic and no verdicts.
	assertCount(t, pool, 0, `SELECT count(*) FROM validation_verdicts`)
	assertCount(t, pool, 0, `SELECT count(*) FROM model_outputs`)
	assertCount(t, pool, 1, `SELECT count(*) FROM pipeline_runs WHERE run_kind = 'validate' AND status = 'ok'`)

	var statsJSON []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT stats FROM pipeline_runs WHERE run_kind = 'validate'`).Scan(&statsJSON); err != nil {
		t.Fatal(err)
	}
	var recorded map[string]any
	if err := json.Unmarshal(statsJSON, &recorded); err != nil {
		t.Fatalf("run stats must be JSON: %v", err)
	}
	if _, ok := recorded["machine_actions"]; !ok {
		t.Fatalf("run stats must carry machine actions: %s", statsJSON)
	}
}

func TestRunAdversarialE2E(t *testing.T) {
	pool := setupDB(t)
	seedClaim(t, pool, "agree-claim", "svc listens on 8080.", ConfidenceEstablished, "svc", "listen_port", "", "8080",
		[2]string{"ref", `{"url": "https://ref/doc-1"}`}, [2]string{"sil", `{"chapter": "Ports"}`})
	seedClaim(t, pool, "downgrade-claim", "svc is highly available.", ConfidenceEstablished, "svc", "availability", "", "high",
		[2]string{"ref", `{"url": "https://ref/doc-1"}`}, [2]string{"sil", `{"chapter": "Reliability"}`})
	seedClaim(t, pool, "flag-claim", "svc retries automatically.", ConfidenceDerivedProbable, "svc", "retry", "", "automatic",
		[2]string{"ref", `{"url": "https://ref/doc-2"}`}, [2]string{"sil", `{"chapter": "Reliability"}`})
	seedClaim(t, pool, "stuck-claim", "svc is contested already.", ConfidenceContested, "svc", "state", "", "contested",
		[2]string{"ref", `{"url": "https://ref/doc-1"}`}, [2]string{"sil", `{"chapter": "State"}`})
	seedClaim(t, pool, "blocked-claim", "other-svc is fine.", ConfidenceDerivedProbable, "other-svc", "status", "", "fine",
		[2]string{"sil", `{"chapter": "State"}`})

	payload := `{"verdicts": [
		{"claim_id": "agree-claim", "verdict": "agree", "agreement": 0.95, "rationale": "plainly stated"},
		{"claim_id": "downgrade-claim", "verdict": "downgrade", "agreement": 0.4,
		 "target_confidence": "derived_probable", "rationale": "no HA evidence in passage"},
		{"claim_id": "flag-claim", "verdict": "flag_review", "agreement": 0.5, "rationale": "cannot decide"},
		{"claim_id": "stuck-claim", "verdict": "downgrade", "agreement": 0.3,
		 "target_confidence": "contested", "rationale": "target not downward"}
	]}`
	client := &fakeCompleter{model: "fake-validator", respond: func(int, string, string) (string, error) {
		return payload, nil
	}}

	reportDir := t.TempDir()
	opts := runOpts()
	opts.ReportDir = reportDir
	stats, err := Run(context.Background(), pool, &stubAdapter{referenceSource: "ref"}, client, opts)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Claims.Checked != 4 || stats.Claims.Blocked != 1 {
		t.Fatalf("claim accounting wrong: %+v", stats.Claims)
	}
	if stats.Adversarial.Verdicts["agree"] != 1 || stats.Adversarial.Verdicts["downgrade"] != 2 ||
		stats.Adversarial.Verdicts["flag_review"] != 1 {
		t.Fatalf("verdict counts wrong: %+v", stats.Adversarial)
	}

	// Ledger: first-write-wins rows for the four checked claims.
	assertCount(t, pool, 4, `SELECT count(*) FROM validation_verdicts WHERE prompt_version = 'validate-001'`)
	assertCount(t, pool, 1, `SELECT count(*) FROM validation_verdicts
		WHERE claim_id = 'downgrade-claim' AND verdict = 'downgrade' AND target_confidence = 'derived_probable' AND applied`)
	assertCount(t, pool, 1, `SELECT count(*) FROM validation_verdicts
		WHERE claim_id = 'stuck-claim' AND NOT applied`)

	// Machine action applied: only strictly-downward moves land.
	if got := confidence(t, pool, "downgrade-claim"); got != ConfidenceDerivedProbable {
		t.Fatalf("adversarial downgrade must apply, got %s", got)
	}
	if got := confidence(t, pool, "stuck-claim"); got != ConfidenceContested {
		t.Fatalf("non-downward target must not move, got %s", got)
	}
	if got := confidence(t, pool, "agree-claim"); got != ConfidenceEstablished {
		t.Fatalf("agree must stand, got %s", got)
	}

	// Review flags for the disagreements a human must see.
	assertCount(t, pool, 1, `SELECT count(*) FROM validation_flags WHERE check_code = 'adversarial_review' AND record_id = 'flag-claim'`)
	assertCount(t, pool, 1, `SELECT count(*) FROM validation_flags WHERE check_code = 'adversarial_review' AND record_id = 'stuck-claim'`)
	assertCount(t, pool, 0, `SELECT count(*) FROM validation_flags WHERE check_code = 'adversarial_review' AND record_id = 'downgrade-claim'`)
	assertCount(t, pool, 1, `SELECT count(*) FROM validation_flags WHERE check_code = 'missing_reference_citation' AND record_id = 'blocked-claim'`)

	// Raw responses verbatim, keyed by prompt version.
	assertCount(t, pool, 1, `SELECT count(*) FROM model_outputs
		WHERE record_kind = 'validation_verdict' AND prompt_version = 'validate-001' AND model = 'fake-validator'`)

	// The prompt carried the skeptical stance and the cited passage.
	prompt := client.prompt(0)
	for _, needle := range []string{
		"ATTACK THE CLAIM",
		"agree-claim",
		"Doc one body: the service listens on 8080.",
		`"chapter":"Ports"`,
		"Test Corpus",
	} {
		if !strings.Contains(prompt, needle) {
			t.Fatalf("prompt missing %q", needle)
		}
	}

	// Reports published.
	for _, name := range []string{FlagRatesJSON, FlagRatesMD, ContradictionsJSON, ContradictionsMD, ReviewQueueJSON, ReviewQueueMD} {
		if _, err := os.Stat(filepath.Join(reportDir, name)); err != nil {
			t.Fatalf("report %s missing: %v", name, err)
		}
	}
	ratesRaw, err := os.ReadFile(filepath.Join(reportDir, FlagRatesJSON))
	if err != nil {
		t.Fatal(err)
	}
	var rates map[string]any
	if err := json.Unmarshal(ratesRaw, &rates); err != nil {
		t.Fatalf("flag_rates.json must parse: %v", err)
	}
	if _, ok := rates["adversarial"]; !ok {
		t.Fatalf("flag rates must summarize adversarial verdicts: %s", ratesRaw)
	}
}

func TestRunResumesByVerdictLedger(t *testing.T) {
	pool := setupDB(t)
	seedClaim(t, pool, "claim-one", "svc listens on 8080.", ConfidenceEstablished, "svc", "listen_port", "", "8080",
		[2]string{"ref", `{"url": "https://ref/doc-1"}`}, [2]string{"sil", `{"chapter": "Ports"}`})
	seedClaim(t, pool, "claim-two", "other-svc is deprecated.", ConfidenceDerivedProbable, "other-svc", "status", "", "deprecated",
		[2]string{"ref", `{"url": "https://ref/doc-2"}`}, [2]string{"sil", `{"chapter": "State"}`})

	first := `{"verdicts": [{"claim_id": "claim-one", "verdict": "agree", "agreement": 0.9}]}`
	second := `{"verdicts": [{"claim_id": "claim-two", "verdict": "agree", "agreement": 0.9}]}`
	client := &fakeCompleter{model: "fake", respond: func(call int, _ string, _ string) (string, error) {
		if call == 1 {
			return first, nil
		}
		return second, nil
	}}
	adapter := &stubAdapter{referenceSource: "ref"}

	opts := runOpts()
	opts.Limit = 1
	opts.ReportDir = ""
	stats, err := Run(context.Background(), pool, adapter, client, opts)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Claims.Checked != 1 || stats.Claims.Deferred != 1 {
		t.Fatalf("limited run wrong: %+v", stats.Claims)
	}

	stats2, err := Run(context.Background(), pool, adapter, client, runOpts())
	if err != nil {
		t.Fatal(err)
	}
	if stats2.Claims.VerdictedPrior != 1 || stats2.Claims.Checked != 1 || stats2.Claims.Deferred != 0 {
		t.Fatalf("resume run wrong: %+v", stats2.Claims)
	}
	assertCount(t, pool, 2, `SELECT count(*) FROM validation_verdicts`)
	assertCount(t, pool, 2, `SELECT count(*) FROM model_outputs WHERE record_kind = 'validation_verdict'`)

	// Idempotent third run: everything verdicted, no more model calls.
	stats3, err := Run(context.Background(), pool, adapter, client, runOpts())
	if err != nil {
		t.Fatal(err)
	}
	if stats3.Claims.VerdictedPrior != 2 || stats3.Claims.Checked != 0 {
		t.Fatalf("third run must fully skip: %+v", stats3.Claims)
	}
	assertCount(t, pool, 2, `SELECT count(*) FROM validation_verdicts`)
}

func TestRunRepairsInvalidJSON(t *testing.T) {
	pool := setupDB(t)
	seedClaim(t, pool, "claim-one", "svc listens on 8080.", ConfidenceEstablished, "svc", "listen_port", "", "8080",
		[2]string{"ref", `{"url": "https://ref/doc-1"}`}, [2]string{"sil", `{"chapter": "Ports"}`})
	client := &fakeCompleter{model: "fake", respond: func(call int, _ string, _ string) (string, error) {
		if call%2 == 1 {
			return "sorry, prose instead of JSON", nil
		}
		return "```json\n{\"verdicts\": [{\"claim_id\": \"claim-one\", \"verdict\": \"agree\", \"agreement\": 0.9}]}\n```", nil
	}}
	stats, err := Run(context.Background(), pool, &stubAdapter{referenceSource: "ref"}, client, runOpts())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Adversarial.Repairs < 1 || stats.Claims.Checked != 1 {
		t.Fatalf("repair stats wrong: %+v %+v", stats.Adversarial, stats.Claims)
	}
	assertCount(t, pool, 1, `SELECT count(*) FROM validation_verdicts WHERE claim_id = 'claim-one'`)
	assertCount(t, pool, 2, `SELECT count(*) FROM model_outputs WHERE record_kind = 'validation_verdict'`)
}

func TestRunBudgetHaltsCleanly(t *testing.T) {
	pool := setupDB(t)
	seedClaim(t, pool, "claim-one", "svc listens on 8080.", ConfidenceEstablished, "svc", "listen_port", "", "8080",
		[2]string{"ref", `{"url": "https://ref/doc-1"}`}, [2]string{"sil", `{"chapter": "Ports"}`})
	seedClaim(t, pool, "claim-two", "other-svc is deprecated.", ConfidenceDerivedProbable, "other-svc", "status", "", "deprecated",
		[2]string{"ref", `{"url": "https://ref/doc-2"}`}, [2]string{"sil", `{"chapter": "State"}`})
	client := &fakeCompleter{model: "fake", respond: func(int, string, string) (string, error) {
		return `{"verdicts": []}`, nil
	}}

	opts := runOpts()
	opts.BatchSize = 1
	opts.PriceInMTok = 3
	opts.PriceOutMTok = 15
	opts.BudgetUSD = 0.000001
	stats, err := Run(context.Background(), pool, &stubAdapter{referenceSource: "ref"}, client, opts)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Claims.Deferred < 1 || stats.Usage.CostUSD == nil {
		t.Fatalf("budget stop wrong: %+v %+v", stats.Claims, stats.Usage)
	}
	assertCount(t, pool, 1, `SELECT count(*) FROM pipeline_runs WHERE run_kind = 'validate' AND status = 'ok'`)
}

func TestRunDryRunWritesNothing(t *testing.T) {
	pool := setupDB(t)
	seedClaim(t, pool, "shapeless", "Something vague.", ConfidenceDerivedProbable, "svc", "", "", "",
		[2]string{"ref", `{"url": "https://ref/doc-1"}`})

	stats, err := Run(context.Background(), pool, &stubAdapter{referenceSource: "ref"}, nil, func() Options {
		o := runOpts()
		o.DryRun = true
		return o
	}())
	if err != nil {
		t.Fatal(err)
	}
	if !stats.DryRun || stats.Flags.Total != 1 {
		t.Fatalf("dry run must report engine findings: %+v", stats)
	}
	for _, table := range []string{"validation_verdicts", "validation_flags", "model_outputs", "claim_versions", "contradictions"} {
		assertCount(t, pool, 0, `SELECT count(*) FROM `+table)
	}
	assertCount(t, pool, 1, `SELECT count(*) FROM pipeline_runs WHERE run_kind = 'validate' AND status = 'ok'`)
}

func TestFlagLifecyclePreservesHumanDecisionsAndClearsStale(t *testing.T) {
	pool := setupDB(t)
	seedClaim(t, pool, "dup-one", "svc serves traffic.", ConfidenceDerivedProbable, "svc", "serves", "", "traffic",
		[2]string{"sil", `{"chapter": "Ports"}`})
	seedClaim(t, pool, "dup-two", "svc serves traffic.", ConfidenceDerivedProbable, "svc", "serves", "", "traffic",
		[2]string{"ut", `{"part": "Legacy"}`})
	seedClaim(t, pool, "shapeless", "Something vague.", ConfidenceDerivedProbable, "svc", "", "", "",
		[2]string{"sil", `{"chapter": "Vague"}`})
	adapter := &stubAdapter{referenceSource: "ref"}

	offline := func() Options {
		o := runOpts()
		o.Offline = true
		return o
	}
	if _, err := Run(context.Background(), pool, adapter, nil, offline()); err != nil {
		t.Fatal(err)
	}
	assertCount(t, pool, 2, `SELECT count(*) FROM validation_flags WHERE review_status = 'open'`)

	// A human dismisses the shapeless finding; the engine must not
	// clobber that decision on re-runs.
	if _, err := pool.Exec(context.Background(), `
		UPDATE validation_flags SET review_status = 'dismissed', reviewed_at = now()
		WHERE check_code = 'claim_shape_invalid' AND record_id = 'shapeless'`); err != nil {
		t.Fatal(err)
	}

	// The duplicate heals: one of the twins leaves the graph.
	if _, err := pool.Exec(context.Background(), `DELETE FROM claims WHERE id = 'dup-two'`); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), pool, adapter, nil, offline()); err != nil {
		t.Fatal(err)
	}

	assertCount(t, pool, 1, `SELECT count(*) FROM validation_flags WHERE review_status = 'dismissed'`)
	assertCount(t, pool, 1, `SELECT count(*) FROM validation_flags
		WHERE check_code = 'duplicate_claim' AND review_status = 'cleared'`)
	assertCount(t, pool, 0, `SELECT count(*) FROM validation_flags WHERE review_status = 'open'`)
}

func TestRunAdapterCustomCheckAlongsideCore(t *testing.T) {
	pool := setupDB(t)
	seedClaim(t, pool, "wordy", "This claim statement is far too long for the house style.", ConfidenceDerivedProbable,
		"svc", "style", "", "verbose",
		[2]string{"sil", `{"chapter": "Style"}`})
	seedClaim(t, pool, "shapeless", "Something vague.", ConfidenceDerivedProbable, "svc", "", "", "",
		[2]string{"sil", `{"chapter": "Vague"}`})

	custom := func(state *GraphState, report *Report) {
		for i := range state.Claims {
			if len(state.Claims[i].Statement) > 40 {
				report.Flags = append(report.Flags, Finding{
					CheckCode:  "statement_too_long",
					RecordKind: "claim",
					RecordID:   state.Claims[i].ID,
					Severity:   SeverityWarning,
					Message:    "statement exceeds the adapter's 40-char bound",
				})
			}
		}
	}
	adapter := &stubAdapter{referenceSource: "ref", checks: []Check{custom}}
	opts := runOpts()
	opts.Offline = true
	stats, err := Run(context.Background(), pool, adapter, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	assertCount(t, pool, 1, `SELECT count(*) FROM validation_flags WHERE check_code = 'statement_too_long' AND severity = 'warning'`)
	assertCount(t, pool, 1, `SELECT count(*) FROM validation_flags WHERE check_code = 'claim_shape_invalid'`)
	if stats.WarningCount() != 1 {
		t.Fatalf("warning gate wrong: %+v", stats.Flags)
	}
}

func TestRunRequiresClientUnlessOfflineOrDryRun(t *testing.T) {
	pool := setupDB(t)
	if _, err := Run(context.Background(), pool, &stubAdapter{referenceSource: "ref"}, nil, runOpts()); err == nil {
		t.Fatal("expected error for missing client")
	}
}

func TestVerdictLedgerIsFirstWriteWins(t *testing.T) {
	pool := setupDB(t)
	seedClaim(t, pool, "claim-one", "svc listens on 8080.", ConfidenceEstablished, "svc", "listen_port", "", "8080",
		[2]string{"ref", `{"url": "https://ref/doc-1"}`}, [2]string{"sil", `{"chapter": "Ports"}`})

	var runID int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO pipeline_runs (run_kind, status) VALUES ('validate', 'ok') RETURNING id`).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	first := []VerdictRow{{ClaimID: "claim-one", Verdict: "agree", Agreement: 0.9}}
	if err := WriteVerdicts(context.Background(), tx, runID, PromptVersion, "fake", first); err != nil {
		t.Fatal(err)
	}
	rival := []VerdictRow{{ClaimID: "claim-one", Verdict: "downgrade", Agreement: 0.1,
		TargetConfidence: ConfidenceContested, Rationale: "a later writer loses"}}
	if err := WriteVerdicts(context.Background(), tx, runID, PromptVersion, "fake", rival); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	assertCount(t, pool, 1, `SELECT count(*) FROM validation_verdicts WHERE claim_id = 'claim-one'`)
	assertCount(t, pool, 1, `SELECT count(*) FROM validation_verdicts
		WHERE claim_id = 'claim-one' AND verdict = 'agree' AND NOT applied`)
	assertCount(t, pool, 0, `SELECT count(*) FROM validation_verdicts WHERE verdict = 'downgrade'`)
}
