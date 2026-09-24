package integrity

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/madeofpendletonwool/fact-checker/internal/migrations"
	"github.com/madeofpendletonwool/fact-checker/internal/testdb"
)

func TestCheckRegistry(t *testing.T) {
	seen := make(map[string]bool, len(Checks))
	for _, check := range Checks {
		if check.Code == "" {
			t.Fatal("check with empty code")
		}
		if seen[check.Code] {
			t.Fatalf("duplicate check code %s", check.Code)
		}
		seen[check.Code] = true
		if check.Severity != SeverityError && check.Severity != SeverityWarning {
			t.Fatalf("%s: severity %q invalid", check.Code, check.Severity)
		}
		if !strings.Contains(check.Query, "AS ref") {
			t.Fatalf("%s: query must select a text column named ref", check.Code)
		}
		if check.Description == "" {
			t.Fatalf("%s: missing description", check.Code)
		}
	}
	if _, ok := ByCode("claims_without_sources"); !ok {
		t.Fatal("core check claims_without_sources missing")
	}
}

func TestCounts(t *testing.T) {
	errCheck, ok := ByCode("claims_without_sources")
	if !ok || errCheck.Severity != SeverityError {
		t.Fatal("claims_without_sources must be an error-severity check")
	}
	warnCheck, ok := ByCode("orphan_claims")
	if !ok || warnCheck.Severity != SeverityWarning {
		t.Fatal("orphan_claims must be a warning-severity check")
	}
	results := []Result{
		{Check: errCheck, Count: 2},
		{Check: warnCheck, Count: 3},
	}
	if ErrorCount(results) != 2 {
		t.Errorf("ErrorCount = %d, want 2", ErrorCount(results))
	}
	if WarningCount(results) != 3 {
		t.Errorf("WarningCount = %d, want 3", WarningCount(results))
	}
}

// The Arda corpus shape must load against the generic schema with no schema
// changes and come out clean: the Tolkien ontology rides in row data
// (entity kinds, locator schemes, tier meanings, relationship categories).
func TestArdaFixtureLoadsClean(t *testing.T) {
	pool := resetScratch(t)

	ctx := context.Background()
	results, err := Run(ctx, pool)
	if err != nil {
		t.Fatalf("run integrity: %v", err)
	}
	if got := ErrorCount(results); got != 0 {
		t.Errorf("errors = %d, want 0\n%s", got, formatResults(results))
	}
	if got := WarningCount(results); got != 0 {
		t.Errorf("warnings = %d, want 0\n%s", got, formatResults(results))
	}

	var contested int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM claims WHERE confidence = 'contested'`).Scan(&contested); err != nil {
		t.Fatalf("count contested claims: %v", err)
	}
	if contested != 1 {
		t.Errorf("contested claims = %d, want 1", contested)
	}
	var versions int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM claim_versions cv
		 WHERE NOT EXISTS (SELECT 1 FROM claim_sources cs WHERE cs.version_id = cv.id)`).Scan(&versions); err != nil {
		t.Fatalf("count uncited versions: %v", err)
	}
	if versions != 0 {
		t.Errorf("uncited claim versions = %d, want 0", versions)
	}
}

// The acceptance rejections: uncited claims, citations with invalid locators
// for their source's scheme, dangling entity references.
func TestChecksRejectViolations(t *testing.T) {
	pool := resetScratch(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx,
		`INSERT INTO claims (id, statement, confidence) VALUES
		    ('uncited-claim', 'A claim with no citation.', 'established')`)
	if err != nil {
		t.Fatalf("insert uncited claim: %v", err)
	}

	_, err = pool.Exec(ctx,
		`INSERT INTO claim_sources (claim_id, version_id, source_id, locator)
		 VALUES ('galadriel-role-in-rebellion', NULL, 'the-silmarillion', '{}'::jsonb)`)
	if err != nil {
		t.Fatalf("insert bad-locator citation: %v", err)
	}

	insertDanglingReference(ctx, t, pool)

	results, err := Run(ctx, pool)
	if err != nil {
		t.Fatalf("run integrity: %v", err)
	}
	if got := ErrorCount(results); got == 0 {
		t.Fatalf("errors = 0, want > 0\n%s", formatResults(results))
	}
	for _, code := range []string{
		"claims_without_sources",
		"invalid_locators",
		"dangling_references",
		"orphan_claims",
	} {
		if resultCount(t, results, code) == 0 {
			t.Errorf("check %s found nothing, want > 0\n%s", code, formatResults(results))
		}
	}
}

// A dangling reference requires bypassing the foreign keys, exactly like a
// bulk load with constraints disabled would.
func insertDanglingReference(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET session_replication_role = replica`); err != nil {
		t.Fatalf("disable triggers (requires superuser): %v", err)
	}
	_, err = conn.Exec(ctx,
		`INSERT INTO event_participants (event_id, entity_id) VALUES
		    ('first-kinslaying', 'no-such-entity')`)
	if _, err := conn.Exec(ctx, `SET session_replication_role = DEFAULT`); err != nil {
		t.Fatalf("re-enable triggers: %v", err)
	}
	if err != nil {
		t.Fatalf("insert dangling participant: %v", err)
	}
}

func resultCount(t *testing.T, results []Result, code string) int {
	t.Helper()
	check, ok := ByCode(code)
	if !ok {
		t.Fatalf("unknown check %s", code)
	}
	for _, r := range results {
		if r.Check.Code == check.Code {
			return r.Count
		}
	}
	return 0
}

func formatResults(results []Result) string {
	var b strings.Builder
	for _, r := range results {
		b.WriteString(r.Check.Code)
		b.WriteString(" (")
		b.WriteString(string(r.Check.Severity))
		b.WriteString("): ")
		b.WriteString(strings.Join(r.Samples, ", "))
		b.WriteString("\n")
	}
	return b.String()
}

func resetScratch(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := testdb.Acquire(t, "integrity")
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Cleanup(pool.Close)
		t.Fatalf("ping scratch database: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := migrations.Down(url); err != nil {
		t.Fatalf("reset: down: %v", err)
	}
	if err := migrations.Up(url); err != nil {
		t.Fatalf("reset: up: %v", err)
	}

	fixture, err := os.ReadFile("testdata/arda_fixture.sql")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, string(fixture)); err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	return pool
}
