// Adversarial second-model validation: every claim re-checked
// independently.
//
// For each extracted claim, an independent model pass receives the claim,
// its citations with locators, and the reference passage it was extracted
// from — then attacks it. The verdict ledger (validation_verdicts, keyed
// by claim + prompt version) makes batch runs idempotent and resumable,
// mirroring the extraction ledger. Disagreements are flagged for review
// or applied as confidence downgrades; a machine pass never upgrades and
// never deletes.

package validate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultReferenceChars caps the reference passage sent per claim.
const DefaultReferenceChars = 6000

// ClaimReview is one adversarial work item: everything the auditor sees.
type ClaimReview struct {
	ClaimID    string
	Statement  string
	Confidence string
	Structure  string // subject / predicate / object, when shaped
	Citations  []string
	// ReferenceTitle, ReferenceURL, and ReferenceText locate the passage
	// the claim was extracted from.
	ReferenceTitle string
	ReferenceURL   string
	ReferenceText  string
}

// ReviewPlan is the split of pending claims into checkable reviews and
// blocked claims (flagged, retried on the next run).
type ReviewPlan struct {
	Reviews []ClaimReview
	Blocked []Finding
}

// ClaimIDs lists the plan's review targets.
func (p *ReviewPlan) ClaimIDs() []string {
	ids := make([]string, 0, len(p.Reviews))
	for _, review := range p.Reviews {
		ids = append(ids, review.ClaimID)
	}
	return ids
}

// BuildReviewPlan builds the adversarial work items. A claim's reference
// citation is the one citing the adapter's reference source with a url
// locator; claims without a usable citation or passage are blocked and
// flagged (they are retried on the next run).
func BuildReviewPlan(ctx context.Context, pool *pgxpool.Pool, claims []GraphClaim, referenceSource string, maxReferenceChars int) (*ReviewPlan, error) {
	if maxReferenceChars <= 0 {
		maxReferenceChars = DefaultReferenceChars
	}
	plan := &ReviewPlan{}
	for i := range claims {
		claim := &claims[i]
		reference := findReferenceCitation(claim, referenceSource)
		if reference == nil {
			plan.Blocked = append(plan.Blocked, Finding{
				CheckCode:  "missing_reference_citation",
				RecordKind: "claim",
				RecordID:   claim.ID,
				Severity:   SeverityReview,
				Message:    "claim has no reference-article citation to re-check against",
			})
			continue
		}
		url, _ := reference.Locator["url"].(string)
		title, text, ok, err := referenceText(ctx, pool, referenceSource, url, maxReferenceChars)
		if err != nil {
			return nil, err
		}
		if !ok {
			plan.Blocked = append(plan.Blocked, Finding{
				CheckCode:  "missing_reference_text",
				RecordKind: "claim",
				RecordID:   claim.ID,
				Severity:   SeverityReview,
				Message:    fmt.Sprintf("reference article not in the raw store: %s", url),
				Details:    map[string]any{"url": url},
			})
			continue
		}

		citations := make([]string, 0, len(claim.Sources))
		for _, citation := range claim.Sources {
			line := fmt.Sprintf("source: %s (tier %d)", citation.SourceID, citation.Tier)
			if citation.CitationForm != "" {
				line += fmt.Sprintf(" — %s", citation.CitationForm)
			}
			line += " locator: " + renderLocator(citation.Locator)
			citations = append(citations, line)
		}

		structure := ""
		if claim.Predicate != "" {
			bits := []string{orDefault(claim.SubjectEntityID, "—"), claim.Predicate}
			switch {
			case claim.ObjectEntityID != "":
				bits = append(bits, "entity:"+claim.ObjectEntityID)
			case claim.ObjectValue != "":
				bits = append(bits, "value:"+claim.ObjectValue)
			default:
				bits = append(bits, "none")
			}
			structure = strings.Join(bits, " / ")
		}
		plan.Reviews = append(plan.Reviews, ClaimReview{
			ClaimID:        claim.ID,
			Statement:      claim.Statement,
			Confidence:     claim.Confidence,
			Structure:      structure,
			Citations:      citations,
			ReferenceTitle: title,
			ReferenceURL:   url,
			ReferenceText:  text,
		})
	}
	return plan, nil
}

// findReferenceCitation picks the claim's reference-source citation with
// a url locator.
func findReferenceCitation(claim *GraphClaim, referenceSource string) *GraphCitation {
	for i := range claim.Sources {
		citation := &claim.Sources[i]
		if citation.SourceID != referenceSource {
			continue
		}
		if url, ok := citation.Locator["url"].(string); ok && url != "" {
			return citation
		}
	}
	return nil
}

// referenceText loads (title, text) of the newest ingested revision for
// a reference URL.
func referenceText(ctx context.Context, pool *pgxpool.Pool, sourceID, url string, maxChars int) (string, string, bool, error) {
	var title *string
	var content *string
	err := pool.QueryRow(ctx, `
		SELECT title, content_text FROM raw_documents
		WHERE url = $1 AND source_id = $2
		ORDER BY fetched_at DESC LIMIT 1`, url, sourceID).Scan(&title, &content)
	if err != nil {
		if isNoRows(err) {
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("validate: query reference text: %w", err)
	}
	if content == nil {
		return "", "", false, nil
	}
	resolvedTitle := url
	if title != nil {
		resolvedTitle = *title
	}
	text := *content
	if len(text) > maxChars {
		text = text[:maxChars]
	}
	return resolvedTitle, text, true, nil
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
