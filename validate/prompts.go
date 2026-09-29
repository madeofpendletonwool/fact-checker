// Versioned prompts for the adversarial validation stage. Prompts are
// code: PROMPT_VERSION keys the verdict ledger (a claim already verdicted
// under the same version is skipped), so a change that affects verdicts
// means bumping the version — never editing in place.

package validate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PromptVersion is the ledger key for the validation contract.
const PromptVersion = "validate-001"

// Brief is the adapter-supplied framing rendered into the adversarial
// prompt: what the corpus is and any domain phrasing the auditor needs.
// The skeptical stance itself is engine-fixed.
type Brief struct {
	// Name is the corpus label ("The History of Arda", "Service docs").
	Name string
	// Mission is one or two sentences on what the corpus builds.
	Mission string
	// Rules are extra domain rules the adapter appends (tier meanings,
	// tradition conventions).
	Rules []string
}

// wireContract is the JSON shape the model must return, embedded in the
// system prompt verbatim so the contract is reviewable in one place.
const wireContract = `{
  "verdicts": [
    {"claim_id": "...",
     "verdict": "agree | downgrade | flag_review",
     "agreement": 0.0,
     "target_confidence": "established | derived_probable | contested | null",
     "rationale": "one or two sentences citing what the passage shows"}
  ]
}`

// BuildSystemPrompt renders the skeptical-auditor contract for a domain.
func BuildSystemPrompt(brief Brief) string {
	var b strings.Builder
	fmt.Fprintf(&b, `You are the adversarial auditor for %s: %s

PRIME DIRECTIVE — ATTACK THE CLAIM, PROTECT THE READER. An extraction model produced each claim from a reference passage and cited primary-source locators. Your job is to find every way the claim could be wrong or overconfident. You are not the author; you are the second, skeptical reader. When in doubt, distrust.

For each claim, judge ONLY against the evidence you are given:
- the reference passage (the text the claim was extracted from), and
- the stated primary citations (source + locator).

HARD RULES:
1. If the passage does not state or directly imply the claim as phrased, the claim fails — verdict "downgrade" (to "derived_probable" or "contested") or "flag_review" when you cannot decide.
2. If the claim is supported but its confidence overreaches the evidence, verdict "downgrade" with a strictly LOWER target_confidence. Never propose an upgrade; upgrades require human review.
3. If the citations look wrong for the content (a locator that cannot plausibly carry this fact), verdict "flag_review".
4. "agreement" is your 0.0–1.0 score of how fully the cited evidence supports the claim as phrased and at its stated confidence.
5. Do NOT use your own knowledge to rescue a claim the passage does not support. Outside knowledge may deepen suspicion, never confirmation.

CONFIDENCE LEVELS: established (stated plainly by a top-tier primary source), derived_probable (legitimate inference or synthesis), contested (traditions genuinely disagree), abandoned_version (a version the sources later abandoned — never proposed as a machine target).

OUTPUT FORMAT:
Return ONE JSON object and nothing else — no prose, no markdown fences. One verdict per claim you were given, same claim ids. Shape:
`, brief.Name, brief.Mission)
	b.WriteString(wireContract)
	for _, rule := range brief.Rules {
		b.WriteString("\n- ")
		b.WriteString(rule)
	}
	return b.String()
}

// BuildUserPrompt renders the task message for one batch of reviews.
func BuildUserPrompt(reviews []ClaimReview) string {
	rendered := make([]string, 0, len(reviews))
	for i, review := range reviews {
		rendered = append(rendered, fmt.Sprintf("### %d\n%s", i+1, renderClaim(review)))
	}
	return fmt.Sprintf(`TASK: Adversarially re-check each extracted claim against its cited evidence. For every claim below, return one verdict object. Judge only from the given passage and citations.

%s

Return only the JSON object with a "verdicts" array covering every claim_id above.`, strings.Join(rendered, "\n\n"))
}

func renderClaim(review ClaimReview) string {
	lines := []string{
		"claim_id: " + review.ClaimID,
		"statement: " + review.Statement,
		"confidence: " + review.Confidence,
	}
	if review.Structure != "" {
		lines = append(lines, "structure: "+review.Structure)
	}
	lines = append(lines, "citations:")
	for _, citation := range review.Citations {
		lines = append(lines, "  - "+citation)
	}
	lines = append(lines,
		fmt.Sprintf("reference passage (%s, %s):", review.ReferenceTitle, review.ReferenceURL),
		`"""`,
		review.ReferenceText,
		`"""`)
	return strings.Join(lines, "\n")
}

// renderLocator is the prompt/JSON rendering shared by the plan builder.
func renderLocator(locator map[string]any) string {
	if len(locator) == 0 {
		return "{}"
	}
	encoded, err := json.Marshal(locator)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}
