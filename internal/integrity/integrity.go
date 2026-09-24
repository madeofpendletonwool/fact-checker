// Package integrity runs deterministic integrity checks over the database.
//
// These complement the schema's own constraints: they catch logical
// violations constraints cannot express (uncited claims, locator shape,
// contradiction-register completeness) and verify references when rows were
// loaded with constraints bypassed. Everything here is pure SQL — no model
// calls; the LLM-based validation pass is a separate pipeline stage.
package integrity

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Severity classifies a check's findings. Errors reject the data (the
// integrity command exits nonzero); warnings demand human attention.
type Severity string

// Severities a check can carry.
const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Check is a named, deterministic SQL assertion. The query must return a
// single text column named `ref` identifying each offending record.
type Check struct {
	Code        string
	Severity    Severity
	Description string
	Query       string
}

// Result is a check's outcome over one database state.
type Result struct {
	Check   Check
	Count   int
	Samples []string
}

// Querier is the subset of *pgxpool.Pool the checks need.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// locatorClause builds the predicate "this citation violates the cited
// source's locator_scheme": the scheme names required keys and the citation
// either has no locator at all or is missing at least one of them.
const locatorClause = `%[1]s.locator_scheme ? 'required_keys' AND (
    %[2]s.locator IS NULL OR EXISTS (
        SELECT 1 FROM jsonb_array_elements_text(
            %[1]s.locator_scheme->'required_keys') r(k)
        WHERE NOT (%[2]s.locator ? r.k)))`

