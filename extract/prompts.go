// Versioned prompts for the extraction stage. Prompts are code: the
// version changes whenever prompt content changes in a way that affects
// outputs — done units in the extraction ledger key on it, so a bump
// re-extracts the corpus under the new contract. Bump = new version
// string, never edit in place.

package extract

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PromptVersion is the ledger key for the extraction contract.
const PromptVersion = "extract-001"

// wireContract is the JSON shape the model must return. It is embedded in
// the system prompt verbatim so the contract is reviewable in one place.
const wireContract = `{
  "entities": [
    {"id": "lowercase-slug", "kind": "adapter-ontology-kind",
     "canonical_name": "Canonical Name",
     "names": [{"name": "Variant", "name_kind": "variant|alias|translation|other", "language": "optional"}],
     "attributes": {}}
  ],
  "events": [
    {"id": "lowercase-slug", "kind": "adapter-ontology-kind",
     "summary": "one-sentence summary", "description": "short factual description or null",
     "dating": {"any adapter-defined dating fields, e.g. label": "..."},
     "location_entity_id": "entity-id-or-null",
     "participants": {"entity-id": "role"},
     "causes": ["id-of-an-earlier-event"],
     "consequences": ["id-of-a-later-event"],
     "sources": [CITATION, ...]}
  ],
  "claims": [
    {"id": "lowercase-slug", "statement": "one atomic statement",
     "confidence": "established|derived_probable|contested|abandoned_version",
     "subject_entity_id": "entity-id", "predicate": "verb-phrase",
     "object_entity_id": "entity-id", "object_value": null,
     "versions": [{"label": "tradition-label", "statement": "this tradition's phrasing",
                   "sources": [CITATION, ...]}],
     "sources": [CITATION, ...]}
  ],
  "relationships": [
    {"from_entity_id": "entity-id", "to_entity_id": "entity-id",
     "rel_type": "one-of-the-allowed-types", "claim_id": "optional-justifying-claim-id"}
  ]
}
CITATION = {"source_id": "...", "locator": {...}, "note": "optional"}`

// DomainBrief is the adapter-supplied framing rendered into the system
// prompt: what the corpus is and any domain phrasing the extractor needs.
type DomainBrief struct {
	// Name is the corpus label ("The History of Arda", "Service docs").
	Name string
	// Role names the extractor ("extraction historian", "docs analyst").
	Role string
	// Mission is one or two sentences on what the corpus builds.
	Mission string
	// DomainRules are extra rules the adapter appends (dating conventions,
	// ontology guidance, tier meanings).
	DomainRules []string
}

// BuildSystemPrompt renders the extraction contract for one unit's policy
// and domain.
func BuildSystemPrompt(brief DomainBrief, policy CitationPolicy, reference Citation) string {
	var b strings.Builder
	fmt.Fprintf(&b, `You are the %s for %s: %s

PRIME DIRECTIVE — EXTRACT AND CITE, NEVER DECIDE. You read the provided material and EXTRACT what it says, with citations. You never decide what is true from your own knowledge. Every fact you emit must be stated in or directly implied by the provided document text, and must be attributed to the primary sources listed as underlying it. Facts you know from elsewhere but that the document does not carry are FORBIDDEN. When the document and your knowledge disagree, the document wins or the record is omitted.
`, brief.Role, brief.Name, brief.Mission)

	b.WriteString("\nCITATION RULES (hard requirements — records violating them are discarded):\n")
	rule := 1
	if policy.RequireReference {
		refLocator, _ := json.Marshal(reference.Locator)
		fmt.Fprintf(&b, "%d. Every claim and every event MUST cite the reference document itself: {\"source_id\": %q, \"locator\": %s}.\n", rule, reference.SourceID, refLocator)
		rule++
	}
	if policy.RequirePrimary {
		fmt.Fprintf(&b, "%d. Every claim and every event MUST cite at least one PRIMARY source from the candidate list, using that candidate's given locator.\n", rule)
		rule++
	}
	fmt.Fprintf(&b, "%d. Omit any claim or event you cannot cite as required. Never invent a citation, never cite a source not in the candidate list.\n", rule)
	fmt.Fprintf(&b, "%d. Only cite candidates that actually underlie the statement.\n", rule+1)

	b.WriteString(`
CONFIDENCE (claims only):
- established: stated plainly by the primary tradition.
- derived_probable: inferred, e.g. the document synthesizes a dating or order.
- contested: the traditions disagree. Give per-tradition "versions", each with its own label, phrasing, and citations. Never pick a winner.
- abandoned_version: a version the sources later abandoned. Always pair it with the version that replaced it (as its own claim or a version).

ENTITY RULES:
- Entities are the graph nodes of this domain (the kinds are listed in the task message or the candidate context).
- ids are stable lowercase slugs (lowercase letters, digits, hyphens/underscores only), unique within your output; reuse an id for the same entity, never mint two ids for one entity.
- Only emit entities that your claims/events/relationships actually reference, plus those the document centrally documents.
- relationship rel_type MUST be one of the allowed types listed in the task message.

RECORD HYGIENE:
- Claims are ATOMIC: one fact each, no compound statements.
- statements/summaries are your own factual phrasing of what the document says — never copy sentences from it wholesale.
- Cross-references (participants, causes, consequences, subject/object entity ids, claim_id, location) must reference ids defined in this same output or already-known ids given in the task message. Dangling references are dropped.

OUTPUT FORMAT:
Return ONE JSON object and nothing else — no prose, no markdown fences. Shape:
`)
	b.WriteString(wireContract)
	b.WriteString("\nEmpty lists are fine. Emit nothing rather than something uncited.")

	for _, rule := range brief.DomainRules {
		b.WriteString("\n- ")
		b.WriteString(rule)
	}
	return b.String()
}

