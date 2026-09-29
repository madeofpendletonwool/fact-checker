// Persistence for the validation stage (idempotent, review-preserving).
//
// Verdicts are first-write-wins per (claim, prompt version) — the resume
// key. Findings are keyed by (check_code, record_kind, record_id):
// re-runs refresh the finding but never clobber a human review decision,
// and open findings the engine no longer reports are marked cleared.
// Machine actions are limited to confidence downgrades and contradiction
// registration.

package validate

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// VerdictRow is one verdict as it lands in the ledger.
type VerdictRow struct {
	ClaimID          string
	Verdict          string
	Agreement        float64
	TargetConfidence string
	Rationale        string
}

// VerdictedClaimIDs returns the resume key: claims already verdicted
// under this prompt version.
func VerdictedClaimIDs(ctx context.Context, pool *pgxpool.Pool, promptVersion string) (map[string]bool, error) {
	rows, err := pool.Query(ctx,
		`SELECT claim_id FROM validation_verdicts WHERE prompt_version = $1`, promptVersion)
	if err != nil {
		return nil, fmt.Errorf("validate: query verdict ledger: %w", err)
	}
	defer rows.Close()
	ids := map[string]bool{}
	for rows.Next() {
		var claimID string
		if err := rows.Scan(&claimID); err != nil {
			return nil, fmt.Errorf("validate: scan verdict: %w", err)
		}
		ids[claimID] = true
	}
	return ids, rows.Err()
}

