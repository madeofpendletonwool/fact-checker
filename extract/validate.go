// Deterministic validation: wire records -> loadable graph records.
//
// The engine's founding principle enforced mechanically: every claim and
// event must carry the citations the adapter's policy demands (the Arda
// both-tier rule: the reference document AND a primary candidate), locators
// must carry the keys the citation surface requires, and every
// cross-reference must resolve. Records that fail any rule are dropped and
// logged with a structured reason — never kept.

package extract

import "regexp"

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Confidence levels that require per-tradition versions and register a
// contradiction entry.
var contradictionConfidences = map[string]bool{
	ConfidenceContested:        true,
	ConfidenceAbandonedVersion: true,
}

// Allowed entity-name kinds (schema constraint variant|alias|translation|
// other). Surface spellings outside the set normalize to "other" and log a
// drop.
var nameKinds = map[string]bool{"variant": true, "alias": true, "translation": true, "other": true}

// Drop is one record-level rejection: what was dropped, where, and why.
type Drop struct {
	Kind   string `json:"kind"`
	Ref    string `json:"ref"`
	Reason string `json:"reason"`
}

// ContradictionEntry is the register row auto-created at extraction for a
// contested or abandoned-version claim. Resolution only ever happens by
// human review.
type ContradictionEntry struct {
	ID          string
	Title       string
	Description string
	ClaimID     string
}

// EntityName is one persisted name variant.
type EntityName struct {
	Name     string
	NameKind string
	Language string
}

// EntityRecord is a loadable entity.
type EntityRecord struct {
	ID            string
	Kind          string
	CanonicalName string
	Names         []EntityName
	Attributes    map[string]any
}

// Participant is one event participant with a role.
type Participant struct {
	EntityID string
	Role     string
}

// EventRecord is a loadable event.
type EventRecord struct {
	ID               string
	Kind             string
	Summary          string
	Description      string
	Dating           map[string]any
	LocationEntityID string
	Participants     []Participant
	Causes           []string
	Consequences     []string
	Sources          []Citation
}

// ClaimVersionRecord is one per-tradition version with pinned citations.
type ClaimVersionRecord struct {
	Label     string
	Statement string
	Sources   []Citation
}

// ClaimRecord is a loadable claim.
type ClaimRecord struct {
	ID              string
	Statement       string
	Confidence      string
	SubjectEntityID string
	Predicate       string
	ObjectEntityID  string
	ObjectValue     string
	Versions        []ClaimVersionRecord
	Sources         []Citation
}

// RelationshipRecord is a loadable graph edge.
type RelationshipRecord struct {
	FromEntityID string
	ToEntityID   string
	RelType      string
	ClaimID      string
}

// Validated is the outcome of validating one unit's wire payload.
type Validated struct {
	Entities       []EntityRecord
	Events         []EventRecord
	Claims         []ClaimRecord
	Relationships  []RelationshipRecord
	Contradictions []ContradictionEntry
	Drops          []Drop
}

// RecordCounts summarises kept records per kind.
func (v *Validated) RecordCounts() map[string]int {
	return map[string]int{
		"entities":       len(v.Entities),
		"events":         len(v.Events),
		"claims":         len(v.Claims),
		"relationships":  len(v.Relationships),
		"contradictions": len(v.Contradictions),
	}
}

// ValidationContext is everything the drop rules need beyond the payload:
// the unit (citation surface, aliases) and the graph ids that exist so far.
type ValidationContext struct {
	Unit        *Unit
	DBEntityIDs map[string]bool
	DBEventIDs  map[string]bool
	DBClaimIDs  map[string]bool
}

// citeable maps each allowed source id to the locator keys a citation
// against it must carry: the reference citation's own keys plus every
// candidate's locator keys.
func citeableSources(u *Unit) map[string][]string {
	allowed := map[string][]string{}
	add := func(sourceID string, locator map[string]any) {
		keys, ok := allowed[sourceID]
		if !ok {
			keys = []string{}
		}
		for k := range locator {
			exists := false
			for _, existing := range keys {
				if existing == k {
					exists = true
					break
				}
			}
			if !exists {
				keys = append(keys, k)
			}
		}
		allowed[sourceID] = keys
	}
	add(u.Reference.SourceID, u.Reference.Locator)
	for _, c := range u.Candidates {
		add(c.SourceID, c.Locator)
	}
	return allowed
}

