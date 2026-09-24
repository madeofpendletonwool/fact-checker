package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/madeofpendletonwool/fact-checker/internal/llm"
	"github.com/madeofpendletonwool/fact-checker/internal/migrations"
	"github.com/madeofpendletonwool/fact-checker/internal/testdb"
)

// stubAdapter serves fixed units; runner behaviour is what is under test.
type stubAdapter struct {
	units []Unit
}

func (a *stubAdapter) Brief() DomainBrief {
	return DomainBrief{Name: "Test Corpus", Role: "test extractor", Mission: "exercising the runner"}
}

func (a *stubAdapter) BuildUnits(_ context.Context, _ Store) ([]Unit, error) {
	return a.units, nil
}

// fakeCompleter replays canned responses (per call index per unit) without
// any network. Safe for concurrent use.
type fakeCompleter struct {
	model   string
	respond func(call int, system, user string) (string, error)
	calls   atomic.Int32
	mu      sync.Mutex
	prompts []string
	latency time.Duration
}

func (f *fakeCompleter) Model() string { return f.model }

func (f *fakeCompleter) Complete(_ context.Context, system, user string) (llm.Response, error) {
	call := int(f.calls.Add(1))
	f.mu.Lock()
	f.prompts = append(f.prompts, system+"\n---\n"+user)
	f.mu.Unlock()
	if f.latency > 0 {
		time.Sleep(f.latency)
	}
	text, err := f.respond(call, system, user)
	if err != nil {
		return llm.Response{}, err
	}
	return llm.Response{
		Text: text, Model: f.model,
		InputTokens: 100, OutputTokens: 50, FinishReason: "stop",
		LatencySeconds: 0.01,
	}, nil
}

func (f *fakeCompleter) prompt(n int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prompts[n]
}

// cannedPayload is a valid extraction against the seed surface: the
// reference source "ref" (url locator) and the candidate "silmarillion"
// (chapter locator), both-tier policy.
const cannedPayload = `{
  "entities": [
    {"id": "feanor", "kind": "person", "canonical_name": "Fëanor", "names": [], "attributes": {}},
    {"id": "finwe", "kind": "person", "canonical_name": "Finwë", "names": [], "attributes": {}}
  ],
  "events": [
    {"id": "flight-of-the-noldor", "kind": "migration", "summary": "The Noldor leave Valinor.",
     "dating": {"label": "Y.T. 1495"},
     "participants": {"feanor": "leader"},
     "causes": [], "consequences": [],
     "sources": [{"source_id": "ref", "locator": {"url": "https://ref/doc-1"}},
                 {"source_id": "silmarillion", "locator": {"chapter": "Of the Flight of the Noldor"}}]},
    {"id": "uncited-event", "kind": "battle", "summary": "No citations at all.", "sources": []}
  ],
  "claims": [
    {"id": "feanor-son-of-finwe", "statement": "Fëanor is a son of Finwë.",
     "confidence": "established", "subject_entity_id": "feanor", "predicate": "son_of",
     "object_entity_id": "finwe", "sources": [
       {"source_id": "ref", "locator": {"url": "https://ref/doc-1"}},
       {"source_id": "silmarillion", "locator": {"chapter": "Of Fëanor"}}]},
    {"id": "ref-only-claim", "statement": "Only the reference document supports this.",
     "confidence": "established", "sources": [{"source_id": "ref", "locator": {"url": "https://ref/doc-1"}}]},
    {"id": "contested-motives", "statement": "Why Galadriel left Valinor.",
     "confidence": "contested", "subject_entity_id": "feanor",
     "versions": [
       {"label": "published", "statement": "Eager rebellion.", "sources": [
         {"source_id": "ref", "locator": {"url": "https://ref/doc-1"}},
         {"source_id": "silmarillion", "locator": {"chapter": "Of the Flight of the Noldor"}}]},
       {"label": "later-writings", "statement": "Opposed Fëanor.", "sources": [
         {"source_id": "ref", "locator": {"url": "https://ref/doc-1"}},
         {"source_id": "silmarillion", "locator": {"chapter": "Of Fëanor"}}]}
     ],
     "sources": [
       {"source_id": "ref", "locator": {"url": "https://ref/doc-1"}},
       {"source_id": "silmarillion", "locator": {"chapter": "Of the Flight of the Noldor"}}]}
  ],
  "relationships": [
    {"from_entity_id": "finwe", "to_entity_id": "feanor", "rel_type": "father_of", "claim_id": "feanor-son-of-finwe"},
    {"from_entity_id": "feanor", "to_entity_id": "finwe", "rel_type": "member_of"}
  ]
}`

func setupDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := testdb.Acquire(t, "extract_runner")
	if err := migrations.Up(url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	seed := `
	INSERT INTO sources (id, tier, title, citation_form, locator_scheme) VALUES
	  ('ref', 3, 'Reference Docs', 'Ref', '{"required_keys": ["url"]}'::jsonb),
	  ('silmarillion', 1, 'The Silmarillion', 'Silmarillion', '{"required_keys": ["chapter"]}'::jsonb);
	INSERT INTO raw_documents (source_id, title, url, fetched_at, content_text) VALUES
	  ('ref', 'Doc One', 'https://ref/doc-1', now(), 'one'),
	  ('ref', 'Doc Two', 'https://ref/doc-2', now(), 'two'),
	  ('ref', 'Doc Three', 'https://ref/doc-3', now(), 'three'),
	  ('ref', 'Doc Four', 'https://ref/doc-4', now(), 'four'),
	  ('ref', 'Doc Five', 'https://ref/doc-5', now(), 'five');
	INSERT INTO relationship_types (rel_type, category, description) VALUES
	  ('father_of', 'genealogy', 'subject is father of object');
	INSERT INTO relationship_type_aliases (alias, rel_type) VALUES ('parent_of', 'father_of');`
	if _, err := pool.Exec(context.Background(), seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return pool
}

func stubUnits(pool *pgxpool.Pool, count int, textOverride ...string) []Unit {
	docIDs := map[string]int64{}
	rows, _ := pool.Query(context.Background(), `SELECT url, id FROM raw_documents ORDER BY id`)
	for rows.Next() {
		var url string
		var id int64
		_ = rows.Scan(&url, &id)
		docIDs[url] = id
	}
	rows.Close()

	units := []Unit{}
	for i := 1; i <= count; i++ {
		url := fmt.Sprintf("https://ref/doc-%d", i)
		text := "body of document"
		if i-1 < len(textOverride) {
			text = textOverride[i-1]
		}
		units = append(units, Unit{
			UnitKey:    fmt.Sprintf("ref:doc-%d", i),
			DocumentID: docIDs[url],
			Title:      url,
			Text:       text,
			Reference:  Citation{SourceID: "ref", Locator: map[string]any{"url": url}},
			Candidates: []Candidate{{
				SourceID: "silmarillion", Tier: 1,
				Locator: map[string]any{"chapter": "Of the Flight of the Noldor"},
				Title:   "Of the Flight of the Noldor",
			}},
			Policy:   BothTier(),
			RelTypes: []string{"father_of"},
		})
	}
	return units
}

func runOpts() Options {
	return Options{BudgetUSD: 25, MaxUnits: 500, MaxAttempts: 3, Concurrency: 1}
}

func TestRunDryRunPlansWithoutSideEffects(t *testing.T) {
	pool := setupDB(t)
	adapter := &stubAdapter{units: stubUnits(pool, 2)}

	stats, err := Run(context.Background(), pool, adapter, nil, func() Options {
		o := runOpts()
		o.DryRun = true
		return o
	}())
	if err != nil {
		t.Fatal(err)
	}
	if stats.UnitsTotal != 2 || stats.UnitsDone != 0 || stats.UnitsDeferred != 0 {
		t.Fatalf("dry-run stats wrong: %+v", stats)
	}
	for _, table := range []string{"extraction_units", "model_outputs", "claims", "events", "entities", "relationships", "contradictions"} {
		var n int
		if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s must stay empty after a dry run, has %d", table, n)
		}
	}
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM pipeline_runs WHERE run_kind = 'extract'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "ok" {
		t.Fatalf("dry run must record status ok, got %q", status)
	}
}