// WriteVerdicts inserts verdict rows first-write-wins.
func WriteVerdicts(ctx context.Context, tx pgx.Tx, runID int64, promptVersion, model string, verdicts []VerdictRow) error {
	for _, verdict := range verdicts {
		var target any
		if verdict.TargetConfidence != "" {
			target = verdict.TargetConfidence
		}
		var rationale any
		if verdict.Rationale != "" {
			rationale = verdict.Rationale
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO validation_verdicts (run_id, claim_id, prompt_version, model,
			                                 verdict, agreement, target_confidence,
			                                 applied, rationale)
			VALUES ($1, $2, $3, $4, $5, $6, $7, FALSE, $8)
			ON CONFLICT (claim_id, prompt_version) DO NOTHING`,
			runID, verdict.ClaimID, promptVersion, model, verdict.Verdict,
			verdict.Agreement, target, rationale); err != nil {
			return fmt.Errorf("validate: insert verdict %s: %w", verdict.ClaimID, err)
		}
	}
	return nil
}

// MarkVerdictApplied records that a verdict's machine action landed.
func MarkVerdictApplied(ctx context.Context, tx pgx.Tx, claimID, promptVersion string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE validation_verdicts SET applied = TRUE
		WHERE claim_id = $1 AND prompt_version = $2`, claimID, promptVersion); err != nil {
		return fmt.Errorf("validate: mark verdict applied %s: %w", claimID, err)
	}
	return nil
}

// ApplyDowngrade applies a confidence downgrade if it is still strictly
// downward. Guards against drift: the adversarial pass may already have
// downgraded the claim this run, or a human may have changed it since
// the snapshot. Never upgrades, never touches abandoned_version.
func ApplyDowngrade(ctx context.Context, tx pgx.Tx, downgrade Downgrade) (bool, error) {
	var current string
	err := tx.QueryRow(ctx,
		`SELECT confidence FROM claims WHERE id = $1`, downgrade.ClaimID).Scan(&current)
	if err != nil {
		if err == pgx.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("validate: read claim %s: %w", downgrade.ClaimID, err)
	}
	if current == ConfidenceAbandonedVersion {
		return false, nil
	}
	if ConfidenceOrder[downgrade.Target] <= ConfidenceOrder[current] {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE claims SET confidence = $1, updated_at = now() WHERE id = $2`,
		downgrade.Target, downgrade.ClaimID); err != nil {
		return false, fmt.Errorf("validate: downgrade claim %s: %w", downgrade.ClaimID, err)
	}
	return true, nil
}

// RegistrationOutcome tallies what RegisterContradictions did.
type RegistrationOutcome struct {
	Created  int
	Reused   int
	Versions int
}

// RegisterContradictions records conflicts with per-source versions
// preserved, never resolved. For each registration: insert or reuse the
// register entry, link every claim, create the per-claim version rows,
// and pin each claim's own citations to its version so the traditions
// stay separable end-to-end.
func RegisterContradictions(ctx context.Context, tx pgx.Tx, registrations []Registration) (RegistrationOutcome, error) {
	outcome := RegistrationOutcome{}
	for _, registration := range registrations {
		if !registration.Reuse {
			if _, err := tx.Exec(ctx, `
				INSERT INTO contradictions (id, title, description)
				VALUES ($1, $2, $3)
				ON CONFLICT (id) DO NOTHING`,
				registration.ID, registration.Title, registration.Description); err != nil {
				return outcome, fmt.Errorf("validate: insert contradiction %s: %w", registration.ID, err)
			}
			outcome.Created++
		} else {
			outcome.Reused++
		}
		for _, entry := range registration.Claims {
			if _, err := tx.Exec(ctx, `
				INSERT INTO contradiction_claims (contradiction_id, claim_id, tradition_label)
				VALUES ($1, $2, $3)
				ON CONFLICT (contradiction_id, claim_id, version_id) DO NOTHING`,
				registration.ID, entry.ClaimID, entry.VersionLabel); err != nil {
				return outcome, fmt.Errorf("validate: link contradiction %s/%s: %w", registration.ID, entry.ClaimID, err)
			}
			var versionID int64
			if err := tx.QueryRow(ctx, `
				INSERT INTO claim_versions (claim_id, label, statement)
				VALUES ($1, $2, $3)
				ON CONFLICT (claim_id, label) DO UPDATE SET statement = EXCLUDED.statement
				RETURNING id`,
				entry.ClaimID, entry.VersionLabel, entry.Statement).Scan(&versionID); err != nil {
				return outcome, fmt.Errorf("validate: insert claim version %s#%s: %w", entry.ClaimID, entry.VersionLabel, err)
			}
			outcome.Versions++
			// Pin the claim's own citations to its version (per-source versions).
			if _, err := tx.Exec(ctx, `
				INSERT INTO claim_sources (claim_id, version_id, source_id, locator, note)
				SELECT cs.claim_id, $1, cs.source_id, cs.locator, cs.note
				FROM claim_sources cs
				WHERE cs.claim_id = $2 AND cs.version_id IS NULL
				ON CONFLICT (claim_id, version_id, source_id) DO NOTHING`,
				versionID, entry.ClaimID); err != nil {
				return outcome, fmt.Errorf("validate: pin version citations %s#%s: %w", entry.ClaimID, entry.VersionLabel, err)
			}
		}
	}
	return outcome, nil
}

// WriteFlags upserts findings; review decisions survive, everything else
// refreshes.
func WriteFlags(ctx context.Context, tx pgx.Tx, runID int64, flags []Finding) error {
	for _, flag := range flags {
		details, err := json.Marshal(flag.Details)
		if err != nil {
			return fmt.Errorf("validate: marshal flag details: %w", err)
		}
		if flag.Details == nil {
			details = []byte("{}")
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO validation_flags (run_id, check_code, record_kind, record_id,
			                              severity, message, details)
			VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)
			ON CONFLICT (check_code, record_kind, record_id) DO UPDATE SET
				run_id      = EXCLUDED.run_id,
				severity    = EXCLUDED.severity,
				message     = EXCLUDED.message,
				details     = EXCLUDED.details,
				updated_at  = now()`,
			runID, flag.CheckCode, flag.RecordKind, flag.RecordID,
			flag.Severity, flag.Message, string(details)); err != nil {
			return fmt.Errorf("validate: upsert flag %s/%s: %w", flag.CheckCode, flag.RecordID, err)
		}
	}
	return nil
}

