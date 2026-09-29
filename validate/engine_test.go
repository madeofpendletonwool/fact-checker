package validate

import (
	"strings"
	"testing"
)

func citation(tier int, sourceID string) GraphCitation {
	return GraphCitation{SourceID: sourceID, Tier: tier}
}

func claim(id, statement, confidence, subject, predicate, objectEntity, objectValue string, sources ...GraphCitation) GraphClaim {
	return GraphClaim{
		ID: id, Statement: statement, Confidence: confidence,
		SubjectEntityID: subject, Predicate: predicate,
		ObjectEntityID: objectEntity, ObjectValue: objectValue,
		Sources: sources,
	}
}

func findingsByCode(report *Report, code string) []Finding {
	flags := []Finding{}
	for _, flag := range report.Flags {
		if flag.CheckCode == code {
			flags = append(flags, flag)
		}
	}
	return flags
}

func TestCheckClaimShape(t *testing.T) {
	state := &GraphState{Claims: []GraphClaim{
		claim("shaped", "A is B.", ConfidenceEstablished, "a", "is_a", "b", "", citation(1, "sil")),
		claim("shapeless", "Something is true.", ConfidenceEstablished, "a", "", "", "", citation(1, "sil")),
	}}
	report := RunEngine(state)
	flags := findingsByCode(report, "claim_shape_invalid")
	if len(flags) != 1 || flags[0].RecordID != "shapeless" || flags[0].Severity != SeverityReview {
		t.Fatalf("claim_shape flags wrong: %+v", flags)
	}
}

func TestCheckDanglingReferences(t *testing.T) {
	state := &GraphState{
		Entities: []GraphEntity{{ID: "exists"}},
		Events: []GraphEvent{
			{ID: "event", Participants: []string{"ghost", "exists"}, LocationEntityID: "nowhere"},
		},
		Claims: []GraphClaim{
			claim("bad-subject", "…", ConfidenceEstablished, "ghost", "is_a", "exists", "", citation(1, "sil")),
			claim("bad-object", "…", ConfidenceEstablished, "exists", "is_a", "phantom", "", citation(1, "sil")),
			claim("fine", "…", ConfidenceEstablished, "exists", "is_a", "exists", "", citation(1, "sil")),
		},
	}
	report := RunEngine(state)
	flags := findingsByCode(report, "dangling_reference")
	if len(flags) != 4 {
		t.Fatalf("want 4 dangling flags (participant, location, subject, object), got %+v", flags)
	}
	for _, flag := range flags {
		if flag.Severity != SeverityError {
			t.Fatalf("dangling refs are errors: %+v", flag)
		}
	}
}

func TestCheckDuplicateClaims(t *testing.T) {
	state := &GraphState{Claims: []GraphClaim{
		claim("one", "A is B.", ConfidenceEstablished, "a", "is_a", "b", "", citation(1, "sil")),
		claim("two", "A is also B.", ConfidenceEstablished, "a", "is_a", "b", "", citation(1, "sil")),
		claim("other", "A is C.", ConfidenceEstablished, "a", "is_a", "c", "", citation(1, "sil")),
	}}
	report := RunEngine(state)
	flags := findingsByCode(report, "duplicate_claim")
	if len(flags) != 1 {
		t.Fatalf("want one duplicate group, got %+v", flags)
	}
	if !strings.Contains(flags[0].Message, "one, two") {
		t.Fatalf("duplicate flag must name both claims: %s", flags[0].Message)
	}
	if flags[0].Severity != SeverityReview {
		t.Fatalf("duplicates are review severity: %+v", flags[0])
	}
}

func TestCheckDuplicateClaimsValueObjectsNormalize(t *testing.T) {
	state := &GraphState{Claims: []GraphClaim{
		claim("one", "Port 8080.", ConfidenceEstablished, "svc", "port", "", "8080", citation(1, "cfg")),
		claim("two", "Port  8080.", ConfidenceEstablished, "svc", "port", "", "8080 ", citation(1, "cfg")),
	}}
	report := RunEngine(state)
	if len(findingsByCode(report, "duplicate_claim")) != 1 {
		t.Fatal("value objects must normalize before comparison")
	}
}