// filterCitations validates each citation against the citeable surface,
// dropping invalid ones with a reason and returning the rest.
func filterCitations(citations []WireCitation, allowed map[string][]string, drops *[]Drop, owner string) []Citation {
	kept := []Citation{}
	for _, w := range citations {
		required, ok := allowed[w.SourceID]
		if !ok {
			*drops = append(*drops, Drop{"citation", owner, "unknown source '" + w.SourceID + "'"})
			continue
		}
		missing := []string{}
		for _, key := range required {
			if _, present := w.Locator[key]; !present {
				missing = append(missing, key)
			}
		}
		if len(missing) > 0 {
			*drops = append(*drops, Drop{
				"citation", owner,
				"bad_locator: locator for " + w.SourceID + " missing required keys " + joinQuoted(missing),
			})
			continue
		}
		kept = append(kept, Citation(w))
	}
	return kept
}

// citationReason checks the policy over the kept citations; nil means the
// citation contract is satisfied.
func citationReason(citations []Citation, policy CitationPolicy, referenceSourceID string) string {
	hasReference := false
	hasPrimary := false
	for _, c := range citations {
		if c.SourceID == referenceSourceID {
			hasReference = true
		} else {
			hasPrimary = true
		}
	}
	if len(citations) == 0 {
		return "uncited"
	}
	if policy.RequireReference && !hasReference {
		return "missing_reference_citation"
	}
	if policy.RequirePrimary && !hasPrimary {
		return "missing_primary_citation"
	}
	return ""
}

func joinQuoted(items []string) string {
	out := "["
	for i, item := range items {
		if i > 0 {
			out += " "
		}
		out += "'" + item + "'"
	}
	return out + "]"
}

func resolveEntity(id string, payload map[string]*EntityRecord, ctx *ValidationContext) bool {
	if id == "" {
		return true
	}
	if _, ok := payload[id]; ok {
		return true
	}
	return ctx.DBEntityIDs[id]
}

func mapEntities(wire *WireExtraction, result *Validated) map[string]*EntityRecord {
	byID := map[string]*EntityRecord{}
	for _, entity := range wire.Entities {
		if !slugRe.MatchString(entity.ID) {
			result.Drops = append(result.Drops, Drop{"entity", entity.ID, "invalid id (use lowercase slugs)"})
			continue
		}
		if _, dup := byID[entity.ID]; dup {
			result.Drops = append(result.Drops, Drop{"entity", entity.ID, "duplicate entity id"})
			continue
		}
		names := []EntityName{}
		seen := map[string]bool{}
		for _, name := range entity.Names {
			if name.Name == "" || seen[name.Name] || name.Name == entity.CanonicalName {
				continue
			}
			seen[name.Name] = true
			kind := name.NameKind
			if !nameKinds[kind] {
				result.Drops = append(result.Drops, Drop{"name_kind", entity.ID + "/" + name.Name, "'" + kind + "' -> other"})
				kind = "other"
			}
			names = append(names, EntityName{Name: name.Name, NameKind: kind, Language: name.Language})
		}
		record := &EntityRecord{
			ID:            entity.ID,
			Kind:          entity.Kind,
			CanonicalName: entity.CanonicalName,
			Names:         names,
			Attributes:    entity.Attributes,
		}
		byID[record.ID] = record
		result.Entities = append(result.Entities, *record)
	}
	return byID
}