// ClearStaleFlags marks open findings the run no longer reports cleared.
func ClearStaleFlags(ctx context.Context, tx pgx.Tx, runID int64) (int, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE validation_flags SET review_status = 'cleared', reviewed_at = now()
		WHERE review_status = 'open' AND run_id <> $1`, runID)
	if err != nil {
		return 0, fmt.Errorf("validate: clear stale flags: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// RecordCounts returns the corpus sizes flag rates normalize against.
func RecordCounts(ctx context.Context, pool *pgxpool.Pool) (map[string]int, error) {
	counts := map[string]int{}
	for kind, table := range map[string]string{
		"claims": "claims", "events": "events",
		"entities": "entities", "relationships": "relationships",
	} {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil {
			return nil, fmt.Errorf("validate: count %s: %w", table, err)
		}
		counts[kind] = count
	}
	return counts, nil
}

// FlagRow is one validation_flags row as reports render it.
type FlagRow struct {
	CheckCode    string
	RecordKind   string
	RecordID     string
	Severity     string
	Message      string
	Details      map[string]any
	ReviewStatus string
}

// RunFlags lists the findings written by one run.
func RunFlags(ctx context.Context, pool *pgxpool.Pool, runID int64) ([]FlagRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT check_code, record_kind, record_id, severity, message, details,
		       review_status
		FROM validation_flags WHERE run_id = $1
		ORDER BY severity, check_code, record_id`, runID)
	if err != nil {
		return nil, fmt.Errorf("validate: query run flags: %w", err)
	}
	defer rows.Close()
	flags := []FlagRow{}
	for rows.Next() {
		var flag FlagRow
		var details []byte
		if err := rows.Scan(&flag.CheckCode, &flag.RecordKind, &flag.RecordID,
			&flag.Severity, &flag.Message, &details, &flag.ReviewStatus); err != nil {
			return nil, fmt.Errorf("validate: scan flag: %w", err)
		}
		if len(details) > 0 {
			decoded, err := decodeJSONObject(details)
			if err != nil {
				return nil, fmt.Errorf("validate: decode flag details: %w", err)
			}
			flag.Details = decoded
		}
		flags = append(flags, flag)
	}
	return flags, rows.Err()
}

// VerdictSummary aggregates one run's verdicts by outcome.
type VerdictSummary struct {
	Total     int
	ByVerdict map[string]VerdictTally
}

// VerdictTally is a per-verdict count and mean agreement.
type VerdictTally struct {
	Count        int
	AvgAgreement float64
}

