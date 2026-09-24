package extract

import (
	"strings"
	"testing"
)

// testUnit is the citation surface every drop-rule test runs against: the
// reference document (url locator) plus one chapter-locator candidate —
// the Arda both-tier shape.
func testUnit() *Unit {
	return &Unit{
		UnitKey:    "ref:doc",
		DocumentID: 1,
		Title:      "Doc",
		Text:       "text",
		Reference:  Citation{SourceID: "ref", Locator: map[string]any{"url": "https://ref/doc"}},
		Candidates: []Candidate{{SourceID: "silmarillion", Tier: 1, Locator: map[string]any{"chapter": "Of the Flight of the Noldor"}, Title: "Of the Flight of the Noldor"}},
		Policy:     BothTier(),
		RelTypes:   []string{"father_of"},
		RelAliases: map[string]string{"parent_of": "father_of"},
	}
}

func refCitation() WireCitation {
	return WireCitation{SourceID: "ref", Locator: map[string]any{"url": "https://ref/doc"}}
}

func primaryCitation() WireCitation {
	return WireCitation{SourceID: "silmarillion", Locator: map[string]any{"chapter": "Of the Flight of the Noldor"}}
}

func bothTiers() []WireCitation {
	return []WireCitation{refCitation(), primaryCitation()}
}

func newContext() *ValidationContext {
	return &ValidationContext{
		Unit:        testUnit(),
		DBEntityIDs: map[string]bool{"known-entity": true},
		DBEventIDs:  map[string]bool{"known-event": true},
		DBClaimIDs:  map[string]bool{"known-claim": true},
	}
}

func dropsWithReason(result *Validated, reason string) []Drop {
	matches := []Drop{}
	for _, d := range result.Drops {
		if d.Reason == reason || strings.HasPrefix(d.Reason, reason) {
			matches = append(matches, d)
		}
	}
	return matches
}

func TestValidateKeepsFullyCitedClaim(t *testing.T) {
	wire := &WireExtraction{
		Claims: []WireClaim{{
			ID: "c1", Statement: "S.", Confidence: ConfidenceEstablished,
			SubjectEntityID: "e1", Predicate: "crafted", ObjectValue: "the Silmarils",
			Sources: bothTiers(),
		}},
		Entities: []WireEntity{{ID: "e1", Kind: "person", CanonicalName: "E"}},
	}
	result := Validate(wire, newContext())
	if len(result.Drops) != 0 {
		t.Fatalf("unexpected drops: %+v", result.Drops)
	}
	if len(result.Claims) != 1 || result.Claims[0].ObjectValue != "the Silmarils" {
		t.Fatalf("claim lost: %+v", result.Claims)
	}
}

func TestValidateClaimCitationDropReasons(t *testing.T) {
	cases := []struct {
		name    string
		sources []WireCitation
		reason  string
	}{
		{"uncited", nil, "uncited"},
		{"missing primary only", []WireCitation{refCitation()}, "missing_primary_citation"},
		{"missing reference", []WireCitation{primaryCitation()}, "missing_reference_citation"},
		{"bad locator then missing primary", []WireCitation{refCitation(), {SourceID: "silmarillion", Locator: map[string]any{"section": "X"}}}, "missing_primary_citation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wire := &WireExtraction{Claims: []WireClaim{{
				ID: "c1", Statement: "S.", Confidence: ConfidenceEstablished, Sources: tc.sources,
			}}}
			result := Validate(wire, newContext())
			if len(result.Claims) != 0 {
				t.Fatalf("claim must be dropped: %+v", result.Claims)
			}
			if len(dropsWithReason(result, "claim:c1:"+tc.reason)) == 0 && len(dropsWithReason(result, tc.reason)) == 0 {
				t.Fatalf("want drop reason %q, got %+v", tc.reason, result.Drops)
			}
		})
	}
	// The bad-locator citation itself is logged separately.
	wire := &WireExtraction{Claims: []WireClaim{{
		ID: "c1", Statement: "S.", Confidence: ConfidenceEstablished,
		Sources: []WireCitation{refCitation(), {SourceID: "silmarillion", Locator: map[string]any{"section": "X"}}},
	}}}
	result := Validate(wire, newContext())
	if len(dropsWithReason(result, "bad_locator: locator for silmarillion missing required keys ['chapter']")) != 1 {
		t.Fatalf("bad_locator drop missing: %+v", result.Drops)
	}
}

