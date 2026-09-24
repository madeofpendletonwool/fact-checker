// Persistence of validated extraction records: idempotent upserts inside
// the caller's per-unit transaction. All inserts are ON CONFLICT DO
// NOTHING — first write wins — so re-runs and cross-document collisions
// merge instead of failing.

package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// LedgerRow is the resume state for one unit under one prompt version.
type LedgerRow struct {
	Status    string
	InputHash string
}

// LoadRecords writes one unit's validated records. The caller owns the
// transaction: either the whole unit lands or none of it does.
func LoadRecords(ctx context.Context, tx pgx.Tx, records *Validated) error {
	for _, entity := range records.Entities {
		attributes, err := marshalJSON(entity.Attributes)
		if err != nil {
			return fmt.Errorf("entity %s attributes: %w", entity.ID, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO entities (id, kind, canonical_name, attributes)
			VALUES ($1, $2, $3, $4::jsonb)
			ON CONFLICT (id) DO NOTHING`,
			entity.ID, entity.Kind, entity.CanonicalName, attributes); err != nil {
			return fmt.Errorf("insert entity %s: %w", entity.ID, err)
		}
		for _, name := range entity.Names {
			var language *string
			if name.Language != "" {
				language = &name.Language
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO entity_names (entity_id, name, name_kind, language)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (entity_id, name) DO NOTHING`,
				entity.ID, name.Name, name.NameKind, language); err != nil {
				return fmt.Errorf("insert entity name %s/%s: %w", entity.ID, name.Name, err)
			}
		}
	}

	// Two passes: link/participant targets must exist as rows before the
	// references to them are written (causes may point forward).
	for _, event := range records.Events {
		attributes := map[string]any{}
		if event.Dating != nil {
			attributes["dating"] = event.Dating
		}
		attributesJSON, err := marshalJSON(attributes)
		if err != nil {
			return fmt.Errorf("event %s attributes: %w", event.ID, err)
		}
		var kind *string
		if event.Kind != "" {
			kind = &event.Kind
		}
		var description *string
		if event.Description != "" {
			description = &event.Description
		}
		var location *string
		if event.LocationEntityID != "" {
			location = &event.LocationEntityID
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO events (id, kind, summary, description, location_entity_id, attributes)
			VALUES ($1, $2, $3, $4, $5, $6::jsonb)
			ON CONFLICT (id) DO NOTHING`,
			event.ID, kind, event.Summary, description, location, attributesJSON); err != nil {
			return fmt.Errorf("insert event %s: %w", event.ID, err)
		}
	}

	for _, event := range records.Events {
		for _, participant := range event.Participants {
			if _, err := tx.Exec(ctx, `
				INSERT INTO event_participants (event_id, entity_id, role)
				VALUES ($1, $2, $3)
				ON CONFLICT (event_id, entity_id) DO NOTHING`,
				event.ID, participant.EntityID, participant.Role); err != nil {
				return fmt.Errorf("insert participant %s/%s: %w", event.ID, participant.EntityID, err)
			}
		}
		for _, link := range event.Causes {
			if _, err := tx.Exec(ctx, `
				INSERT INTO event_links (from_event_id, to_event_id, link_kind)
				VALUES ($1, $2, 'cause')
				ON CONFLICT (from_event_id, to_event_id, link_kind) DO NOTHING`,
				link, event.ID); err != nil {
				return fmt.Errorf("insert event link %s->%s: %w", link, event.ID, err)
			}
		}
		for _, consequence := range event.Consequences {
			if _, err := tx.Exec(ctx, `
				INSERT INTO event_links (from_event_id, to_event_id, link_kind)
				VALUES ($1, $2, 'cause')
				ON CONFLICT (from_event_id, to_event_id, link_kind) DO NOTHING`,
				event.ID, consequence); err != nil {
				return fmt.Errorf("insert event link %s->%s: %w", event.ID, consequence, err)
			}
		}
		if err := insertEventCitations(ctx, tx, event.ID, event.Sources); err != nil {
			return err
		}
	}

	for _, claim := range records.Claims {
		notes := map[string]any{"extracted_by": "extract:" + PromptVersion}
		notesJSON, err := marshalJSON(notes)
		if err != nil {
			return fmt.Errorf("claim %s notes: %w", claim.ID, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO claims (id, statement, subject_entity_id, predicate,
			                    object_entity_id, object_value, confidence, notes)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb)
			ON CONFLICT (id) DO NOTHING`,
			claim.ID, claim.Statement,
			nullable(claim.SubjectEntityID), nullable(claim.Predicate),
			nullable(claim.ObjectEntityID), nullable(claim.ObjectValue),
			claim.Confidence, notesJSON); err != nil {
			return fmt.Errorf("insert claim %s: %w", claim.ID, err)
		}
		versionIDs := map[string]int64{}
		for _, version := range claim.Versions {
			var versionID int64
			if err := tx.QueryRow(ctx, `
				INSERT INTO claim_versions (claim_id, label, statement)
				VALUES ($1, $2, $3)
				ON CONFLICT (claim_id, label) DO UPDATE SET statement = EXCLUDED.statement
				RETURNING id`,
				claim.ID, version.Label, version.Statement).Scan(&versionID); err != nil {
				return fmt.Errorf("insert claim version %s#%s: %w", claim.ID, version.Label, err)
			}
			versionIDs[version.Label] = versionID
		}
		if err := insertClaimCitations(ctx, tx, claim.ID, nil, claim.Sources); err != nil {
			return err
		}
		for _, version := range claim.Versions {
			id := versionIDs[version.Label]
			if err := insertClaimCitations(ctx, tx, claim.ID, &id, version.Sources); err != nil {
				return err
			}
		}
	}

	for _, rel := range records.Relationships {
		if _, err := tx.Exec(ctx, `
			INSERT INTO relationships (from_entity_id, to_entity_id, rel_type, claim_id)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (from_entity_id, to_entity_id, rel_type) DO NOTHING`,
			rel.FromEntityID, rel.ToEntityID, rel.RelType, nullable(rel.ClaimID)); err != nil {
			return fmt.Errorf("insert relationship %s-%s-%s: %w", rel.FromEntityID, rel.RelType, rel.ToEntityID, err)
		}
	}

	for _, entry := range records.Contradictions {
		if _, err := tx.Exec(ctx, `
			INSERT INTO contradictions (id, title, description)
			VALUES ($1, $2, $3)
			ON CONFLICT (id) DO NOTHING`,
			entry.ID, entry.Title, entry.Description); err != nil {
			return fmt.Errorf("insert contradiction %s: %w", entry.ID, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO contradiction_claims (contradiction_id, claim_id, version_id, tradition_label)
			VALUES ($1, $2, NULL, $3)
			ON CONFLICT (contradiction_id, claim_id, version_id) DO NOTHING`,
			entry.ID, entry.ClaimID, "auto-registered at extraction"); err != nil {
			return fmt.Errorf("insert contradiction claim %s/%s: %w", entry.ID, entry.ClaimID, err)
		}
	}
	return nil
}

// insertEventCitations writes event citations.
func insertEventCitations(ctx context.Context, tx pgx.Tx, eventID string, citations []Citation) error {
	for _, citation := range citations {
		locator, err := marshalJSON(citation.Locator)
		if err != nil {
			return fmt.Errorf("citation locator for %s->%s: %w", eventID, citation.SourceID, err)
		}
		var note *string
		if citation.Note != "" {
			note = &citation.Note
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO event_sources (event_id, source_id, locator, note)
			VALUES ($1, $2, $3::jsonb, $4)
			ON CONFLICT (event_id, source_id) DO NOTHING`,
			eventID, citation.SourceID, locator, note); err != nil {
			return fmt.Errorf("insert event citation %s->%s: %w", eventID, citation.SourceID, err)
		}
	}
	return nil
}

// insertClaimCitations writes claim citations. versionID nil means the
// citation supports the claim as a whole.
func insertClaimCitations(ctx context.Context, tx pgx.Tx, recordID string, versionID *int64, citations []Citation) error {
	var version any
	if versionID != nil {
		version = *versionID
	}
	for _, citation := range citations {
		locator, err := marshalJSON(citation.Locator)
		if err != nil {
			return fmt.Errorf("citation locator for %s->%s: %w", recordID, citation.SourceID, err)
		}
		var note *string
		if citation.Note != "" {
			note = &citation.Note
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO claim_sources (claim_id, version_id, source_id, locator, note)
			VALUES ($1, $2, $3, $4::jsonb, $5)
			ON CONFLICT (claim_id, version_id, source_id) DO NOTHING`,
			recordID, version, citation.SourceID, locator, note); err != nil {
			return fmt.Errorf("insert claim citation %s->%s: %w", recordID, citation.SourceID, err)
		}
	}
	return nil
}

// WriteModelOutput stores one raw model response verbatim with its usage.
func WriteModelOutput(ctx context.Context, tx pgx.Tx, runID int64, unit *Unit, model, text string, inputTokens, outputTokens int) error {
	raw, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return fmt.Errorf("marshal model output: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO model_outputs (run_id, record_kind, prompt_version, model,
		                           input_ref, raw_output, parsed_ref,
		                           input_tokens, output_tokens)
		VALUES ($1, 'extraction', $2, $3, $4, $5::jsonb, NULL, $6, $7)`,
		runID, PromptVersion, model, fmt.Sprintf("raw_documents:%d", unit.DocumentID),
		string(raw), inputTokens, outputTokens)
	if err != nil {
		return fmt.Errorf("insert model output: %w", err)
	}
	return nil
}

// UpsertLedgerRow marks a unit pending under the current prompt version
// before its model call is submitted.
func UpsertLedgerRow(ctx context.Context, tx pgx.Tx, unit *Unit, model string, inputHash string, runID int64) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO extraction_units (raw_document_id, unit_key, prompt_version, model,
		                              status, input_hash, run_id)
		VALUES ($1, $2, $3, $4, 'pending', $5, $6)
		ON CONFLICT (unit_key, prompt_version) DO UPDATE SET
		    model       = EXCLUDED.model,
		    status      = 'pending',
		    input_hash  = EXCLUDED.input_hash,
		    run_id      = EXCLUDED.run_id,
		    last_error = NULL`,
		unit.DocumentID, unit.UnitKey, PromptVersion, model, inputHash, runID)
	if err != nil {
		return fmt.Errorf("upsert ledger row %s: %w", unit.UnitKey, err)
	}
	return nil
}

// FinishLedgerRow completes a unit's ledger row with its outcome.
func FinishLedgerRow(ctx context.Context, tx pgx.Tx, unit *Unit, status string, recordCounts map[string]int, dropped []Drop, lastError string) error {
	recordsJSON, err := marshalJSON(recordCounts)
	if err != nil {
		return fmt.Errorf("marshal records: %w", err)
	}
	droppedJSON, err := marshalJSON(dropped)
	if err != nil {
		return fmt.Errorf("marshal drops: %w", err)
	}
	var errorPtr *string
	if lastError != "" {
		errorPtr = &lastError
	}
	_, err = tx.Exec(ctx, `
		UPDATE extraction_units SET
		    status       = $1,
		    attempts     = extraction_units.attempts + 1,
		    last_error   = $2,
		    records      = $3::jsonb,
		    dropped      = $4::jsonb,
		    completed_at = $5
		WHERE unit_key = $6 AND prompt_version = $7`,
		status, errorPtr, recordsJSON, droppedJSON, time.Now().UTC(), unit.UnitKey, PromptVersion)
	if err != nil {
		return fmt.Errorf("finish ledger row %s: %w", unit.UnitKey, err)
	}
	return nil
}

// Querier is the read surface the ledger helpers need; satisfied by a
// connection pool.
type Querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// LedgerState returns unit_key -> resume state for one prompt version.
func LedgerState(ctx context.Context, q Querier) (map[string]LedgerRow, error) {
	rows, err := q.Query(ctx, `
		SELECT unit_key, status, input_hash FROM extraction_units
		WHERE prompt_version = $1`, PromptVersion)
	if err != nil {
		return nil, fmt.Errorf("query ledger: %w", err)
	}
	defer rows.Close()

	state := map[string]LedgerRow{}
	for rows.Next() {
		var key string
		var row LedgerRow
		if err := rows.Scan(&key, &row.Status, &row.InputHash); err != nil {
			return nil, fmt.Errorf("scan ledger row: %w", err)
		}
		state[key] = row
	}
	return state, rows.Err()
}

// IDSnapshots loads the graph id sets validation resolves references
// against.
func IDSnapshots(ctx context.Context, pool Querier) (entities, events, claims map[string]bool, err error) {
	entities, err = idSet(ctx, pool, "entities")
	if err != nil {
		return nil, nil, nil, err
	}
	events, err = idSet(ctx, pool, "events")
	if err != nil {
		return nil, nil, nil, err
	}
	claims, err = idSet(ctx, pool, "claims")
	if err != nil {
		return nil, nil, nil, err
	}
	return entities, events, claims, nil
}

func idSet(ctx context.Context, pool Querier, table string) (map[string]bool, error) {
	rows, err := pool.Query(ctx, "SELECT id FROM "+table)
	if err != nil {
		return nil, fmt.Errorf("query %s ids: %w", table, err)
	}
	defer rows.Close()

	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan %s id: %w", table, err)
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

func marshalJSON(v any) ([]byte, error) {
	if v == nil {
		return []byte("null"), nil
	}
	return json.Marshal(v)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
