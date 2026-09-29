// Deterministic consistency engine: catches what the models miss.
//
// Pure functions over a GraphState snapshot — no database, no network —
// so every rule is unit-testable in isolation. Findings are Finding
// records (severity error / warning / review); machine actions are
// limited by policy to confidence DOWNgrades and contradiction
// registration. Nothing here ever deletes a record or upgrades a
// confidence: contradictions are preserved and flagged, never smoothed
// away.
//
// The check set is a registry: the engine ships the generic core
// (CoreChecks) and adapters append their own domain invariants — the
// reference implementation's dating bounds and genealogy cycles are the
// worked example of what an adapter adds.

package validate

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Finding severities, in weight order.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
	SeverityReview  = "review"
)

// Confidence levels, engine-fixed. May only move DOWN this order by
// machine action; abandoned_version is a statement about strata, not
// strength, and is never a machine target.
const (
	ConfidenceEstablished      = "established"
	ConfidenceDerivedProbable  = "derived_probable"
	ConfidenceContested        = "contested"
	ConfidenceAbandonedVersion = "abandoned_version"
)

// ConfidenceOrder ranks the levels downward.
var ConfidenceOrder = map[string]int{
	ConfidenceEstablished:      0,
	ConfidenceDerivedProbable:  1,
	ConfidenceContested:        2,
	ConfidenceAbandonedVersion: 3,
}

// Finding is one deterministic-check discovery. Keyed by
// (CheckCode, RecordKind, RecordID) in the flags table.
type Finding struct {
	CheckCode  string
	RecordKind string // claim | event | entity | relationship | adapter kinds
	RecordID   string
	Severity   string
	Message    string
	Details    map[string]any
}

// Downgrade is a proposed confidence move. Machine passes may only move
// confidence strictly downward.
type Downgrade struct {
	ClaimID   string
	Current   string
	Target    string
	CheckCode string
}

// Downward reports whether the target is strictly below the current level.
func (d Downgrade) Downward() bool {
	return ConfidenceOrder[d.Target] > ConfidenceOrder[d.Current]
}

// RegistrationClaim is one claim tradition entering the contradiction
// register.
type RegistrationClaim struct {
	ClaimID      string
	VersionLabel string
	Statement    string
}

// Registration is one contradiction-register entry the engine wants
// recorded: every version preserved, never resolved to a winner.
type Registration struct {
	ID          string
	Title       string
	Description string
	Claims      []RegistrationClaim
	// Reuse marks an existing register entry that already links ≥2 of
	// these claims (created stays false; only the links refresh).
	Reuse bool
}

// Report is everything one engine pass produced.
type Report struct {
	Flags         []Finding
	Downgrades    []Downgrade
	Registrations []Registration
}

// FlagCounts tallies findings per check code.
func (r *Report) FlagCounts() map[string]int {
	counts := map[string]int{}
	for _, flag := range r.Flags {
		counts[flag.CheckCode]++
	}
	return counts
}

// SeverityCounts tallies findings per severity.
func (r *Report) SeverityCounts() map[string]int {
	counts := map[string]int{SeverityError: 0, SeverityWarning: 0, SeverityReview: 0}
	for _, flag := range r.Flags {
		counts[flag.Severity]++
	}
	return counts
}

// Check is one registered deterministic check: a pure function over the
// snapshot appending findings, downgrades, and registrations to the
// report. Adapters implement checks over the same GraphState the core
// sees (their domain data arrives as entity/event/relationship
// attributes, which adapter checks interpret).
type Check func(state *GraphState, report *Report)

// CoreChecks is the generic check set shipped in the engine core;
// adapters append their own domain invariants after these.
func CoreChecks() []Check {
	return []Check{
		CheckClaimShape,
		CheckDanglingReferences,
		CheckDuplicateClaims,
		CheckPredicateConflicts,
		CheckConfidenceRules,
	}
}

// RunEngine executes the core check set plus any adapter-registered
// checks over the snapshot.
func RunEngine(state *GraphState, adapterChecks ...Check) *Report {
	report := &Report{}
	for _, check := range CoreChecks() {
		check(state, report)
	}
	for _, check := range adapterChecks {
		check(state, report)
	}
	return report
}