func TestValidateUnknownSourceCitation(t *testing.T) {
	wire := &WireExtraction{Claims: []WireClaim{{
		ID: "c1", Statement: "S.", Confidence: ConfidenceEstablished,
		Sources: []WireCitation{
			refCitation(),
			{SourceID: "made-up-source", Locator: map[string]any{"chapter": "X"}},
		},
	}}}
	result := Validate(wire, newContext())
	if len(result.Claims) != 0 {
		t.Fatal("claim citing an unknown source must drop")
	}
	found := false
	for _, d := range result.Drops {
		if d.Kind == "citation" && strings.Contains(d.Reason, "unknown source 'made-up-source'") {
			found = true
		}
	}
	if !found {
		t.Fatalf("unknown-source drop missing: %+v", result.Drops)
	}
}

func TestValidatePolicyRelaxed(t *testing.T) {
	unit := testUnit()
	unit.Policy = CitationPolicy{RequirePrimary: true}
	wire := &WireExtraction{Claims: []WireClaim{{
		ID: "c1", Statement: "S.", Confidence: ConfidenceEstablished,
		Sources: []WireCitation{primaryCitation()},
	}}}
	if result := Validate(wire, &ValidationContext{Unit: unit}); len(result.Claims) != 1 {
		t.Fatalf("primary-only citation must pass when reference not required: %+v", result.Drops)
	}
}

func TestValidateEntityRules(t *testing.T) {
	wire := &WireExtraction{Entities: []WireEntity{
		{ID: "Bad Slug", Kind: "person", CanonicalName: "X"},
		{ID: "dup", Kind: "person", CanonicalName: "X"},
		{ID: "dup", Kind: "person", CanonicalName: "X2"},
		{ID: "clean", Kind: "person", CanonicalName: "C",
			Names: []WireName{
				{Name: "Epithet", NameKind: "epithet"},
				{Name: "Alias", NameKind: "alias"},
				{Name: "C"},
			}},
	}}
	result := Validate(wire, newContext())
	if len(result.Entities) != 2 {
		t.Fatalf("want 2 kept entities, got %d: %+v", len(result.Entities), result.Entities)
	}
	reasons := map[string]int{}
	for _, d := range result.Drops {
		reasons[d.Reason]++
	}
	if reasons["invalid id (use lowercase slugs)"] != 1 {
		t.Fatalf("invalid-id drop missing: %+v", result.Drops)
	}
	if reasons["duplicate entity id"] != 1 {
		t.Fatalf("duplicate-entity drop missing: %+v", result.Drops)
	}
	for _, e := range result.Entities {
		if e.ID == "clean" {
			if len(e.Names) != 2 {
				t.Fatalf("canonical-name duplicate should be skipped: %+v", e.Names)
			}
			for _, n := range e.Names {
				if n.NameKind == "epithet" {
					t.Fatalf("epithet should normalize to other: %+v", e.Names)
				}
			}
		}
	}
	if len(dropsWithReason(result, "'epithet' -> other")) != 1 {
		t.Fatalf("name_kind normalization not logged: %+v", result.Drops)
	}
}

