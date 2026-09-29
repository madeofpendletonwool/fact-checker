// Graph-state loading for the validation engine.
//
// Pulls the claims graph into small typed snapshots so the deterministic
// checks run over plain structs (testable without a database) while
// staying a faithful view of what is actually stored.

package validate

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// GraphCitation is one claim citation joined with its source registry row.
type GraphCitation struct {
	SourceID     string
	Tier         int
	CitationForm string
	Locator      map[string]any
	Note         string
	// VersionID is set (non-zero) when the citation backs one specific
	// version of a contested claim.
	VersionID int64
}

// GraphVersion is one per-tradition version row of a claim.
type GraphVersion struct {
	RowID     int64
	Label     string
	Statement string
}

// GraphClaim is one claims row with its citations and versions.
type GraphClaim struct {
	ID              string
	Statement       string
	Confidence      string
	SubjectEntityID string
	Predicate       string
	ObjectEntityID  string
	ObjectValue     string
	Sources         []GraphCitation
	Versions        []GraphVersion
}

// PrimaryCitations returns the credible-source citations (tiers 1–2) —
// the engine-fixed credibility floor; adapters with finer tier semantics
// register their own checks on top.
func (c *GraphClaim) PrimaryCitations() []GraphCitation {
	primary := []GraphCitation{}
	for _, citation := range c.Sources {
		if citation.Tier == 1 || citation.Tier == 2 {
			primary = append(primary, citation)
		}
	}
	return primary
}

// BestPrimary is the strongest primary citation (lowest tier, then
// source id) — the tradition label for version pinning.
func (c *GraphClaim) BestPrimary() *GraphCitation {
	var best *GraphCitation
	for i := range c.Sources {
		citation := &c.Sources[i]
		if citation.Tier != 1 && citation.Tier != 2 {
			continue
		}
		if best == nil || citation.Tier < best.Tier ||
			(citation.Tier == best.Tier && citation.SourceID < best.SourceID) {
			best = citation
		}
	}
	return best
}

// GraphEntity is one entities row with its name variants.
type GraphEntity struct {
	ID            string
	Kind          string
	CanonicalName string
	Names         []string
}

// GraphEvent is one events row. Dating is adapter-shaped data in
// attributes.dating; the engine's generic checks do not interpret it
// (domain dating invariants are adapter checks).
type GraphEvent struct {
	ID               string
	Kind             string
	Summary          string
	LocationEntityID string
	Participants     []string
}

// GraphRelationship is one relationships row joined with its type's
// category (the adapter vocabulary the edge belongs to).
type GraphRelationship struct {
	FromEntityID string
	ToEntityID   string
	RelType      string
	Category     string
}

// GraphState is the full snapshot the deterministic engine checks.
type GraphState struct {
	Entities      []GraphEntity
	Claims        []GraphClaim
	Events        []GraphEvent
	Relationships []GraphRelationship
	// ContradictionLinks maps register entry id -> claim ids currently
	// linked, so conflict detection can reuse entries instead of
	// duplicating them.
	ContradictionLinks map[string]map[string]bool
}

// ClaimByID indexes the snapshot's claims.
func (s *GraphState) ClaimByID() map[string]*GraphClaim {
	byID := make(map[string]*GraphClaim, len(s.Claims))
	for i := range s.Claims {
		byID[s.Claims[i].ID] = &s.Claims[i]
	}
	return byID
}