// Checks is the engine's core check set. Adapters register further
// domain-specific checks into the stage-2 validation engine; these hold for
// every adapter.
var Checks = []Check{
	{
		Code:     "claims_without_sources",
		Severity: SeverityError,
		Description: "Every claim must cite at least one source " +
			"(uncited records are dropped at extraction, never kept).",
		Query: `SELECT c.id AS ref FROM claims c
            WHERE NOT EXISTS (SELECT 1 FROM claim_sources cs WHERE cs.claim_id = c.id)`,
	},
	{
		Code:        "events_without_sources",
		Severity:    SeverityError,
		Description: "Every event must cite at least one source.",
		Query: `SELECT e.id AS ref FROM events e
            WHERE NOT EXISTS (SELECT 1 FROM event_sources es WHERE es.event_id = e.id)`,
	},
	{
		Code:     "invalid_locators",
		Severity: SeverityError,
		Description: "Citations must carry locators satisfying the cited " +
			"source's locator_scheme.required_keys.",
		Query: fmt.Sprintf(`
            SELECT 'claim:' || cs.claim_id || '->' || cs.source_id AS ref
            FROM claim_sources cs JOIN sources s ON s.id = cs.source_id
            WHERE %s
            UNION ALL
            SELECT 'event:' || es.event_id || '->' || es.source_id AS ref
            FROM event_sources es JOIN sources s ON s.id = es.source_id
            WHERE %s
            UNION ALL
            SELECT 'statement:' || sp.statement_id || '->' || sp.source_id AS ref
            FROM statement_provenance sp JOIN sources s ON s.id = sp.source_id
            WHERE %s`,
			fmt.Sprintf(locatorClause, "s", "cs"),
			fmt.Sprintf(locatorClause, "s", "es"),
			fmt.Sprintf(locatorClause, "s", "sp")),
	},
	{
		Code:     "dangling_references",
		Severity: SeverityError,
		Description: "Logical references must resolve: participants, event " +
			"links, relationship ends and justifying claims, statement " +
			"provenance. Catches rows loaded with constraints bypassed.",
		Query: `
            SELECT 'event_participants:' || ep.event_id || '/' || ep.entity_id AS ref
            FROM event_participants ep
            WHERE NOT EXISTS (SELECT 1 FROM entities e WHERE e.id = ep.entity_id)
            UNION ALL
            SELECT 'event_links:' || el.from_event_id || '/' || el.to_event_id AS ref
            FROM event_links el
            WHERE NOT EXISTS (SELECT 1 FROM events e WHERE e.id = el.from_event_id)
               OR NOT EXISTS (SELECT 1 FROM events e WHERE e.id = el.to_event_id)
            UNION ALL
            SELECT 'relationships:' || r.from_entity_id || '/' || r.to_entity_id AS ref
            FROM relationships r
            WHERE NOT EXISTS (SELECT 1 FROM entities e WHERE e.id = r.from_entity_id)
               OR NOT EXISTS (SELECT 1 FROM entities e WHERE e.id = r.to_entity_id)
               OR (r.claim_id IS NOT NULL
                   AND NOT EXISTS (SELECT 1 FROM claims c WHERE c.id = r.claim_id))
            UNION ALL
            SELECT 'statement_provenance:' || sp.id AS ref
            FROM statement_provenance sp
            WHERE (sp.claim_id IS NOT NULL
                   AND NOT EXISTS (SELECT 1 FROM claims c WHERE c.id = sp.claim_id))
               OR (sp.event_id IS NOT NULL
                   AND NOT EXISTS (SELECT 1 FROM events e WHERE e.id = sp.event_id))
               OR (sp.source_id IS NOT NULL
                   AND NOT EXISTS (SELECT 1 FROM sources s WHERE s.id = sp.source_id))`,
	},
	{
		Code:     "orphan_claims",
		Severity: SeverityWarning,
		Description: "Claims connected to nothing: no subject/object entity, " +
			"no justifying relationship, no contradiction entry, no " +
			"statement provenance.",
		Query: `SELECT c.id AS ref FROM claims c
            WHERE c.subject_entity_id IS NULL AND c.object_entity_id IS NULL
              AND NOT EXISTS (SELECT 1 FROM relationships r WHERE r.claim_id = c.id)
              AND NOT EXISTS (SELECT 1 FROM contradiction_claims cc WHERE cc.claim_id = c.id)
              AND NOT EXISTS (SELECT 1 FROM statement_provenance sp WHERE sp.claim_id = c.id)`,
	},
	{
		Code:     "contested_claims_unregistered",
		Severity: SeverityWarning,
		Description: "Contested and abandoned-version claims should appear in " +
			"the contradiction register (preserved, never smoothed).",
		Query: `SELECT c.id AS ref FROM claims c
            WHERE c.confidence IN ('contested', 'abandoned_version')
              AND NOT EXISTS (
                  SELECT 1 FROM contradiction_claims cc WHERE cc.claim_id = c.id)`,
	},
	{
		Code:     "claim_versions_without_citations",
		Severity: SeverityWarning,
		Description: "Each per-source claim version should be cited by at " +
			"least one claim_sources row pinned to it.",
		Query: `SELECT cv.claim_id || '#' || cv.label AS ref
            FROM claim_versions cv
            WHERE NOT EXISTS (SELECT 1 FROM claim_sources cs WHERE cs.version_id = cv.id)`,
	},
	{
		Code:     "statements_without_provenance",
		Severity: SeverityWarning,
		Description: "Audited statements need provenance rows or an explicit " +
			"interpretive_additions declaration.",
		Query: `SELECT st.id::text AS ref FROM statements st
            WHERE st.interpretive_additions IS NULL
              AND NOT EXISTS (
                  SELECT 1 FROM statement_provenance sp
                  WHERE sp.statement_id = st.id)`,
	},
	{
		Code:     "duplicate_entity_names",
		Severity: SeverityWarning,
		Description: "Two entities of the same kind under the same name — " +
			"the merge planner's territory, never auto-merged by a check.",
		Query: `SELECT e.id AS ref FROM entities e
            WHERE EXISTS (
                SELECT 1 FROM entities o
                WHERE o.id <> e.id AND o.kind = e.kind
                  AND lower(o.canonical_name) = lower(e.canonical_name))`,
	},
}

// ByCode returns the check with the given code.
func ByCode(code string) (Check, bool) {
	for _, c := range Checks {
		if c.Code == code {
			return c, true
		}
	}
	return Check{}, false
}

// Run executes every check against the database. A failing query is an
// error; findings are data, not errors.
func Run(ctx context.Context, q Querier) ([]Result, error) {
	results := make([]Result, 0, len(Checks))
	for _, check := range Checks {
		rows, err := q.Query(ctx, check.Query)
		if err != nil {
			return nil, fmt.Errorf("run check %s: %w", check.Code, err)
		}
		var refs []string
		for rows.Next() {
			var ref string
			if err := rows.Scan(&ref); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan check %s: %w", check.Code, err)
			}
			refs = append(refs, ref)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterate check %s: %w", check.Code, err)
		}
		results = append(results, Result{Check: check, Count: len(refs), Samples: refs})
	}
	return results, nil
}

// ErrorCount totals findings from error-severity checks.
func ErrorCount(results []Result) int {
	return countBySeverity(results, SeverityError)
}

// WarningCount totals findings from warning-severity checks.
func WarningCount(results []Result) int {
	return countBySeverity(results, SeverityWarning)
}

func countBySeverity(results []Result, severity Severity) int {
	n := 0
	for _, r := range results {
		if r.Check.Severity == severity {
			n += r.Count
		}
	}
	return n
}

// ensure the pool satisfies the Querier contract.
var _ Querier = (*pgxpool.Pool)(nil)
