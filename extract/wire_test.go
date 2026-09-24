package extract

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestJSONBlock(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
		fail  bool
	}{
		{"plain", `{"a": 1}`, `{"a": 1}`, false},
		{"fenced", "```json\n{\"a\": 1}\n```", `{"a": 1}`, false},
		{"prose wrapped", "Here you go:\n{\"a\": 1}\nHope that helps!", `{"a": 1}`, false},
		{"no object", "no braces at all", "", true},
		{"reversed braces", `} {`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := JSONBlock(tc.input)
			if tc.fail {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.TrimSpace(got) != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseExtractionCleanPayload(t *testing.T) {
	payload := `{
	  "entities": [{"id": "feanor", "kind": "person", "canonical_name": "Fëanor",
	                "names": [{"name": "Curufinwë", "name_kind": "variant"}], "attributes": {}}],
	  "claims": [{"id": "c1", "statement": "S.", "confidence": "established",
	              "sources": [{"source_id": "s", "locator": {"chapter": "X"}}]}]
	}`
	wire, problems, err := ParseExtraction(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if len(wire.Entities) != 1 || len(wire.Claims) != 1 {
		t.Fatalf("unexpected wire: %+v", wire)
	}
	if wire.Claims[0].Sources[0].Locator["chapter"] != "X" {
		t.Fatalf("locator lost: %+v", wire.Claims[0].Sources[0])
	}
}

func TestParseExtractionNullLists(t *testing.T) {
	wire, problems, err := ParseExtraction(`{"entities": null, "events": null, "claims": null, "relationships": null}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("nulls are not problems: %v", problems)
	}
	if wire.Entities == nil || wire.Claims == nil {
		t.Fatalf("nulls should become empty lists: %+v", wire)
	}
}

func TestParseExtractionSalvagesValidSiblings(t *testing.T) {
	payload := `{
	  "claims": [
	    {"id": "good", "statement": "S.", "confidence": "derived_probable",
	     "sources": [{"source_id": "s", "locator": {}}]},
	    {"id": "bad", "statement": "S.", "confidence": "nope"},
	    {"id": "worse", "statement": "S.", "confidence": "established", "surprise_field": 1}
	  ]
	}`
	wire, problems, err := ParseExtraction(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire.Claims) != 1 || wire.Claims[0].ID != "good" {
		t.Fatalf("valid sibling must survive: %+v", wire.Claims)
	}
	if len(problems) != 2 {
		t.Fatalf("want 2 problems, got %d: %v", len(problems), problems)
	}
	joined := strings.Join(problems, "\n")
	if !strings.Contains(joined, "invalid confidence") {
		t.Fatalf("missing confidence problem: %v", problems)
	}
	if !strings.Contains(joined, "surprise_field") {
		t.Fatalf("missing unknown-field problem: %v", problems)
	}
}

func TestParseExtractionMissingRequiredFields(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{"entity no id", `{"entities": [{"kind": "person", "canonical_name": "X"}]}`, "id is required"},
		{"entity no kind", `{"entities": [{"id": "x", "canonical_name": "X"}]}`, "kind is required"},
		{"event no summary", `{"events": [{"id": "e"}]}`, "summary is required"},
		{"claim no statement", `{"claims": [{"id": "c", "confidence": "established"}]}`, "statement is required"},
		{"citation no source", `{"claims": [{"id": "c", "statement": "s", "confidence": "established", "sources": [{"locator": {}}]}]}`, "source_id is required"},
		{"version no label", `{"claims": [{"id": "c", "statement": "s", "confidence": "contested", "versions": [{"statement": "v"}]}]}`, "label is required"},
		{"rel no type", `{"relationships": [{"from_entity_id": "a", "to_entity_id": "b"}]}`, "rel_type is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, problems, err := ParseExtraction(tc.payload)
			if err != nil {
				t.Fatal(err)
			}
			if len(problems) != 1 || !strings.Contains(problems[0], tc.want) {
				t.Fatalf("want problem %q, got %v", tc.want, problems)
			}
		})
	}
}

func TestParseExtractionNotAList(t *testing.T) {
	_, problems, err := ParseExtraction(`{"claims": {"id": "not-a-list"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "not a list") {
		t.Fatalf("want not-a-list problem, got %v", problems)
	}
}

func TestParseExtractionInvalidJSON(t *testing.T) {
	if _, _, err := ParseExtraction(`{"claims": [`); err == nil {
		t.Fatal("expected WireParseError for invalid JSON")
	}
	if _, _, err := ParseExtraction(`"just a string"`); err == nil {
		t.Fatal("expected WireParseError for non-object payload")
	}
}

func TestWireRoundTripMarshal(t *testing.T) {
	wire := &WireExtraction{
		Claims: []WireClaim{{
			ID: "c", Statement: "s", Confidence: ConfidenceEstablished,
			Sources: []WireCitation{{SourceID: "src", Locator: map[string]any{"url": "https://x"}}},
		}},
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	parsed, problems, err := ParseExtraction(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 || len(parsed.Claims) != 1 {
		t.Fatalf("round trip failed: %v %+v", problems, parsed)
	}
}