// objectKey normalizes a claim's object for equality comparison.
func objectKey(claim *GraphClaim) string {
	if claim.ObjectEntityID != "" {
		return "entity:" + claim.ObjectEntityID
	}
	if claim.ObjectValue != "" {
		return "value:" + normalize(claim.ObjectValue)
	}
	return "none"
}

// CheckClaimShape flags claims with no predicate and no object — not
// machine-checkable.
func CheckClaimShape(state *GraphState, report *Report) {
	for i := range state.Claims {
		claim := &state.Claims[i]
		hasStructure := claim.Predicate != "" || claim.ObjectEntityID != "" || claim.ObjectValue != ""
		if !hasStructure {
			report.Flags = append(report.Flags, Finding{
				CheckCode:  "claim_shape_invalid",
				RecordKind: "claim",
				RecordID:   claim.ID,
				Severity:   SeverityReview,
				Message:    "claim has no predicate and no object; not machine-checkable",
				Details:    map[string]any{"statement": claim.Statement},
			})
		}
	}
}

// CheckDanglingReferences flags event participants, event locations, and
// claim endpoints that do not resolve to an entity.
func CheckDanglingReferences(state *GraphState, report *Report) {
	entityIDs := map[string]bool{}
	for _, entity := range state.Entities {
		entityIDs[entity.ID] = true
	}

	for _, event := range state.Events {
		for _, participantID := range event.Participants {
			if !entityIDs[participantID] {
				report.Flags = append(report.Flags, Finding{
					CheckCode:  "dangling_reference",
					RecordKind: "event",
					RecordID:   event.ID + "/" + participantID,
					Severity:   SeverityError,
					Message:    fmt.Sprintf("participant %q does not exist", participantID),
				})
			}
		}
		if event.LocationEntityID != "" && !entityIDs[event.LocationEntityID] {
			report.Flags = append(report.Flags, Finding{
				CheckCode:  "dangling_reference",
				RecordKind: "event",
				RecordID:   event.ID,
				Severity:   SeverityError,
				Message:    fmt.Sprintf("location %q does not exist", event.LocationEntityID),
			})
		}
	}
	for i := range state.Claims {
		claim := &state.Claims[i]
		endpoints := []struct{ label, id string }{
			{"subject", claim.SubjectEntityID},
			{"object", claim.ObjectEntityID},
		}
		for _, endpoint := range endpoints {
			if endpoint.id != "" && !entityIDs[endpoint.id] {
				report.Flags = append(report.Flags, Finding{
					CheckCode:  "dangling_reference",
					RecordKind: "claim",
					RecordID:   claim.ID + "/" + endpoint.label,
					Severity:   SeverityError,
					Message:    fmt.Sprintf("%s entity %q does not exist", endpoint.label, endpoint.id),
				})
			}
		}
	}
}

// CheckDuplicateClaims flags the same subject/predicate/object carried
// by two claim ids — a merge candidate.
func CheckDuplicateClaims(state *GraphState, report *Report) {
	type triple struct{ subject, predicate, object string }
	groups := map[triple][]string{}
	for i := range state.Claims {
		claim := &state.Claims[i]
		key := triple{claim.SubjectEntityID, claim.Predicate, objectKey(claim)}
		groups[key] = append(groups[key], claim.ID)
	}

	keys := make([]triple, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(a, b int) bool {
		if keys[a].subject != keys[b].subject {
			return keys[a].subject < keys[b].subject
		}
		if keys[a].predicate != keys[b].predicate {
			return keys[a].predicate < keys[b].predicate
		}
		return groups[keys[a]][0] < groups[keys[b]][0]
	})

	for _, key := range keys {
		claimIDs := groups[key]
		if len(claimIDs) < 2 {
			continue
		}
		sort.Strings(claimIDs)
		subject := key.subject
		if subject == "" {
			subject = "—"
		}
		predicate := key.predicate
		if predicate == "" {
			predicate = "—"
		}
		report.Flags = append(report.Flags, Finding{
			CheckCode:  "duplicate_claim",
			RecordKind: "claim",
			RecordID:   claimIDs[0],
			Severity:   SeverityReview,
			Message: fmt.Sprintf("claims %s carry the same subject/predicate/object (%s %s %s) — merge candidate",
				strings.Join(claimIDs, ", "), subject, predicate, key.object),
			Details: map[string]any{
				"claims":    claimIDs,
				"subject":   key.subject,
				"predicate": key.predicate,
				"object":    key.object,
			},
		})
	}
}