// BuildUserPrompt renders the task message for one unit.
func BuildUserPrompt(brief DomainBrief, unit *Unit) string {
	var b strings.Builder
	fmt.Fprintf(&b, "TASK: Extract %s records from this reference document.\n", brief.Name)

	if len(unit.ScopeNotes) > 0 {
		b.WriteString("\nThe corpus context this extraction serves (for scope, not for citation):\n")
		for _, note := range unit.ScopeNotes {
			b.WriteString("- " + note + "\n")
		}
	}

	b.WriteString("\n")
	if len(unit.KnownEntityIDs) > 0 {
		sample := sortedIDs(unit.KnownEntityIDs)
		if len(sample) > 120 {
			sample = append(sample[:120:120], "…")
		}
		fmt.Fprintf(&b, "Already-known entity ids you may reference (do not redefine them unless the document adds names/attributes): %s\n", strings.Join(sample, ", "))
	} else {
		b.WriteString("No entities exist in the graph yet.\n")
	}

	relTypes := "(none)"
	if len(unit.RelTypes) > 0 {
		relTypes = strings.Join(unit.RelTypes, ", ")
	}
	fmt.Fprintf(&b, "\nAllowed relationship rel_types: %s\n", relTypes)

	b.WriteString("\nPRIMARY SOURCE CANDIDATES (cite only these):\n")
	b.WriteString(renderCandidates(unit.Candidates))

	refWhere := ""
	if url, ok := unit.Reference.Locator["url"].(string); ok && url != "" {
		refWhere = "url: " + url
	} else if locator, err := json.Marshal(unit.Reference.Locator); err == nil {
		refWhere = "locator: " + string(locator)
	}
	refHeader := "Reference document:"
	if unit.Policy.RequireReference {
		refHeader = "Reference document (the citation every record must carry):"
	}
	fmt.Fprintf(&b, "\n%s\ntitle: %s\n%s\n", refHeader, unit.Title, refWhere)

	fmt.Fprintf(&b, "\nDOCUMENT TEXT:\n\"\"\"\n%s\n\"\"\"\n", unit.Text)

	b.WriteString("\nExtract the claims, events, entities, and relationships this document supports, following every rule in the system message. Return only the JSON object.")
	return b.String()
}

func renderCandidates(candidates []Candidate) string {
	var b strings.Builder
	for _, c := range candidates {
		fmt.Fprintf(&b, "- source_id: %s\n  tier: %d\n", c.SourceID, c.Tier)
		if c.CitationForm != "" {
			fmt.Fprintf(&b, "  cite as: %s\n", c.CitationForm)
		}
		locator, _ := json.Marshal(c.Locator)
		fmt.Fprintf(&b, "  locator: %s\n", locator)
		fmt.Fprintf(&b, "  unit: %s\n", c.Title)
		if c.Period != "" {
			fmt.Fprintf(&b, "  period: %s\n", c.Period)
		}
		if c.Summary != "" {
			fmt.Fprintf(&b, "  covers: %s\n", c.Summary)
		}
	}
	if b.Len() == 0 {
		return "(none — no primary candidates attach to this document)\n"
	}
	return b.String()
}