func TestRunExtractsAndDrops(t *testing.T) {
	pool := setupDB(t)
	adapter := &stubAdapter{units: stubUnits(pool, 2)}
	client := &fakeCompleter{model: "fake", respond: func(int, string, string) (string, error) {
		return cannedPayload, nil
	}}

	stats, err := Run(context.Background(), pool, adapter, client, runOpts())
	if err != nil {
		t.Fatal(err)
	}
	if stats.UnitsDone != 2 || stats.UnitsSkipped != 0 || stats.UnitsFailed != 0 {
		t.Fatalf("unit stats wrong: %+v", stats)
	}
	// Both units replay the same payload, so unit 2's same-id claims and
	// events merge into unit 1's (dropped as already-known — the reference
	// behaviour), while entities/relationships re-validate and dedupe at
	// the constraint.
	if stats.Records["claims"] != 2 || stats.Records["entities"] != 4 || stats.Records["events"] != 1 ||
		stats.Records["relationships"] != 2 || stats.Records["contradictions"] != 1 {
		t.Fatalf("record counts wrong: %+v", stats.Records)
	}
	if stats.DroppedRecords != 9 {
		t.Fatalf("drop count wrong: %+v", stats.DropLog)
	}
	for _, reason := range []string{
		"event:uncited",
		"claim:missing_primary_citation",
		"claim:duplicate or already-known claim id (merged)",
		"event:duplicate or already-known event id (merged)",
		"relationship:unknown rel_type 'member_of'",
	} {
		if stats.DropLog[reason] == 0 {
			t.Fatalf("drop log missing %s: %+v", reason, stats.DropLog)
		}
	}

	// Both contested claims (one per unit, same id) collapse onto one
	// contradiction tradition; claim_versions pin per-tradition citations.
	assertCount(t, pool, 1, `SELECT count(*) FROM contradictions`)
	assertCount(t, pool, 2, `SELECT count(*) FROM claim_versions`)
	assertCount(t, pool, 1, `SELECT count(*) FROM contradiction_claims`)
	assertCount(t, pool, 2, `SELECT count(*) FROM model_outputs`)
	assertCount(t, pool, 2, `SELECT count(*) FROM extraction_units WHERE status = 'done'`)
	assertCount(t, pool, 1, `SELECT count(*) FROM relationships WHERE rel_type = 'father_of'`)
	assertCount(t, pool, 2, `SELECT count(*) FROM claims`)
	assertCount(t, pool, 2, `SELECT count(*) FROM entities`)
	assertCount(t, pool, 1, `SELECT count(*) FROM events`)

	var dropped []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT dropped FROM extraction_units WHERE unit_key = 'ref:doc-1'`).Scan(&dropped); err != nil {
		t.Fatal(err)
	}
	var drops []Drop
	if err := json.Unmarshal(dropped, &drops); err != nil {
		t.Fatal(err)
	}
	if len(drops) != 3 {
		t.Fatalf("ledger drops wrong: %s", dropped)
	}

	// Idempotent re-run: everything skips, nothing duplicates.
	stats2, err := Run(context.Background(), pool, adapter, client, runOpts())
	if err != nil {
		t.Fatal(err)
	}
	if stats2.UnitsSkipped != 2 || stats2.UnitsDone != 0 {
		t.Fatalf("re-run must skip done units: %+v", stats2)
	}
	assertCount(t, pool, 2, `SELECT count(*) FROM claims`)
	// The skipped re-run makes no model calls, so outputs stay at two.
	assertCount(t, pool, 2, `SELECT count(*) FROM model_outputs`)
}

func TestRunResumesByLimit(t *testing.T) {
	pool := setupDB(t)
	adapter := &stubAdapter{units: stubUnits(pool, 2)}
	client := &fakeCompleter{model: "fake", respond: func(int, string, string) (string, error) {
		return cannedPayload, nil
	}}

	opts := runOpts()
	opts.Limit = 1
	stats, err := Run(context.Background(), pool, adapter, client, opts)
	if err != nil {
		t.Fatal(err)
	}
	if stats.UnitsDone != 1 || stats.UnitsDeferred != 1 {
		t.Fatalf("limited run wrong: %+v", stats)
	}

	stats2, err := Run(context.Background(), pool, adapter, client, runOpts())
	if err != nil {
		t.Fatal(err)
	}
	if stats2.UnitsSkipped != 1 || stats2.UnitsDone != 1 || stats2.UnitsDeferred != 0 {
		t.Fatalf("resume run wrong: %+v", stats2)
	}
	assertCount(t, pool, 2, `SELECT count(*) FROM extraction_units WHERE status = 'done'`)
	assertCount(t, pool, 2, `SELECT count(*) FROM claims`)
}

func TestRunRetriesFailedUnit(t *testing.T) {
	pool := setupDB(t)
	adapter := &stubAdapter{units: stubUnits(pool, 2)}
	var calls atomic.Int32
	client := &fakeCompleter{model: "fake", respond: func(_ int, _ string, _ string) (string, error) {
		if calls.Add(1) <= 3 { // first unit exhausts its attempts
			return "", llm.NewError("endpoint on fire")
		}
		return cannedPayload, nil
	}}

	stats, err := Run(context.Background(), pool, adapter, client, runOpts())
	if err != nil {
		t.Fatal(err)
	}
	if stats.UnitsFailed != 1 || stats.UnitsDone != 1 {
		t.Fatalf("failure split wrong: %+v", stats)
	}
	var lastError string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(last_error, '') FROM extraction_units WHERE unit_key = 'ref:doc-1'`).Scan(&lastError); err != nil {
		t.Fatal(err)
	}
	if lastError == "" {
		t.Fatal("failed unit must record its error")
	}

	// A later run retries the failed unit (not skipped) and succeeds.
	stats2, err := Run(context.Background(), pool, adapter, client, runOpts())
	if err != nil {
		t.Fatal(err)
	}
	if stats2.UnitsSkipped != 1 || stats2.UnitsDone != 1 || stats2.UnitsFailed != 0 {
		t.Fatalf("retry run wrong: %+v", stats2)
	}
	assertCount(t, pool, 2, `SELECT count(*) FROM extraction_units WHERE status = 'done'`)
}