// LoadGraphState snapshots the graph tables the engine checks.
func LoadGraphState(ctx context.Context, pool *pgxpool.Pool) (*GraphState, error) {
	state := &GraphState{ContradictionLinks: map[string]map[string]bool{}}

	entityRows, err := pool.Query(ctx, `
		SELECT id, kind, canonical_name FROM entities ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("validate: query entities: %w", err)
	}
	defer entityRows.Close()
	entitiesByID := map[string]*GraphEntity{}
	for entityRows.Next() {
		var entity GraphEntity
		if err := entityRows.Scan(&entity.ID, &entity.Kind, &entity.CanonicalName); err != nil {
			return nil, fmt.Errorf("validate: scan entity: %w", err)
		}
		state.Entities = append(state.Entities, entity)
	}
	if err := entityRows.Err(); err != nil {
		return nil, fmt.Errorf("validate: entities: %w", err)
	}
	for i := range state.Entities {
		entitiesByID[state.Entities[i].ID] = &state.Entities[i]
	}
	nameRows, err := pool.Query(ctx, `
		SELECT entity_id, name FROM entity_names ORDER BY entity_id, name`)
	if err != nil {
		return nil, fmt.Errorf("validate: query entity names: %w", err)
	}
	defer nameRows.Close()
	for nameRows.Next() {
		var entityID, name string
		if err := nameRows.Scan(&entityID, &name); err != nil {
			return nil, fmt.Errorf("validate: scan entity name: %w", err)
		}
		if entity, ok := entitiesByID[entityID]; ok {
			entity.Names = append(entity.Names, name)
		}
	}
	if err := nameRows.Err(); err != nil {
		return nil, fmt.Errorf("validate: entity names: %w", err)
	}

	claimRows, err := pool.Query(ctx, `
		SELECT c.id, c.statement, c.confidence, c.subject_entity_id,
		       c.predicate, c.object_entity_id, c.object_value
		FROM claims c ORDER BY c.id`)
	if err != nil {
		return nil, fmt.Errorf("validate: query claims: %w", err)
	}
	defer claimRows.Close()
	for claimRows.Next() {
		var claim GraphClaim
		var subject, predicate, objectEntity, objectValue *string
		if err := claimRows.Scan(&claim.ID, &claim.Statement, &claim.Confidence,
			&subject, &predicate, &objectEntity, &objectValue); err != nil {
			return nil, fmt.Errorf("validate: scan claim: %w", err)
		}
		claim.SubjectEntityID = deref(subject)
		claim.Predicate = deref(predicate)
		claim.ObjectEntityID = deref(objectEntity)
		claim.ObjectValue = deref(objectValue)
		state.Claims = append(state.Claims, claim)
	}
	if err := claimRows.Err(); err != nil {
		return nil, fmt.Errorf("validate: claims: %w", err)
	}
	claimsByID := state.ClaimByID()

	citationRows, err := pool.Query(ctx, `
		SELECT cs.claim_id, cs.version_id, cs.source_id, s.tier,
		       s.citation_form, cs.locator, cs.note
		FROM claim_sources cs JOIN sources s ON s.id = cs.source_id
		ORDER BY cs.claim_id, s.tier, cs.source_id`)
	if err != nil {
		return nil, fmt.Errorf("validate: query claim citations: %w", err)
	}
	defer citationRows.Close()
	for citationRows.Next() {
		var claimID, sourceID string
		var tier int
		var versionID *int64
		var citationForm, note *string
		var locator []byte
		if err := citationRows.Scan(&claimID, &versionID, &sourceID, &tier,
			&citationForm, &locator, &note); err != nil {
			return nil, fmt.Errorf("validate: scan claim citation: %w", err)
		}
		claim := claimsByID[claimID]
		if claim == nil {
			continue
		}
		citation := GraphCitation{
			SourceID:     sourceID,
			Tier:         tier,
			CitationForm: deref(citationForm),
			Note:         deref(note),
		}
		if versionID != nil {
			citation.VersionID = *versionID
		}
		if len(locator) > 0 {
			decoded, err := decodeJSONObject(locator)
			if err != nil {
				return nil, fmt.Errorf("validate: claim %s citation locator: %w", claimID, err)
			}
			citation.Locator = decoded
		}
		claim.Sources = append(claim.Sources, citation)
	}
	if err := citationRows.Err(); err != nil {
		return nil, fmt.Errorf("validate: claim citations: %w", err)
	}

	versionRows, err := pool.Query(ctx, `
		SELECT id, claim_id, label, statement FROM claim_versions
		ORDER BY claim_id, label`)
	if err != nil {
		return nil, fmt.Errorf("validate: query claim versions: %w", err)
	}
	defer versionRows.Close()
	for versionRows.Next() {
		var rowID int64
		var claimID, label, statement string
		if err := versionRows.Scan(&rowID, &claimID, &label, &statement); err != nil {
			return nil, fmt.Errorf("validate: scan claim version: %w", err)
		}
		if claim := claimsByID[claimID]; claim != nil {
			claim.Versions = append(claim.Versions, GraphVersion{RowID: rowID, Label: label, Statement: statement})
		}
	}
	if err := versionRows.Err(); err != nil {
		return nil, fmt.Errorf("validate: claim versions: %w", err)
	}

	eventRows, err := pool.Query(ctx, `
		SELECT id, kind, summary, location_entity_id FROM events ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("validate: query events: %w", err)
	}
	defer eventRows.Close()
	for eventRows.Next() {
		var event GraphEvent
		var kind, location *string
		if err := eventRows.Scan(&event.ID, &kind, &event.Summary, &location); err != nil {
			return nil, fmt.Errorf("validate: scan event: %w", err)
		}
		event.Kind = deref(kind)
		event.LocationEntityID = deref(location)
		state.Events = append(state.Events, event)
	}
	if err := eventRows.Err(); err != nil {
		return nil, fmt.Errorf("validate: events: %w", err)
	}

	participantRows, err := pool.Query(ctx, `
		SELECT event_id, entity_id FROM event_participants
		ORDER BY event_id, entity_id`)
	if err != nil {
		return nil, fmt.Errorf("validate: query event participants: %w", err)
	}
	defer participantRows.Close()
	eventsByID := map[string]*GraphEvent{}
	for i := range state.Events {
		eventsByID[state.Events[i].ID] = &state.Events[i]
	}
	for participantRows.Next() {
		var eventID, entityID string
		if err := participantRows.Scan(&eventID, &entityID); err != nil {
			return nil, fmt.Errorf("validate: scan event participant: %w", err)
		}
		if event := eventsByID[eventID]; event != nil {
			event.Participants = append(event.Participants, entityID)
		}
	}
	if err := participantRows.Err(); err != nil {
		return nil, fmt.Errorf("validate: event participants: %w", err)
	}

	relationshipRows, err := pool.Query(ctx, `
		SELECT r.from_entity_id, r.to_entity_id, r.rel_type, rt.category
		FROM relationships r
		JOIN relationship_types rt ON rt.rel_type = r.rel_type
		ORDER BY r.from_entity_id, r.to_entity_id, r.rel_type`)
	if err != nil {
		return nil, fmt.Errorf("validate: query relationships: %w", err)
	}
	defer relationshipRows.Close()
	for relationshipRows.Next() {
		var relationship GraphRelationship
		if err := relationshipRows.Scan(&relationship.FromEntityID, &relationship.ToEntityID,
			&relationship.RelType, &relationship.Category); err != nil {
			return nil, fmt.Errorf("validate: scan relationship: %w", err)
		}
		state.Relationships = append(state.Relationships, relationship)
	}
	if err := relationshipRows.Err(); err != nil {
		return nil, fmt.Errorf("validate: relationships: %w", err)
	}

	linkRows, err := pool.Query(ctx, `
		SELECT contradiction_id, claim_id FROM contradiction_claims`)
	if err != nil {
		return nil, fmt.Errorf("validate: query contradiction links: %w", err)
	}
	defer linkRows.Close()
	for linkRows.Next() {
		var contradictionID, claimID string
		if err := linkRows.Scan(&contradictionID, &claimID); err != nil {
			return nil, fmt.Errorf("validate: scan contradiction link: %w", err)
		}
		if state.ContradictionLinks[contradictionID] == nil {
			state.ContradictionLinks[contradictionID] = map[string]bool{}
		}
		state.ContradictionLinks[contradictionID][claimID] = true
	}
	if err := linkRows.Err(); err != nil {
		return nil, fmt.Errorf("validate: contradiction links: %w", err)
	}

	return state, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// decodeJSONObject parses a JSONB column value into a string-keyed map.
func decodeJSONObject(raw []byte) (map[string]any, error) {
	decoded := map[string]any{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}
