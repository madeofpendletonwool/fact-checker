// Wire schema: the JSON contract the adversarial validator must satisfy.
// Mirrors the extraction stage's tolerant parsing: per-record salvage so
// one malformed verdict never sinks its valid siblings.

package validate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/madeofpendletonwool/fact-checker/extract"
)

// Verdict values the wire contract accepts.
var validVerdicts = map[string]bool{
	"agree":       true,
	"downgrade":   true,
	"flag_review": true,
}

// WireVerdict is one verdict record in a model response.
type WireVerdict struct {
	ClaimID          string   `json:"claim_id"`
	Verdict          string   `json:"verdict"`
	Agreement        *float64 `json:"agreement"`
	TargetConfidence string   `json:"target_confidence"`
	Rationale        string   `json:"rationale"`
}

// EffectiveAgreement is the agreement score with a sensible default:
// 1.0 for 'agree', 0.0 otherwise.
func (v WireVerdict) EffectiveAgreement() float64 {
	if v.Agreement != nil {
		return *v.Agreement
	}
	if v.Verdict == "agree" {
		return 1.0
	}
	return 0.0
}

// Problem is one malformed record in an otherwise salvageable response.
type Problem struct {
	Index   int
	Message string
}

func (p Problem) String() string {
	return fmt.Sprintf("verdicts[%d]: %s", p.Index, p.Message)
}

// ParseVerdicts parses an adversarial-validation response into salvageable
// verdicts. Per-record problems are returned alongside; a response that is
// not JSON at all, or not a JSON object, is an error the caller repairs by
// re-prompting.
func ParseVerdicts(text string) ([]WireVerdict, []Problem, error) {
	block, err := extract.JSONBlock(text)
	if err != nil {
		return nil, nil, err
	}
	var payload map[string]json.RawMessage
	if err := json.NewDecoder(strings.NewReader(block)).Decode(&payload); err != nil {
		return nil, nil, fmt.Errorf("invalid JSON: %w", err)
	}

	raw := payload["verdicts"]
	if raw == nil {
		raw = payload["claims"]
	}
	verdicts := []WireVerdict{}
	problems := []Problem{}
	if raw == nil {
		return verdicts, problems, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return verdicts, append(problems, Problem{Message: "verdicts: not a list; skipped"}), nil
	}
	for i, item := range items {
		var verdict WireVerdict
		if err := json.Unmarshal(item, &verdict); err != nil {
			problems = append(problems, Problem{Index: i, Message: err.Error()})
			continue
		}
		if verdict.ClaimID == "" {
			problems = append(problems, Problem{Index: i, Message: "claim_id is required"})
			continue
		}
		if !validVerdicts[verdict.Verdict] {
			problems = append(problems, Problem{Index: i, Message: fmt.Sprintf("unknown verdict %q", verdict.Verdict)})
			continue
		}
		if verdict.Agreement != nil {
			clamped := *verdict.Agreement
			if clamped < 0 {
				clamped = 0
			}
			if clamped > 1 {
				clamped = 1
			}
			verdict.Agreement = &clamped
		}
		if verdict.TargetConfidence == "null" || verdict.TargetConfidence == "none" {
			verdict.TargetConfidence = ""
		}
		if verdict.TargetConfidence != "" && !extract.ValidConfidence(verdict.TargetConfidence) {
			verdict.TargetConfidence = ""
			problems = append(problems, Problem{Index: i, Message: "target_confidence dropped: not a confidence level"})
		}
		verdicts = append(verdicts, verdict)
	}
	return verdicts, problems, nil
}
