// Wire schema: the JSON contract a model response must satisfy. Kept
// separate from the DB record shapes so the prompt contract can evolve
// without touching persistence, and vice versa.

package extract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Confidence levels, engine-fixed. The four-level order is part of the
// platform contract; adapters remap display names only.
const (
	ConfidenceEstablished      = "established"
	ConfidenceDerivedProbable  = "derived_probable"
	ConfidenceContested        = "contested"
	ConfidenceAbandonedVersion = "abandoned_version"
)

// ValidConfidence reports whether s is one of the four fixed levels.
func ValidConfidence(s string) bool {
	switch s {
	case ConfidenceEstablished, ConfidenceDerivedProbable, ConfidenceContested, ConfidenceAbandonedVersion:
		return true
	}
	return false
}

// WireCitation is the citation shape inside a model response.
type WireCitation struct {
	SourceID string         `json:"source_id"`
	Locator  map[string]any `json:"locator"`
	Note     string         `json:"note"`
}

// WireName is one entity name variant.
type WireName struct {
	Name     string `json:"name"`
	NameKind string `json:"name_kind"`
	Language string `json:"language"`
}

// WireEntity is one entity in a model response.
type WireEntity struct {
	ID            string         `json:"id"`
	Kind          string         `json:"kind"`
	CanonicalName string         `json:"canonical_name"`
	Names         []WireName     `json:"names"`
	Attributes    map[string]any `json:"attributes"`
}

// WireEvent is one event in a model response. Dating is adapter-shaped
// free-form data stored verbatim under events.attributes.dating.
type WireEvent struct {
	ID               string            `json:"id"`
	Kind             string            `json:"kind"`
	Summary          string            `json:"summary"`
	Description      string            `json:"description"`
	Dating           map[string]any    `json:"dating"`
	LocationEntityID string            `json:"location_entity_id"`
	Participants     map[string]string `json:"participants"`
	Causes           []string          `json:"causes"`
	Consequences     []string          `json:"consequences"`
	Sources          []WireCitation    `json:"sources"`
}

// WireVersion is one per-tradition version of a contested claim.
type WireVersion struct {
	Label     string         `json:"label"`
	Statement string         `json:"statement"`
	Sources   []WireCitation `json:"sources"`
}

// WireClaim is one claim in a model response.
type WireClaim struct {
	ID              string         `json:"id"`
	Statement       string         `json:"statement"`
	Confidence      string         `json:"confidence"`
	SubjectEntityID string         `json:"subject_entity_id"`
	Predicate       string         `json:"predicate"`
	ObjectEntityID  string         `json:"object_entity_id"`
	ObjectValue     string         `json:"object_value"`
	Versions        []WireVersion  `json:"versions"`
	Sources         []WireCitation `json:"sources"`
}

// WireRelationship is one graph edge in a model response.
type WireRelationship struct {
	FromEntityID string `json:"from_entity_id"`
	ToEntityID   string `json:"to_entity_id"`
	RelType      string `json:"rel_type"`
	ClaimID      string `json:"claim_id"`
}

// WireExtraction is the whole payload. Lists are optional; null counts as
// empty (models legitimately emit null for "no items").
type WireExtraction struct {
	Entities      []WireEntity       `json:"entities"`
	Events        []WireEvent        `json:"events"`
	Claims        []WireClaim        `json:"claims"`
	Relationships []WireRelationship `json:"relationships"`
}

// WireParseError reports a response that is not parseable as an extraction
// payload at all (not JSON, or not a JSON object).
type WireParseError struct{ msg string }

func (e *WireParseError) Error() string { return e.msg }

// JSONBlock best-effort strips prose and markdown fences around a
// JSON object.
func JSONBlock(text string) (string, error) {
	stripped := strings.TrimSpace(text)
	if fences := strings.Split(stripped, "```"); len(fences) >= 3 {
		for _, fence := range fences[1 : len(fences)-1] {
			inner := strings.TrimSpace(fence)
			if inner == "" {
				continue
			}
			if idx := strings.IndexByte(inner, '\n'); idx >= 0 {
				inner = strings.TrimSpace(inner[idx+1:])
			}
			stripped = inner
			break
		}
	}
	start := strings.IndexByte(stripped, '{')
	end := strings.LastIndexByte(stripped, '}')
	if start == -1 || end == -1 || end <= start {
		return "", &WireParseError{msg: "response contains no JSON object"}
	}
	return stripped[start : end+1], nil
}