func mapEvents(wire *WireExtraction, allowed map[string][]string, payloadEntities map[string]*EntityRecord, ctx *ValidationContext, result *Validated) {
	rawLinks := map[string][2][]string{}

	for _, event := range wire.Events {
		if !slugRe.MatchString(event.ID) {
			result.Drops = append(result.Drops, Drop{"event", event.ID, "invalid id (use lowercase slugs)"})
			continue
		}
		if _, dup := rawLinks[event.ID]; dup || ctx.DBEventIDs[event.ID] {
			result.Drops = append(result.Drops, Drop{"event", event.ID, "duplicate or already-known event id (merged)"})
			continue
		}
		citations := filterCitations(event.Sources, allowed, &result.Drops, "event:"+event.ID)
		if reason := citationReason(citations, ctx.Unit.Policy, ctx.Unit.Reference.SourceID); reason != "" {
			result.Drops = append(result.Drops, Drop{"event", event.ID, reason})
			continue
		}

		participants := []Participant{}
		for _, entityID := range sortedKeys(event.Participants) {
			role := event.Participants[entityID]
			if resolveEntity(entityID, payloadEntities, ctx) {
				participants = append(participants, Participant{EntityID: entityID, Role: role})
			} else {
				result.Drops = append(result.Drops, Drop{"participant", event.ID + "/" + entityID, "dangling entity ref"})
			}
		}

		location := event.LocationEntityID
		if location != "" && !resolveEntity(location, payloadEntities, ctx) {
			result.Drops = append(result.Drops, Drop{"location", event.ID, "dangling location ref '" + location + "'; dropped"})
			location = ""
		}

		rawLinks[event.ID] = [2][]string{append([]string(nil), event.Causes...), append([]string(nil), event.Consequences...)}
		result.Events = append(result.Events, EventRecord{
			ID:               event.ID,
			Kind:             event.Kind,
			Summary:          event.Summary,
			Description:      event.Description,
			Dating:           event.Dating,
			LocationEntityID: location,
			Participants:     participants,
			Causes:           []string{},
			Consequences:     []string{},
			Sources:          citations,
		})
	}

	// Resolve cause/consequence links only against events that survived
	// (plus already-known ones).
	kept := map[string]bool{}
	for id := range rawLinks {
		kept[id] = true
	}
	for id := range ctx.DBEventIDs {
		kept[id] = true
	}
	for i := range result.Events {
		record := &result.Events[i]
		causes, consequences := rawLinks[record.ID][0], rawLinks[record.ID][1]
		for _, cause := range causes {
			if kept[cause] {
				record.Causes = append(record.Causes, cause)
			} else {
				result.Drops = append(result.Drops, Drop{"event_link", record.ID + "->" + cause, "dangling cause ref; dropped"})
			}
		}
		for _, consequence := range consequences {
			if kept[consequence] {
				record.Consequences = append(record.Consequences, consequence)
			} else {
				result.Drops = append(result.Drops, Drop{"event_link", record.ID + "->" + consequence, "dangling consequence ref; dropped"})
			}
		}
	}
}

func mapClaims(wire *WireExtraction, allowed map[string][]string, payloadEntities map[string]*EntityRecord, ctx *ValidationContext, result *Validated) map[string]*ClaimRecord {
	byID := map[string]*ClaimRecord{}

	for _, claim := range wire.Claims {
		if !slugRe.MatchString(claim.ID) {
			result.Drops = append(result.Drops, Drop{"claim", claim.ID, "invalid id (use lowercase slugs)"})
			continue
		}
		if _, dup := byID[claim.ID]; dup || ctx.DBClaimIDs[claim.ID] {
			result.Drops = append(result.Drops, Drop{"claim", claim.ID, "duplicate or already-known claim id (merged)"})
			continue
		}

		objectValue := claim.ObjectValue
		if claim.ObjectEntityID != "" && objectValue != "" {
			result.Drops = append(result.Drops, Drop{"claim_field", claim.ID, "both object_entity_id and object_value; kept entity"})
			objectValue = ""
		}
		dangling := ""
		for _, ref := range []struct{ id, label string }{
			{claim.SubjectEntityID, "subject"},
			{claim.ObjectEntityID, "object"},
		} {
			if ref.id != "" && !resolveEntity(ref.id, payloadEntities, ctx) {
				dangling = "dangling " + ref.label + " entity ref '" + ref.id + "'"
				break
			}
		}
		if dangling != "" {
			result.Drops = append(result.Drops, Drop{"claim", claim.ID, dangling})
			continue
		}

		citations := filterCitations(claim.Sources, allowed, &result.Drops, "claim:"+claim.ID)
		reason := citationReason(citations, ctx.Unit.Policy, ctx.Unit.Reference.SourceID)
		versions := []ClaimVersionRecord{}
		if reason == "" {
			for _, version := range claim.Versions {
				vc := filterCitations(version.Sources, allowed, &result.Drops, "claim:"+claim.ID+"#"+version.Label)
				if vReason := citationReason(vc, ctx.Unit.Policy, ctx.Unit.Reference.SourceID); vReason != "" {
					result.Drops = append(result.Drops, Drop{"version", claim.ID + "#" + version.Label, vReason})
					continue
				}
				versions = append(versions, ClaimVersionRecord{Label: version.Label, Statement: version.Statement, Sources: vc})
			}
			if contradictionConfidences[claim.Confidence] && len(versions) == 0 {
				reason = "contested_without_versions"
			}
		}
		if reason != "" {
			result.Drops = append(result.Drops, Drop{"claim", claim.ID, reason})
			continue
		}

		record := &ClaimRecord{
			ID:              claim.ID,
			Statement:       claim.Statement,
			Confidence:      claim.Confidence,
			SubjectEntityID: claim.SubjectEntityID,
			Predicate:       claim.Predicate,
			ObjectEntityID:  claim.ObjectEntityID,
			ObjectValue:     objectValue,
			Versions:        versions,
			Sources:         citations,
		}
		byID[record.ID] = record
		result.Claims = append(result.Claims, *record)
		if contradictionConfidences[record.Confidence] {
			title := record.Statement
			if len(title) > 120 {
				title = title[:120]
			}
			result.Contradictions = append(result.Contradictions, ContradictionEntry{
				ID:          record.ID + "-tradition",
				Title:       title,
				Description: "Auto-registered at extraction: traditions diverge on this claim; per-tradition versions preserved, never auto-resolved.",
				ClaimID:     record.ID,
			})
		}
	}
	return byID
}