// versionLabel names a claim's tradition by its best primary citation.
func versionLabel(claim *GraphClaim) string {
	if best := claim.BestPrimary(); best != nil {
		if best.CitationForm != "" {
			return slug(best.CitationForm)
		}
		return slug(best.SourceID)
	}
	return slug(claim.ID)
}

// CheckPredicateConflicts registers genuine multi-source divergence:
// same predicate, conflicting objects, every side citing a credible
// (tier 1–2) source.
//
// Living claims are downgraded to contested — never resolved to a
// winner; a register entry is created (or an existing one reused)
// linking every version, and each claim gets a claim_versions row
// labeled by its best primary citation with its own citations pinned to
// it, so traditions stay separable end-to-end. Conflicts involving an
// abandoned_version claim are registered but left at their confidence:
// abandoned and contested are distinct tradition states.
func CheckPredicateConflicts(state *GraphState, report *Report) {
	type pair struct{ subject, predicate string }
	groups := map[pair][]int{}
	for i := range state.Claims {
		claim := &state.Claims[i]
		if claim.Predicate == "" {
			continue
		}
		key := pair{claim.SubjectEntityID, claim.Predicate}
		groups[key] = append(groups[key], i)
	}

	keys := make([]pair, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(a, b int) bool {
		if keys[a].subject != keys[b].subject {
			return keys[a].subject < keys[b].subject
		}
		return keys[a].predicate < keys[b].predicate
	})

	for _, key := range keys {
		objects := map[string]bool{}
		for _, index := range groups[key] {
			objects[objectKey(&state.Claims[index])] = true
		}
		if len(objects) < 2 {
			continue
		}
		var credible []*GraphClaim
		for _, index := range groups[key] {
			if len(state.Claims[index].PrimaryCitations()) > 0 {
				credible = append(credible, &state.Claims[index])
			}
		}
		if len(credible) < 2 {
			continue
		}
		sort.Slice(credible, func(a, b int) bool { return credible[a].ID < credible[b].ID })

		// Reuse a register entry that already links ≥2 of these claims.
		candidateIDs := map[string]bool{}
		involvesAbandoned := false
		for _, claim := range credible {
			candidateIDs[claim.ID] = true
			if claim.Confidence == ConfidenceAbandonedVersion {
				involvesAbandoned = true
			}
		}
		registrationID := ""
		reuse := false
		for contradictionID, linked := range state.ContradictionLinks {
			overlap := 0
			for claimID := range candidateIDs {
				if linked[claimID] {
					overlap++
				}
			}
			if overlap >= 2 {
				registrationID = contradictionID
				reuse = true
				break
			}
		}
		if registrationID == "" {
			registrationID = slug(orDefault(key.subject, "tradition")) + "-" +
				slug(key.predicate) + "-traditions"
		}

		subjectLabel := key.subject
		if subjectLabel == "" {
			subjectLabel = "the tradition"
		}
		registration := Registration{
			ID:    registrationID,
			Title: fmt.Sprintf("%s: conflicting %s across the traditions", subjectLabel, key.predicate),
			Description: fmt.Sprintf(
				"Registered by the validation engine: claims disagree on '%s' for %s; "+
					"each version cites its own tradition and is preserved as-is, never auto-resolved.",
				key.predicate, subjectLabel),
			Reuse: reuse,
		}
		objectList := []string{}
		objectsByID := map[string]string{}
		for _, claim := range credible {
			registration.Claims = append(registration.Claims, RegistrationClaim{
				ClaimID:      claim.ID,
				VersionLabel: versionLabel(claim),
				Statement:    claim.Statement,
			})
			objectList = append(objectList, claim.ID+" → "+objectKey(claim))
			objectsByID[claim.ID] = objectKey(claim)
		}
		report.Registrations = append(report.Registrations, registration)
		report.Flags = append(report.Flags, Finding{
			CheckCode:  "predicate_value_conflict",
			RecordKind: "claim",
			RecordID:   registration.ID,
			Severity:   SeverityReview,
			Message: fmt.Sprintf("%d credible claims disagree on '%s' for %s: %s — "+
				"versions preserved in the contradiction register",
				len(credible), key.predicate, subjectLabel, strings.Join(objectList, "; ")),
			Details: map[string]any{
				"claims":                     claimIDs(credible),
				"objects":                    objectsByID,
				"contradiction_id":           registration.ID,
				"involves_abandoned_version": involvesAbandoned,
			},
		})

		// Downgrade only living (non-abandoned) claims, and only downward.
		living := 0
		for _, claim := range credible {
			if claim.Confidence == ConfidenceAbandonedVersion {
				continue
			}
			living++
			downgrade := Downgrade{
				ClaimID:   claim.ID,
				Current:   claim.Confidence,
				Target:    ConfidenceContested,
				CheckCode: "predicate_value_conflict",
			}
			if downgrade.Downward() {
				report.Downgrades = append(report.Downgrades, downgrade)
			}
		}
	}
}