func TestRunBudgetHaltsCleanly(t *testing.T) {
	pool := setupDB(t)
	adapter := &stubAdapter{units: stubUnits(pool, 2)}
	client := &fakeCompleter{model: "fake", respond: func(int, string, string) (string, error) {
		return cannedPayload, nil
	}}

	opts := runOpts()
	opts.PriceInMTok = 3
	opts.PriceOutMTok = 15
	opts.BudgetUSD = 0.000001 // smaller than any single-call estimate
	stats, err := Run(context.Background(), pool, adapter, client, opts)
	if err != nil {
		t.Fatal(err)
	}
	if stats.UnitsDone != 0 || stats.UnitsDeferred != 2 {
		t.Fatalf("budget stop wrong: %+v", stats)
	}
	if stats.CostUSD == nil {
		t.Fatal("priced run must report spend")
	}
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM pipeline_runs WHERE run_kind = 'extract' ORDER BY id DESC LIMIT 1`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "ok" {
		t.Fatalf("budget stop is a clean stop, not a failure; got %q", status)
	}
	assertCount(t, pool, 0, `SELECT count(*) FROM claims`)
}

func TestRunUnpricedBudgetFallsBackToUnitCap(t *testing.T) {
	pool := setupDB(t)
	adapter := &stubAdapter{units: stubUnits(pool, 2)}
	client := &fakeCompleter{model: "fake", respond: func(int, string, string) (string, error) {
		return cannedPayload, nil
	}}

	opts := runOpts()
	opts.BudgetUSD = 0.000001 // ignored without prices
	opts.MaxUnits = 1         // the hard cap binds instead
	stats, err := Run(context.Background(), pool, adapter, client, opts)
	if err != nil {
		t.Fatal(err)
	}
	if stats.UnitsDone != 1 || stats.UnitsDeferred != 1 {
		t.Fatalf("unit-cap run wrong: %+v", stats)
	}
}

func TestRunRepairsInvalidJSON(t *testing.T) {
	pool := setupDB(t)
	adapter := &stubAdapter{units: stubUnits(pool, 2)}
	client := &fakeCompleter{model: "fake", respond: func(call int, _ string, _ string) (string, error) {
		if call%2 == 1 {
			return "I am very sorry, but here is some prose instead of JSON.", nil
		}
		return "```json\n" + cannedPayload + "\n```", nil
	}}

	stats, err := Run(context.Background(), pool, adapter, client, runOpts())
	if err != nil {
		t.Fatal(err)
	}
	if stats.UnitsDone != 2 || stats.Repairs < 2 || stats.Attempts >= stats.Repairs*2+1 {
		t.Fatalf("repair stats wrong: %+v", stats)
	}
	if stats.UnitsFailed != 0 {
		t.Fatalf("repairs must rescue the units: %+v", stats)
	}
}

func TestRunOverloadFailsUnitFast(t *testing.T) {
	pool := setupDB(t)
	adapter := &stubAdapter{units: stubUnits(pool, 2)}
	client := &fakeCompleter{model: "fake", respond: func(int, string, string) (string, error) {
		return "", llm.NewOverloadError("HTTP 400: request too large for the model")
	}}

	stats, err := Run(context.Background(), pool, adapter, client, runOpts())
	if err != nil {
		t.Fatal(err)
	}
	if stats.UnitsFailed != 2 || stats.Attempts != 2 {
		t.Fatalf("overload must fail units without retrying: %+v", stats)
	}
}

func TestRunConcurrentWorkers(t *testing.T) {
	pool := setupDB(t)
	adapter := &stubAdapter{units: stubUnits(pool, 5)}
	client := &fakeCompleter{
		model:   "fake",
		latency: 5 * time.Millisecond,
		respond: func(int, string, string) (string, error) { return cannedPayload, nil },
	}

	opts := runOpts()
	opts.Concurrency = 3
	stats, err := Run(context.Background(), pool, adapter, client, opts)
	if err != nil {
		t.Fatal(err)
	}
	if stats.UnitsDone != 5 || stats.UnitsFailed != 0 {
		t.Fatalf("concurrent run wrong: %+v", stats)
	}
	assertCount(t, pool, 5, `SELECT count(*) FROM extraction_units WHERE status = 'done'`)
}

func TestRunInputDriftReextracts(t *testing.T) {
	pool := setupDB(t)
	adapter := &stubAdapter{units: stubUnits(pool, 2)}
	client := &fakeCompleter{model: "fake", respond: func(int, string, string) (string, error) {
		return cannedPayload, nil
	}}
	if _, err := Run(context.Background(), pool, adapter, client, runOpts()); err != nil {
		t.Fatal(err)
	}

	// Same documents, changed text: input hash moves, units re-extract.
	drifted := &stubAdapter{units: stubUnits(pool, 2, "changed body", "changed body")}
	stats, err := Run(context.Background(), pool, drifted, client, runOpts())
	if err != nil {
		t.Fatal(err)
	}
	if stats.UnitsSkipped != 0 || stats.UnitsDone != 2 {
		t.Fatalf("input drift must re-extract: %+v", stats)
	}
	assertCount(t, pool, 2, `SELECT count(*) FROM claims`)
}

func TestRunPromptCarriesCitationSurface(t *testing.T) {
	pool := setupDB(t)
	adapter := &stubAdapter{units: stubUnits(pool, 2)}
	client := &fakeCompleter{model: "fake", respond: func(int, string, string) (string, error) {
		return cannedPayload, nil
	}}
	if _, err := Run(context.Background(), pool, adapter, client, runOpts()); err != nil {
		t.Fatal(err)
	}
	prompt := client.prompt(0)
	for _, needle := range []string{
		`"source_id": "ref"`,
		"silmarillion",
		"Of the Flight of the Noldor",
		"https://ref/doc-1",
		"father_of",
		"EXTRACT AND CITE",
	} {
		if !strings.Contains(prompt, needle) {
			t.Fatalf("prompt missing %q", needle)
		}
	}
}

func TestRunRequiresClientUnlessDryRun(t *testing.T) {
	pool := setupDB(t)
	adapter := &stubAdapter{units: stubUnits(pool, 2)}
	if _, err := Run(context.Background(), pool, adapter, nil, runOpts()); err == nil {
		t.Fatal("expected error for missing client")
	}
}

func assertCount(t *testing.T, pool *pgxpool.Pool, want int, query string) {
	t.Helper()
	var got int
	if err := pool.QueryRow(context.Background(), query).Scan(&got); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	if got != want {
		t.Fatalf("query %q returned %d, want %d", query, got, want)
	}
}