func TestCheckPredicateConflictsRegistersAndDowngrades(t *testing.T) {
	state := &GraphState{Claims: []GraphClaim{
		claim("says-b", "A is B.", ConfidenceEstablished, "a", "is_a", "b", "",
			citation(1, "sil"), citation(3, "ref")),
		claim("says-c", "A is C.", ConfidenceEstablished, "a", "is_a", "c", "",
			citation(2, "ut"), citation(3, "ref")),
	}}
	report := RunEngine(state)

	if len(report.Registrations) != 1 {
		t.Fatalf("want one registration, got %+v", report.Registrations)
	}
	registration := report.Registrations[0]
	if registration.ID != "a-is-a-traditions" || registration.Reuse {
		t.Fatalf("registration wrong: %+v", registration)
	}
	if len(registration.Claims) != 2 {
		t.Fatalf("both traditions must register: %+v", registration.Claims)
	}
	if registration.Claims[0].VersionLabel != "sil" || registration.Claims[1].VersionLabel != "ut" {
		t.Fatalf("version labels must come from citation forms / source ids: %+v", registration.Claims)
	}
	if len(findingsByCode(report, "predicate_value_conflict")) != 1 {
		t.Fatal("conflict flag missing")
	}
	if len(report.Downgrades) != 2 {
		t.Fatalf("both living claims downgrade to contested: %+v", report.Downgrades)
	}
	for _, downgrade := range report.Downgrades {
		if downgrade.Target != ConfidenceContested || !downgrade.Downward() {
			t.Fatalf("downgrade wrong: %+v", downgrade)
		}
	}
	// established_without_tier1 must not re-flag claims already contested
	// by the conflict.
	for _, flag := range findingsByCode(report, "established_without_tier1") {
		if flag.RecordID == "says-b" || flag.RecordID == "says-c" {
			t.Fatalf("conflict-downgraded claims must not get tier1 flags: %+v", flag)
		}
	}
}

func TestCheckPredicateConflictsNeedsCredibleBothSides(t *testing.T) {
	state := &GraphState{Claims: []GraphClaim{
		claim("credible", "A is B.", ConfidenceEstablished, "a", "is_a", "b", "", citation(1, "sil")),
		claim("blog-noise", "A is C.", ConfidenceEstablished, "a", "is_a", "c", "", citation(3, "ref")),
	}}
	report := RunEngine(state)
	if len(report.Registrations) != 0 {
		t.Fatal("uncited conflicts must not register")
	}
	if len(findingsByCode(report, "predicate_value_conflict")) != 0 {
		t.Fatal("no conflict flag without credible sources")
	}
}

func TestCheckPredicateConflictsAbandonedVersionRegistersWithoutDowngrade(t *testing.T) {
	state := &GraphState{Claims: []GraphClaim{
		claim("living", "A is B.", ConfidenceEstablished, "a", "is_a", "b", "", citation(1, "sil")),
		claim("abandoned", "A is C.", ConfidenceAbandonedVersion, "a", "is_a", "c", "", citation(1, "ut")),
	}}
	report := RunEngine(state)
	if len(report.Registrations) != 1 {
		t.Fatal("abandoned traditions still register")
	}
	flags := findingsByCode(report, "predicate_value_conflict")
	if len(flags) != 1 || flags[0].Details["involves_abandoned_version"] != true {
		t.Fatalf("abandoned involvement must be recorded: %+v", flags)
	}
	if len(report.Downgrades) != 1 || report.Downgrades[0].ClaimID != "living" {
		t.Fatalf("only the living claim downgrades: %+v", report.Downgrades)
	}
}

func TestCheckPredicateConflictsReusesRegisterEntry(t *testing.T) {
	state := &GraphState{
		Claims: []GraphClaim{
			claim("says-b", "A is B.", ConfidenceEstablished, "a", "is_a", "b", "", citation(1, "sil")),
			claim("says-c", "A is C.", ConfidenceEstablished, "a", "is_a", "c", "", citation(1, "ut")),
		},
		ContradictionLinks: map[string]map[string]bool{
			"existing-entry": {"says-b": true, "says-c": true},
		},
	}
	report := RunEngine(state)
	if len(report.Registrations) != 1 || !report.Registrations[0].Reuse ||
		report.Registrations[0].ID != "existing-entry" {
		t.Fatalf("entry linking both claims must be reused: %+v", report.Registrations)
	}
}