// CheckConfidenceRules downgrades established claims without direct
// tier-1 support to derived_probable. Claims already downgraded to
// contested by conflict detection are left alone — the tradition
// divergence is the controlling fact about them.
func CheckConfidenceRules(state *GraphState, report *Report) {
	alreadyDowngraded := map[string]bool{}
	for _, downgrade := range report.Downgrades {
		alreadyDowngraded[downgrade.ClaimID] = true
	}
	for i := range state.Claims {
		claim := &state.Claims[i]
		if claim.Confidence != ConfidenceEstablished {
			continue
		}
		if alreadyDowngraded[claim.ID] {
			continue
		}
		hasTier1 := false
		cited := map[string]bool{}
		for _, citation := range claim.Sources {
			cited[citation.SourceID] = true
			if citation.Tier == 1 {
				hasTier1 = true
			}
		}
		if hasTier1 {
			continue
		}
		report.Downgrades = append(report.Downgrades, Downgrade{
			ClaimID:   claim.ID,
			Current:   claim.Confidence,
			Target:    ConfidenceDerivedProbable,
			CheckCode: "established_without_tier1",
		})
		sources := make([]string, 0, len(cited))
		for sourceID := range cited {
			sources = append(sources, sourceID)
		}
		sort.Strings(sources)
		report.Flags = append(report.Flags, Finding{
			CheckCode:  "established_without_tier1",
			RecordKind: "claim",
			RecordID:   claim.ID,
			Severity:   SeverityReview,
			Message: "confidence established without a Tier-1 citation; downgraded to " +
				"derived_probable (established requires a direct Tier-1 statement)",
			Details: map[string]any{"cited_sources": sources},
		})
	}
}

func claimIDs(claims []*GraphClaim) []string {
	ids := make([]string, 0, len(claims))
	for _, claim := range claims {
		ids = append(ids, claim.ID)
	}
	return ids
}

var whitespace = regexp.MustCompile(`\s+`)

// latinFold maps common accented Latin letters to their base form so
// values like "Fëanor" normalize without a Unicode-normalization
// dependency. Applied after lowercasing.
var latinFold = map[rune]rune{
	'á': 'a', 'à': 'a', 'â': 'a', 'ä': 'a', 'ã': 'a', 'å': 'a',
	'é': 'e', 'è': 'e', 'ê': 'e', 'ë': 'e',
	'í': 'i', 'ì': 'i', 'î': 'i', 'ï': 'i',
	'ó': 'o', 'ò': 'o', 'ô': 'o', 'ö': 'o', 'õ': 'o',
	'ú': 'u', 'ù': 'u', 'û': 'u', 'ü': 'u',
	'ñ': 'n', 'ç': 'c', 'ý': 'y', 'ÿ': 'y',
}

// normalize folds text for equality comparison: lowercase, accents
// folded, whitespace collapsed.
func normalize(text string) string {
	folded := strings.Map(func(r rune) rune {
		if base, ok := latinFold[r]; ok {
			return base
		}
		return r
	}, strings.ToLower(text))
	return strings.TrimSpace(whitespace.ReplaceAllString(folded, " "))
}

var slugSeparators = regexp.MustCompile(`[^a-z0-9]+`)

// slug reduces text to a stable id fragment.
func slug(text string) string {
	slugified := strings.Trim(slugSeparators.ReplaceAllString(normalize(text), "-"), "-")
	if slugified == "" {
		return "tradition"
	}
	return slugified
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