func TestValidateEventRules(t *testing.T) {
	wire := &WireExtraction{
		Entities: []WireEntity{{ID: "e1", Kind: "person", CanonicalName: "E"}},
		Events: []WireEvent{
			{ID: "Good Event", Summary: "s", Sources: bothTiers()},
			{ID: "dup-event", Summary: "s", Sources: bothTiers()},
			{ID: "dup-event", Summary: "s2", Sources: bothTiers()},
			{ID: "known-event", Summary: "already in graph", Sources: bothTiers()},
			{ID: "uncited", Summary: "s", Sources: nil},
			{
				ID: "linked", Summary: "s", Sources: bothTiers(),
				LocationEntityID: "missing-place",
				Participants:     map[string]string{"e1": "leader", "ghost": "role"},
				Causes:           []string{"known-event", "dropped-cause"},
				Consequences:     []string{"dup-event"},
			},
		},
	}
	result := Validate(wire, newContext())
	kept := map[string]bool{}
	for _, e := range result.Events {
		kept[e.ID] = true
	}
	if !kept["linked"] || !kept["dup-event"] || kept["known-event"] || kept["uncited"] || kept["Good Event"] {
		t.Fatalf("wrong keep set: %+v", kept)
	}
	for _, e := range result.Events {
		if e.ID == "linked" {
			if e.LocationEntityID != "" {
				t.Fatal("dangling location must be dropped")
			}
			if len(e.Participants) != 1 || e.Participants[0].EntityID != "e1" {
				t.Fatalf("dangling participant must be dropped: %+v", e.Participants)
			}
			if len(e.Causes) != 1 || e.Causes[0] != "known-event" {
				t.Fatalf("dangling cause must be dropped: %+v", e.Causes)
			}
		}
	}
	if len(dropsWithReason(result, "dangling entity ref")) != 1 ||
		len(dropsWithReason(result, "dangling location ref 'missing-place'; dropped")) != 1 ||
		len(dropsWithReason(result, "dangling cause ref; dropped")) != 1 {
		t.Fatalf("reference drops missing: %+v", result.Drops)
	}
}

func TestValidateClaimFieldRules(t *testing.T) {
	wire := &WireExtraction{
		Entities: []WireEntity{{ID: "e1", Kind: "person", CanonicalName: "E"}},
		Claims: []WireClaim{
			{ID: "Bad", Statement: "s", Confidence: ConfidenceEstablished, Sources: bothTiers()},
			{ID: "dup", Statement: "s", Confidence: ConfidenceEstablished, Sources: bothTiers()},
			{ID: "dup", Statement: "s2", Confidence: ConfidenceEstablished, Sources: bothTiers()},
			{ID: "known-claim", Statement: "s", Confidence: ConfidenceEstablished, Sources: bothTiers()},
			{
				ID: "dangling", Statement: "s", Confidence: ConfidenceEstablished,
				SubjectEntityID: "ghost", Sources: bothTiers(),
			},
			{
				ID: "both-objects", Statement: "s", Confidence: ConfidenceEstablished,
				SubjectEntityID: "e1", ObjectEntityID: "e1", ObjectValue: "val", Sources: bothTiers(),
			},
		},
	}
	result := Validate(wire, newContext())
	if len(result.Claims) != 2 {
		t.Fatalf("wrong keep set: %+v", result.Claims)
	}
	byID := map[string]ClaimRecord{}
	for _, c := range result.Claims {
		byID[c.ID] = c
	}
	if _, ok := byID["both-objects"]; !ok {
		t.Fatalf("both-objects must be kept: %+v", result.Claims)
	}
	if byID["both-objects"].ObjectValue != "" {
		t.Fatal("object_value must lose to object_entity_id")
	}
	if len(dropsWithReason(result, "both object_entity_id and object_value; kept entity")) != 1 {
		t.Fatalf("field-conflict drop missing: %+v", result.Drops)
	}
	if len(dropsWithReason(result, "dangling subject entity ref 'ghost'")) != 1 {
		t.Fatalf("dangling-ref drop missing: %+v", result.Drops)
	}
}

func TestValidateContestedClaimVersions(t *testing.T) {
	goodVersion := WireVersion{
		Label: "tradition-a", Statement: "A says.",
		Sources: []WireCitation{refCitation(), {SourceID: "silmarillion", Locator: map[string]any{"chapter": "X"}}},
	}
	badVersion := WireVersion{Label: "tradition-b", Statement: "B says.", Sources: []WireCitation{primaryCitation()}}

	wire := &WireExtraction{Claims: []WireClaim{{
		ID: "contested-claim", Statement: "s", Confidence: ConfidenceContested,
		Versions: []WireVersion{goodVersion, badVersion},
		Sources:  bothTiers(),
	}}}
	result := Validate(wire, newContext())
	if len(result.Claims) != 1 {
		t.Fatalf("claim must be kept: %+v", result.Drops)
	}
	if len(result.Claims[0].Versions) != 1 || result.Claims[0].Versions[0].Label != "tradition-a" {
		t.Fatalf("only the both-tier version survives: %+v", result.Claims[0].Versions)
	}
	if len(dropsWithReason(result, "missing_reference_citation")) != 1 {
		t.Fatalf("version drop missing: %+v", result.Drops)
	}
	if len(result.Contradictions) != 1 || result.Contradictions[0].ClaimID != "contested-claim" {
		t.Fatalf("contradiction must auto-register: %+v", result.Contradictions)
	}

	wire.Claims[0].Versions = nil
	result = Validate(wire, newContext())
	if len(result.Claims) != 0 {
		t.Fatal("contested claim without versions must drop")
	}
	if len(dropsWithReason(result, "contested_without_versions")) != 1 {
		t.Fatalf("contested_without_versions drop missing: %+v", result.Drops)
	}
}

