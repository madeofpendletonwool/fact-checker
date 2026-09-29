package validate

import (
	"strings"
	"testing"
)

func TestParseVerdictsHappyPath(t *testing.T) {
	text := `{"verdicts": [
		{"claim_id": "a", "verdict": "agree", "agreement": 0.93, "rationale": "plainly stated"},
		{"claim_id": "b", "verdict": "downgrade", "agreement": 0.41,
		 "target_confidence": "derived_probable", "rationale": "overreach"}
	]}`
	verdicts, problems, err := ParseVerdicts(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %+v", problems)
	}
	if len(verdicts) != 2 {
		t.Fatalf("verdict count wrong: %+v", verdicts)
	}
	if verdicts[0].EffectiveAgreement() != 0.93 {
		t.Fatalf("agreement wrong: %v", verdicts[0].EffectiveAgreement())
	}
	if verdicts[1].TargetConfidence != ConfidenceDerivedProbable {
		t.Fatalf("target wrong: %q", verdicts[1].TargetConfidence)
	}
}

func TestParseVerdictsClaimsKeyFallback(t *testing.T) {
	verdicts, _, err := ParseVerdicts(`{"claims": [{"claim_id": "a", "verdict": "agree"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 1 {
		t.Fatalf("claims key must be accepted: %+v", verdicts)
	}
	if verdicts[0].EffectiveAgreement() != 1.0 {
		t.Fatal("agree defaults to full agreement")
	}
}

func TestParseVerdictsFencedAndProsed(t *testing.T) {
	text := "Here you go:\n```json\n{\"verdicts\": [{\"claim_id\": \"a\", \"verdict\": \"flag_review\"}]}\n```\nthanks"
	verdicts, _, err := ParseVerdicts(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 1 || verdicts[0].Verdict != "flag_review" {
		t.Fatalf("fenced payload must parse: %+v", verdicts)
	}
	if verdicts[0].EffectiveAgreement() != 0.0 {
		t.Fatal("non-agree defaults to zero agreement")
	}
}

func TestParseVerdictsSalvagesSiblings(t *testing.T) {
	text := `{"verdicts": [
		{"claim_id": "", "verdict": "agree"},
		{"claim_id": "b", "verdict": "explode"},
		{"claim_id": "c", "verdict": "agree", "agreement": 7},
		{"claim_id": "d", "verdict": "downgrade", "target_confidence": "ultra_sure"}
	]}`
	verdicts, problems, err := ParseVerdicts(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 2 {
		t.Fatalf("valid records must survive: %+v", verdicts)
	}
	if len(problems) != 3 {
		t.Fatalf("problems wrong: %+v", problems)
	}
	for _, verdict := range verdicts {
		if verdict.ClaimID == "c" && verdict.EffectiveAgreement() != 1.0 {
			t.Fatalf("agreement must clamp to [0,1]: %v", verdict.EffectiveAgreement())
		}
		if verdict.ClaimID == "d" && verdict.TargetConfidence != "" {
			t.Fatalf("invalid target must drop: %q", verdict.TargetConfidence)
		}
	}
}

func TestParseVerdictsNullsAndMissing(t *testing.T) {
	verdicts, problems, err := ParseVerdicts(`{"verdicts": null}`)
	if err != nil || len(verdicts) != 0 || len(problems) != 0 {
		t.Fatalf("null verdicts are empty: %+v %+v %v", verdicts, problems, err)
	}
	verdicts, _, err = ParseVerdicts(`{}`)
	if err != nil || len(verdicts) != 0 {
		t.Fatalf("missing key is empty: %+v %v", verdicts, err)
	}
	verdicts, _, err = ParseVerdicts(`{"verdicts": {"not": "a list"}}`)
	if err != nil || len(verdicts) != 0 {
		t.Fatalf("non-list must salvage to empty: %+v %v", verdicts, err)
	}
}

func TestParseVerdictsUnparseableIsError(t *testing.T) {
	for _, text := range []string{
		"I am sorry, I cannot do that.",
		"[]",
		"[1, 2, 3]",
	} {
		if _, _, err := ParseVerdicts(text); err == nil {
			t.Fatalf("%q must be a parse error", text)
		}
	}
}

func TestBuildPromptsCarryContract(t *testing.T) {
	brief := Brief{Name: "Service Docs", Mission: "docs checked against live infrastructure",
		Rules: []string{"Tier 1 is live configuration."}}
	system := BuildSystemPrompt(brief)
	for _, needle := range []string{
		"adversarial auditor for Service Docs",
		"ATTACK THE CLAIM, PROTECT THE READER",
		"Never propose an upgrade",
		`"verdict"`,
		"Tier 1 is live configuration.",
	} {
		if !strings.Contains(system, needle) {
			t.Fatalf("system prompt missing %q", needle)
		}
	}

	reviews := []ClaimReview{{
		ClaimID:        "svc-listen-port",
		Statement:      "The service listens on 8080.",
		Confidence:     ConfidenceEstablished,
		Structure:      "svc / listen_port / value:8080",
		Citations:      []string{"source: config (tier 1) — values.yaml locator: {\"file\": \"values.yaml\"}"},
		ReferenceTitle: "Service overview",
		ReferenceURL:   "https://docs/service",
		ReferenceText:  "The service listens on 8080.",
	}, {
		ClaimID:        "no-structure",
		Statement:      "Something unstructured.",
		Confidence:     ConfidenceDerivedProbable,
		Citations:      []string{"source: config (tier 1) locator: {}"},
		ReferenceTitle: "Doc",
		ReferenceURL:   "https://docs/other",
		ReferenceText:  "Text.",
	}}
	user := BuildUserPrompt(reviews)
	for _, needle := range []string{
		"svc-listen-port",
		"The service listens on 8080.",
		"established",
		"svc / listen_port / value:8080",
		"https://docs/service",
		"values.yaml",
		`"verdicts"`,
	} {
		if !strings.Contains(user, needle) {
			t.Fatalf("user prompt missing %q", needle)
		}
	}
}