func TestCheckPredicateConflictsNoDowngradeWhenNotDownward(t *testing.T) {
	state := &GraphState{Claims: []GraphClaim{
		claim("contested-b", "A is B.", ConfidenceContested, "a", "is_a", "b", "", citation(1, "sil")),
		claim("contested-c", "A is C.", ConfidenceContested, "a", "is_a", "c", "", citation(1, "ut")),
	}}
	report := RunEngine(state)
	if len(report.Registrations) != 1 {
		t.Fatal("conflict still registers")
	}
	if len(report.Downgrades) != 0 {
		t.Fatalf("contested claims cannot move further down by conflict: %+v", report.Downgrades)
	}
}

func TestCheckConfidenceRules(t *testing.T) {
	state := &GraphState{Claims: []GraphClaim{
		claim("solid", "A is B.", ConfidenceEstablished, "a", "is_a", "b", "", citation(1, "sil")),
		claim("shaky", "C is D.", ConfidenceEstablished, "c", "is_a", "d", "", citation(3, "ref")),
		claim("probable", "E is F.", ConfidenceDerivedProbable, "e", "is_a", "f", "", citation(3, "ref")),
	}}
	report := RunEngine(state)
	flags := findingsByCode(report, "established_without_tier1")
	if len(flags) != 1 || flags[0].RecordID != "shaky" {
		t.Fatalf("only the tierless established claim flags: %+v", flags)
	}
	if len(report.Downgrades) != 1 || report.Downgrades[0].ClaimID != "shaky" ||
		report.Downgrades[0].Target != ConfidenceDerivedProbable {
		t.Fatalf("downgrade wrong: %+v", report.Downgrades)
	}
}

func TestDowngradeDownward(t *testing.T) {
	cases := []struct {
		current, target string
		downward        bool
	}{
		{ConfidenceEstablished, ConfidenceDerivedProbable, true},
		{ConfidenceEstablished, ConfidenceContested, true},
		{ConfidenceDerivedProbable, ConfidenceEstablished, false},
		{ConfidenceContested, ConfidenceContested, false},
		{ConfidenceDerivedProbable, ConfidenceContested, true},
	}
	for _, tc := range cases {
		d := Downgrade{Current: tc.current, Target: tc.target}
		if d.Downward() != tc.downward {
			t.Fatalf("%s -> %s: want downward=%v", tc.current, tc.target, tc.downward)
		}
	}
}

func TestRunEngineRunsAdapterChecksAlongsideCore(t *testing.T) {
	custom := func(state *GraphState, report *Report) {
		for i := range state.Claims {
			if len(state.Claims[i].Statement) > 12 {
				report.Flags = append(report.Flags, Finding{
					CheckCode:  "statement_too_long",
					RecordKind: "claim",
					RecordID:   state.Claims[i].ID,
					Severity:   SeverityWarning,
					Message:    "statement exceeds the adapter's 12-char bound",
				})
			}
		}
	}
	state := &GraphState{Claims: []GraphClaim{
		claim("long-one", "This statement is long.", ConfidenceEstablished, "a", "is_a", "b", "", citation(1, "sil")),
	}}
	report := RunEngine(state, custom)
	if len(findingsByCode(report, "statement_too_long")) != 1 {
		t.Fatal("adapter check must run alongside the core set")
	}
	if len(findingsByCode(report, "claim_shape_invalid")) != 0 {
		t.Fatal("core checks still ran")
	}
}

func TestObjectKeyAndSlugNormalize(t *testing.T) {
	if got := objectKey(&GraphClaim{ObjectValue: "Fëanor  the  Elder"}); got != "value:feanor the elder" {
		t.Fatalf("object key normalization wrong: %q", got)
	}
	if got := slug("Of the Flight of the Noldor"); got != "of-the-flight-of-the-noldor" {
		t.Fatalf("slug wrong: %q", got)
	}
	if got := slug("…"); got != "tradition" {
		t.Fatalf("empty slug must fall back: %q", got)
	}
}