func TestValidateRelationshipRules(t *testing.T) {
	wire := &WireExtraction{
		Entities: []WireEntity{{ID: "e1", Kind: "person", CanonicalName: "E"}},
		Relationships: []WireRelationship{
			{FromEntityID: "e1", ToEntityID: "e1", RelType: "father_of"},
			{FromEntityID: "e1", ToEntityID: "known-entity", RelType: "member_of"},
			{FromEntityID: "e1", ToEntityID: "ghost", RelType: "father_of"},
			{FromEntityID: "e1", ToEntityID: "known-entity", RelType: "parent_of"},
			{FromEntityID: "e1", ToEntityID: "known-entity", RelType: "father_of"},
			{FromEntityID: "e1", ToEntityID: "known-entity", RelType: "father-of"},
			{FromEntityID: "known-entity", ToEntityID: "e1", RelType: "parent_of", ClaimID: "ghost-claim"},
		},
	}
	result := Validate(wire, newContext())
	if len(result.Relationships) != 2 {
		t.Fatalf("want 2 kept relationships (alias and explicit are one edge), got %d: %+v",
			len(result.Relationships), result.Relationships)
	}
	reasons := map[string]int{}
	for _, d := range result.Drops {
		if d.Kind == "relationship" {
			reasons[d.Reason]++
		}
	}
	if reasons["self-relationship"] != 1 || reasons["unknown rel_type 'member_of'"] != 1 ||
		reasons["unknown rel_type 'father-of'"] != 1 ||
		reasons["dangling endpoint"] != 1 || reasons["duplicate relationship"] != 1 {
		t.Fatalf("relationship drops wrong: %+v", result.Drops)
	}
	for _, rel := range result.Relationships {
		if rel.RelType != "father_of" {
			t.Fatalf("aliases must normalize: %+v", rel)
		}
		if rel.FromEntityID == "known-entity" && rel.ClaimID != "" {
			t.Fatalf("dangling claim_id must be dropped, edge kept: %+v", rel)
		}
	}
	if len(dropsWithReason(result, "dangling claim_id 'ghost-claim'; kept without justification")) != 1 {
		t.Fatalf("claim_id drop missing: %+v", result.Drops)
	}
}

func TestValidateKnownGraphIDsResolve(t *testing.T) {
	ctx := newContext()
	ctx.DBEntityIDs["known2-entity"] = true
	wire := &WireExtraction{
		Claims: []WireClaim{{
			ID: "c1", Statement: "s", Confidence: ConfidenceEstablished,
			SubjectEntityID: "known-entity", Sources: bothTiers(),
		}},
		Relationships: []WireRelationship{
			{FromEntityID: "known-entity", ToEntityID: "known2-entity", RelType: "father_of", ClaimID: "known-claim"},
		},
	}
	result := Validate(wire, ctx)
	if len(result.Claims) != 1 || len(result.Relationships) != 1 {
		t.Fatalf("known graph ids must resolve: %+v", result.Drops)
	}
}

func TestValidateRecordCounts(t *testing.T) {
	wire := &WireExtraction{
		Entities: []WireEntity{{ID: "e1", Kind: "person", CanonicalName: "E"}},
		Claims:   []WireClaim{{ID: "c1", Statement: "s", Confidence: ConfidenceContested, Versions: []WireVersion{{Label: "v", Statement: "vs", Sources: bothTiers()}}, Sources: bothTiers()}},
	}
	result := Validate(wire, newContext())
	counts := result.RecordCounts()
	if counts["entities"] != 1 || counts["claims"] != 1 || counts["contradictions"] != 1 {
		t.Fatalf("counts wrong: %+v", counts)
	}
}