// RunVerdictSummary summarizes the verdicts recorded by one run.
func RunVerdictSummary(ctx context.Context, pool *pgxpool.Pool, runID int64) (VerdictSummary, error) {
	summary := VerdictSummary{ByVerdict: map[string]VerdictTally{}}
	rows, err := pool.Query(ctx, `
		SELECT verdict, COUNT(*), COALESCE(AVG(agreement), 0)
		FROM validation_verdicts WHERE run_id = $1
		GROUP BY verdict`, runID)
	if err != nil {
		return summary, fmt.Errorf("validate: query verdict summary: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var verdict string
		var tally VerdictTally
		if err := rows.Scan(&verdict, &tally.Count, &tally.AvgAgreement); err != nil {
			return summary, fmt.Errorf("validate: scan verdict summary: %w", err)
		}
		summary.ByVerdict[verdict] = tally
		summary.Total += tally.Count
	}
	return summary, rows.Err()
}

// OpenFlagCounts tallies open findings by severity (for status output).
func OpenFlagCounts(ctx context.Context, pool *pgxpool.Pool) (map[string]int, error) {
	rows, err := pool.Query(ctx, `
		SELECT severity, COUNT(*) FROM validation_flags
		WHERE review_status = 'open' GROUP BY severity`)
	if err != nil {
		return nil, fmt.Errorf("validate: query open flags: %w", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var severity string
		var count int
		if err := rows.Scan(&severity, &count); err != nil {
			return nil, fmt.Errorf("validate: scan open flags: %w", err)
		}
		counts[severity] = count
	}
	return counts, rows.Err()
}

// VerdictLedger tallies the whole ledger by verdict under one prompt
// version (for status output).
func VerdictLedger(ctx context.Context, pool *pgxpool.Pool, promptVersion string) (map[string]int, error) {
	rows, err := pool.Query(ctx, `
		SELECT verdict, COUNT(*) FROM validation_verdicts
		WHERE prompt_version = $1 GROUP BY verdict`, promptVersion)
	if err != nil {
		return nil, fmt.Errorf("validate: query verdict ledger: %w", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var verdict string
		var count int
		if err := rows.Scan(&verdict, &count); err != nil {
			return nil, fmt.Errorf("validate: scan verdict ledger: %w", err)
		}
		counts[verdict] = count
	}
	return counts, rows.Err()
}

// ContradictionEntry is one register export row.
type ContradictionEntry struct {
	ID          string                  `json:"id"`
	Title       string                  `json:"title"`
	Description string                  `json:"description"`
	Status      string                  `json:"status"`
	Resolution  string                  `json:"resolution,omitempty"`
	Claims      []ContradictionClaimRow `json:"claims"`
}

// ContradictionClaimRow is one linked claim with its versions.
type ContradictionClaimRow struct {
	ClaimID    string                    `json:"claim_id"`
	Tradition  string                    `json:"tradition,omitempty"`
	Statement  string                    `json:"statement"`
	Confidence string                    `json:"confidence"`
	Versions   []ContradictionVersionRow `json:"versions"`
}

// ContradictionVersionRow is one preserved tradition with its sources.
type ContradictionVersionRow struct {
	Label     string   `json:"label"`
	Statement string   `json:"statement"`
	Sources   []string `json:"sources"`
}

// ContradictionRegister exports the full register with per-source
// versions.
func ContradictionRegister(ctx context.Context, pool *pgxpool.Pool) ([]ContradictionEntry, error) {
	entries := []ContradictionEntry{}
	entryRows, err := pool.Query(ctx, `
		SELECT id, title, description, status, COALESCE(resolution, '')
		FROM contradictions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("validate: query contradictions: %w", err)
	}
	defer entryRows.Close()
	for entryRows.Next() {
		var entry ContradictionEntry
		if err := entryRows.Scan(&entry.ID, &entry.Title, &entry.Description,
			&entry.Status, &entry.Resolution); err != nil {
			return nil, fmt.Errorf("validate: scan contradiction: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := entryRows.Err(); err != nil {
		return nil, fmt.Errorf("validate: contradictions: %w", err)
	}

	for i := range entries {
		linkRows, err := pool.Query(ctx, `
			SELECT cc.claim_id, COALESCE(cc.tradition_label, ''), c.statement, c.confidence
			FROM contradiction_claims cc JOIN claims c ON c.id = cc.claim_id
			WHERE cc.contradiction_id = $1 ORDER BY cc.claim_id`, entries[i].ID)
		if err != nil {
			return nil, fmt.Errorf("validate: query contradiction claims: %w", err)
		}
		for linkRows.Next() {
			var claim ContradictionClaimRow
			if err := linkRows.Scan(&claim.ClaimID, &claim.Tradition, &claim.Statement,
				&claim.Confidence); err != nil {
				linkRows.Close()
				return nil, fmt.Errorf("validate: scan contradiction claim: %w", err)
			}
			claim.Versions, err = claimVersions(ctx, pool, claim.ClaimID)
			if err != nil {
				linkRows.Close()
				return nil, err
			}
			entries[i].Claims = append(entries[i].Claims, claim)
		}
		if err := linkRows.Err(); err != nil {
			linkRows.Close()
			return nil, fmt.Errorf("validate: contradiction claims: %w", err)
		}
		linkRows.Close()
	}
	return entries, nil
}

func claimVersions(ctx context.Context, pool *pgxpool.Pool, claimID string) ([]ContradictionVersionRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT cv.label, cv.statement, COALESCE(array_agg(DISTINCT cs.source_id)
		               FILTER (WHERE cs.source_id IS NOT NULL), '{}')
		FROM claim_versions cv
		LEFT JOIN claim_sources cs ON cs.version_id = cv.id
		WHERE cv.claim_id = $1
		GROUP BY cv.id, cv.label, cv.statement ORDER BY cv.label`, claimID)
	if err != nil {
		return nil, fmt.Errorf("validate: query claim versions: %w", err)
	}
	defer rows.Close()
	versions := []ContradictionVersionRow{}
	for rows.Next() {
		var version ContradictionVersionRow
		var sources []string
		if err := rows.Scan(&version.Label, &version.Statement, &sources); err != nil {
			return nil, fmt.Errorf("validate: scan claim version: %w", err)
		}
		sort.Strings(sources)
		version.Sources = sources
		versions = append(versions, version)
	}
	return versions, rows.Err()
}

// WriteModelOutput stores one raw model response verbatim with usage.
func WriteModelOutput(ctx context.Context, pool *pgxpool.Pool, runID int64, model, inputRef, rawOutput string, inputTokens, outputTokens int) error {
	payload, err := json.Marshal(map[string]any{"text": rawOutput})
	if err != nil {
		return fmt.Errorf("validate: marshal model output: %w", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO model_outputs (run_id, record_kind, prompt_version, model,
		                           input_ref, raw_output, parsed_ref,
		                           input_tokens, output_tokens)
		VALUES ($1, 'validation_verdict', $2, $3, $4, $5::jsonb, NULL, $6, $7)`,
		runID, PromptVersion, model, inputRef, string(payload), inputTokens, outputTokens); err != nil {
		return fmt.Errorf("validate: insert model output: %w", err)
	}
	return nil
}