// ParseExtraction parses a model response into a WireExtraction.
//
// Records are decoded one by one so a single malformed record never sinks
// its valid siblings; per-record problems are returned alongside the
// salvage and never raised. A payload that is not JSON at all, or not a
// JSON object, is a WireParseError the runner repairs by re-prompting.
func ParseExtraction(text string) (*WireExtraction, []string, error) {
	block, err := JSONBlock(text)
	if err != nil {
		return nil, nil, err
	}
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(strings.NewReader(block))
	if err := dec.Decode(&raw); err != nil {
		return nil, nil, &WireParseError{msg: fmt.Sprintf("invalid JSON: %v", err)}
	}

	problems := []string{}
	wire := &WireExtraction{
		Entities:      salvageList[WireEntity]("entities", raw["entities"], &problems),
		Events:        salvageList[WireEvent]("events", raw["events"], &problems),
		Claims:        salvageList[WireClaim]("claims", raw["claims"], &problems),
		Relationships: salvageList[WireRelationship]("relationships", raw["relationships"], &problems),
	}
	return wire, problems, nil
}

// wireRecord is implemented by every wire record type; a non-empty
// required-problem marks the record malformed for salvage.
type wireRecord interface {
	requiredProblem() string
}

func (e WireEntity) requiredProblem() string {
	switch {
	case e.ID == "":
		return "id is required"
	case e.Kind == "":
		return "kind is required"
	case e.CanonicalName == "":
		return "canonical_name is required"
	}
	return ""
}

func (e WireEvent) requiredProblem() string {
	switch {
	case e.ID == "":
		return "id is required"
	case e.Summary == "":
		return "summary is required"
	}
	return citationsProblem("sources", e.Sources)
}

func (v WireVersion) requiredProblem() string {
	switch {
	case v.Label == "":
		return "label is required"
	case v.Statement == "":
		return "statement is required"
	}
	return citationsProblem("sources", v.Sources)
}

func (c WireClaim) requiredProblem() string {
	switch {
	case c.ID == "":
		return "id is required"
	case c.Statement == "":
		return "statement is required"
	case !ValidConfidence(c.Confidence):
		return fmt.Sprintf("invalid confidence %q", c.Confidence)
	}
	if problem := citationsProblem("sources", c.Sources); problem != "" {
		return problem
	}
	for _, v := range c.Versions {
		if p := v.requiredProblem(); p != "" {
			return fmt.Sprintf("versions[]: %s", p)
		}
	}
	return ""
}

func (r WireRelationship) requiredProblem() string {
	switch {
	case r.FromEntityID == "":
		return "from_entity_id is required"
	case r.ToEntityID == "":
		return "to_entity_id is required"
	case r.RelType == "":
		return "rel_type is required"
	}
	return ""
}

func citationsProblem(field string, citations []WireCitation) string {
	for _, c := range citations {
		if c.SourceID == "" {
			return fmt.Sprintf("%s[].source_id is required", field)
		}
	}
	return ""
}

// salvageList decodes raw (which may be nil, null, or malformed) into a
// slice of T, appending one problem per record that fails strict decoding
// or required-field checks. Unknown fields are rejected, mirroring the
// reference contract.
func salvageList[T wireRecord](kind string, raw json.RawMessage, problems *[]string) []T {
	items := []T{}
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return items
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		*problems = append(*problems, fmt.Sprintf("%s: not a list; skipped", kind))
		return items
	}
	for i, item := range arr {
		dec := json.NewDecoder(strings.NewReader(string(item)))
		dec.DisallowUnknownFields()
		var record T
		if err := dec.Decode(&record); err != nil {
			*problems = append(*problems, fmt.Sprintf("%s[%d]: %v", kind, i, err))
			continue
		}
		if problem := record.requiredProblem(); problem != "" {
			*problems = append(*problems, fmt.Sprintf("%s[%d]: %s", kind, i, problem))
			continue
		}
		items = append(items, record)
	}
	return items
}
