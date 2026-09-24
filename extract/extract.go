// Engine-side vocabulary shared by the wire schema, validation, prompts,
// and the runner: work units, citation candidates, and the Store interface
// adapters read graph state through.

package extract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// CitationPolicy is the adapter-configured shape of the citation contract.
// The Arda both-tier rule sets both flags: every record cites the reference
// document AND at least one primary candidate.
type CitationPolicy struct {
	// RequireReference: each claim/event must cite the unit's reference
	// source (the document being extracted).
	RequireReference bool `json:"require_reference"`
	// RequirePrimary: each claim/event must cite at least one non-reference
	// source from the candidate set.
	RequirePrimary bool `json:"require_primary"`
}

// BothTier is the reference implementation's citation policy.
func BothTier() CitationPolicy { return CitationPolicy{RequireReference: true, RequirePrimary: true} }

// Citation is one source reference with an adapter-shaped locator.
type Citation struct {
	SourceID string         `json:"source_id"`
	Locator  map[string]any `json:"locator,omitempty"`
	Note     string         `json:"note,omitempty"`
}

// Candidate is one citeable primary-source unit offered to the model for a
// work unit. Locators are exact: the model is told to cite them verbatim,
// and validation checks the required keys are present.
type Candidate struct {
	SourceID     string         `json:"source_id"`
	Tier         int            `json:"tier"`
	CitationForm string         `json:"citation_form,omitempty"`
	Locator      map[string]any `json:"locator"`
	Title        string         `json:"title"`
	Period       string         `json:"period,omitempty"`
	Summary      string         `json:"summary,omitempty"`
	DefaultPick  bool           `json:"default_pick,omitempty"`
}

// Source is one registry row, as builders need it.
type Source struct {
	ID            string         `json:"id"`
	Tier          int            `json:"tier"`
	Title         string         `json:"title"`
	CitationForm  string         `json:"citation_form"`
	LocatorScheme map[string]any `json:"locator_scheme"`
}

// Document is one ingested raw_documents row.
type Document struct {
	ID          int64     `json:"id"`
	SourceID    string    `json:"source_id"`
	Title       string    `json:"title"`
	URL         string    `json:"url"`
	FetchedAt   time.Time `json:"fetched_at"`
	Revision    string    `json:"revision"`
	ContentText string    `json:"content_text"`
}

// Unit is one model call's scope: a document's text plus the citation
// surface the adapter built for it. One unit = one ledger row = one
// transaction.
type Unit struct {
	// UnitKey is the stable resume key for this unit within its source
	// (e.g. 'tolkien-gateway:galadriel').
	UnitKey    string `json:"unit_key"`
	DocumentID int64  `json:"document_id"`
	Title      string `json:"title"`

	// Text is the (possibly truncated) document text sent to the model.
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`

	// Reference is the citation every record must carry when the policy
	// requires it — normally the document itself.
	Reference Citation `json:"reference"`

	// Candidates are the primary-source units the model may cite.
	Candidates []Candidate `json:"candidates"`

	// Policy is the citation contract for this unit.
	Policy CitationPolicy `json:"policy"`

	// ScopeNotes are adapter context lines rendered into the prompt
	// (what corpus this extraction serves — never citeable).
	ScopeNotes []string `json:"scope_notes,omitempty"`

	// KnownEntityIDs, RelTypes, and RelAliases snapshot the graph state so
	// the model references existing records instead of duplicating them.
	KnownEntityIDs []string          `json:"known_entity_ids,omitempty"`
	RelTypes       []string          `json:"rel_types,omitempty"`
	RelAliases     map[string]string `json:"rel_aliases,omitempty"`
}

// InputHash is the ledger's input-drift key: any change to the unit's text
// or citation surface produces a new hash, so a done unit is re-extracted
// rather than silently stale.
func (u *Unit) InputHash() string {
	candidates := make([]map[string]any, 0, len(u.Candidates))
	for _, c := range u.Candidates {
		candidates = append(candidates, map[string]any{
			"source_id": c.SourceID,
			"locator":   c.Locator,
			"title":     c.Title,
		})
	}
	sum := sha256.Sum256([]byte(u.Text))
	payload, err := json.Marshal(map[string]any{
		"document_id": u.DocumentID,
		"title":       u.Title,
		"candidates":  candidates,
		"text_length": len(u.Text),
		"text_sha256": hex.EncodeToString(sum[:]),
	})
	if err != nil {
		// map[string]any of strings and ints always marshals.
		panic(fmt.Sprintf("unit input hash: %v", err))
	}
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:])
}

// Store is the read side of the database the engine and its unit builders
// share. Implementations must be safe for concurrent reads, but stage
// runners call them from a single goroutine.
type Store interface {
	// LatestDocuments returns the newest revision per URL for one source.
	LatestDocuments(ctx context.Context, sourceID string) ([]Document, error)
	// Sources returns the full registry keyed by id.
	Sources(ctx context.Context) (map[string]Source, error)
	// KnownEntityIDs returns every entity id in the graph.
	KnownEntityIDs(ctx context.Context) ([]string, error)
	// RelationshipTypes returns the allowed rel_type vocabulary.
	RelationshipTypes(ctx context.Context) ([]string, error)
	// RelationshipTypeAliases maps surface spellings to canonical types.
	RelationshipTypeAliases(ctx context.Context) (map[string]string, error)
}

// sortedIDs stabilises snapshot lists for prompts and hashes.
func sortedIDs(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}