func mapRelationships(wire *WireExtraction, payloadEntities map[string]*EntityRecord, payloadClaims map[string]*ClaimRecord, ctx *ValidationContext, result *Validated) {
	seen := map[string]bool{}
	for _, rel := range wire.Relationships {
		ref := rel.FromEntityID + "-" + rel.RelType + "-" + rel.ToEntityID
		// One edge, several spellings: authored aliases normalize, and
		// anything else still fails closed.
		relType := rel.RelType
		if canonical, ok := ctx.Unit.RelAliases[relType]; ok {
			relType = canonical
		}
		if !containsString(ctx.Unit.RelTypes, relType) {
			result.Drops = append(result.Drops, Drop{"relationship", ref, "unknown rel_type '" + rel.RelType + "'"})
			continue
		}
		if !resolveEntity(rel.FromEntityID, payloadEntities, ctx) || !resolveEntity(rel.ToEntityID, payloadEntities, ctx) {
			result.Drops = append(result.Drops, Drop{"relationship", ref, "dangling endpoint"})
			continue
		}
		if rel.FromEntityID == rel.ToEntityID {
			result.Drops = append(result.Drops, Drop{"relationship", ref, "self-relationship"})
			continue
		}
		claimID := rel.ClaimID
		if claimID != "" {
			if _, inPayload := payloadClaims[claimID]; !inPayload && !ctx.DBClaimIDs[claimID] {
				result.Drops = append(result.Drops, Drop{"relationship", ref, "dangling claim_id '" + claimID + "'; kept without justification"})
				claimID = ""
			}
		}
		key := rel.FromEntityID + "\x00" + rel.ToEntityID + "\x00" + relType
		if seen[key] {
			result.Drops = append(result.Drops, Drop{"relationship", ref, "duplicate relationship"})
			continue
		}
		seen[key] = true
		result.Relationships = append(result.Relationships, RelationshipRecord{
			FromEntityID: rel.FromEntityID,
			ToEntityID:   rel.ToEntityID,
			RelType:      relType,
			ClaimID:      claimID,
		})
	}
}

// Validate applies every drop rule and returns the loadable records plus
// the drop log.
func Validate(wire *WireExtraction, ctx *ValidationContext) *Validated {
	result := &Validated{}
	allowed := citeableSources(ctx.Unit)
	payloadEntities := mapEntities(wire, result)
	mapEvents(wire, allowed, payloadEntities, ctx, result)
	payloadClaims := mapClaims(wire, allowed, payloadEntities, ctx, result)
	mapRelationships(wire, payloadEntities, payloadClaims, ctx, result)
	return result
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]string) []string {
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
