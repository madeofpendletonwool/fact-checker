// Package arda is the Arda-shaped adapter: the reference implementation's
// extraction domain lifted into configuration. What was hardcoded there —
// the Tolkien Gateway reference source, the both-tier citation rule, the
// lexical candidate selection over primary source units — is data here.
//
// A manifest (see testdata/arda_pilot.json) names the reference source,
// the citation policy, the candidate primary-source units with their
// locators and scoring keywords, and the prompt framing.
package arda

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/madeofpendletonwool/fact-checker/adapters"
	"github.com/madeofpendletonwool/fact-checker/extract"
)

// CandidateConfig is one citeable primary-source unit.
type CandidateConfig struct {
	Source   string         `json:"source"`
	Title    string         `json:"title"`
	Locator  map[string]any `json:"locator"`
	Period   string         `json:"period,omitempty"`
	Summary  string         `json:"summary,omitempty"`
	Keywords []string       `json:"keywords,omitempty"`
	Default  bool           `json:"default,omitempty"`
}

// Domain is the prompt framing.
type Domain struct {
	Name    string   `json:"name"`
	Role    string   `json:"role"`
	Mission string   `json:"mission"`
	Rules   []string `json:"rules,omitempty"`
}

// Config is the full manifest body.
type Config struct {
	ReferenceSource string                 `json:"reference_source"`
	Domain          Domain                 `json:"domain"`
	Policy          extract.CitationPolicy `json:"policy"`
	ScopeNotes      []string               `json:"scope_notes,omitempty"`
	MaxCandidates   int                    `json:"max_candidates"`
	MaxUnitChars    int                    `json:"max_unit_chars"`
	// CandidateTierMax caps which source tiers may serve as candidates
	// (the reference implementation's "primary" filter: tiers 1–2).
	CandidateTierMax int               `json:"candidate_tier_max"`
	Candidates       []CandidateConfig `json:"candidate_units"`
}

// Adapter builds extraction units the way the reference pipeline did:
// one unit per latest-revision document of the reference source, with
// candidates selected per document by lexical overlap.
type Adapter struct {
	config Config
	brief  extract.DomainBrief
}

// Default caps, mirroring the reference implementation.
const (
	DefaultMaxCandidates = 14
	DefaultMaxUnitChars  = 24000
	DefaultTierMax       = 2
)

// Parse builds the adapter from a manifest body.
func Parse(raw json.RawMessage) (any, error) {
	var config Config
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, fmt.Errorf("arda: parse config: %w", err)
	}
	if config.ReferenceSource == "" {
		return nil, fmt.Errorf("arda: reference_source is required")
	}
	if config.Domain.Name == "" || config.Domain.Role == "" || config.Domain.Mission == "" {
		return nil, fmt.Errorf("arda: domain name, role, and mission are required")
	}
	if config.MaxCandidates <= 0 {
		config.MaxCandidates = DefaultMaxCandidates
	}
	if config.MaxUnitChars <= 0 {
		config.MaxUnitChars = DefaultMaxUnitChars
	}
	if config.CandidateTierMax <= 0 {
		config.CandidateTierMax = DefaultTierMax
	}
	for _, candidate := range config.Candidates {
		if candidate.Source == "" || candidate.Title == "" || len(candidate.Locator) == 0 {
			return nil, fmt.Errorf("arda: candidate %q needs source, title, and locator", candidate.Title)
		}
	}
	return &Adapter{
		config: config,
		brief: extract.DomainBrief{
			Name:        config.Domain.Name,
			Role:        config.Domain.Role,
			Mission:     config.Domain.Mission,
			DomainRules: config.Domain.Rules,
		},
	}, nil
}

func init() {
	adapters.Register("arda", Parse)
}

// Brief implements extract.Adapter.
func (a *Adapter) Brief() extract.DomainBrief { return a.brief }

// BuildUnits implements extract.Adapter: one unit per latest-revision
// document of the reference source, candidates narrowed per document.
func (a *Adapter) BuildUnits(ctx context.Context, s extract.Store) ([]extract.Unit, error) {
	sources, err := s.Sources(ctx)
	if err != nil {
		return nil, err
	}
	reference, ok := sources[a.config.ReferenceSource]
	if !ok {
		return nil, fmt.Errorf("arda: reference source %q missing — load the source registry first",
			a.config.ReferenceSource)
	}
	documents, err := s.LatestDocuments(ctx, a.config.ReferenceSource)
	if err != nil {
		return nil, err
	}
	known, err := s.KnownEntityIDs(ctx)
	if err != nil {
		return nil, err
	}
	relTypes, err := s.RelationshipTypes(ctx)
	if err != nil {
		return nil, err
	}
	aliases, err := s.RelationshipTypeAliases(ctx)
	if err != nil {
		return nil, err
	}

	units := make([]extract.Unit, 0, len(documents))
	for _, doc := range documents {
		text := doc.ContentText
		truncated := false
		if len(text) > a.config.MaxUnitChars {
			text = text[:a.config.MaxUnitChars]
			truncated = true
		}
		units = append(units, extract.Unit{
			UnitKey:        reference.ID + ":" + slugify(doc.Title),
			DocumentID:     doc.ID,
			Title:          doc.Title,
			Text:           text,
			Truncated:      truncated,
			Reference:      extract.Citation{SourceID: reference.ID, Locator: map[string]any{"url": doc.URL}},
			Candidates:     a.selectCandidates(doc.Title, text, sources),
			Policy:         a.config.Policy,
			ScopeNotes:     a.config.ScopeNotes,
			KnownEntityIDs: known,
			RelTypes:       relTypes,
			RelAliases:     aliases,
		})
	}
	return units, nil
}

// selectCandidates ports the reference lexical narrowing: score every
// configured candidate by term overlap with the document, keep the top
// slice, fall back to the flagged defaults when nothing matches.
func (a *Adapter) selectCandidates(title, text string, sources map[string]extract.Source) []extract.Candidate {
	articleTerms := terms(title + " " + text[:min(len(text), 4000)])

	ranked := make([]rankedCandidate, len(a.config.Candidates))
	for i, candidate := range a.config.Candidates {
		surface := candidate.Title + " " + candidate.Summary + " " + strings.Join(candidate.Keywords, " ")
		candidateTerms := terms(surface)
		score := 0
		for term := range candidateTerms {
			if articleTerms[term] {
				score++
			}
		}
		ranked[i] = rankedCandidate{index: i, score: score}
	}
	sort.SliceStable(ranked, func(x, y int) bool {
		if ranked[x].score != ranked[y].score {
			return ranked[x].score > ranked[y].score
		}
		return ranked[x].index < ranked[y].index
	})

	keep := a.config.MaxCandidates
	if keep > len(ranked) {
		keep = len(ranked)
	}
	chosen := ranked[:keep]
	if !anyPositive(chosen) {
		// Nothing lexically matches: fall back to the authored defaults
		// (the reference pipeline's "primary" units).
		defaults := []rankedCandidate{}
		for i, candidate := range a.config.Candidates {
			if candidate.Default {
				defaults = append(defaults, rankedCandidate{index: i, score: 0})
			}
		}
		if len(defaults) > keep {
			defaults = defaults[:keep]
		}
		chosen = defaults
	}

	candidates := []extract.Candidate{}
	seen := map[string]bool{}
	for _, pick := range chosen {
		config := a.config.Candidates[pick.index]
		source, ok := sources[config.Source]
		if !ok || source.Tier > a.config.CandidateTierMax {
			continue
		}
		key := config.Source + "\x00" + config.Title
		if seen[key] {
			continue
		}
		seen[key] = true
		candidates = append(candidates, extract.Candidate{
			SourceID:     source.ID,
			Tier:         source.Tier,
			CitationForm: source.CitationForm,
			Locator:      config.Locator,
			Title:        config.Title,
			Period:       config.Period,
			Summary:      config.Summary,
		})
	}
	return candidates
}

// rankedCandidate is one configured candidate with its overlap score.
type rankedCandidate struct {
	index int
	score int
}

func anyPositive(ranked []rankedCandidate) bool {
	for _, r := range ranked {
		if r.score > 0 {
			return true
		}
	}
	return false
}

var stopWords = map[string]bool{}

func init() {
	for _, word := range strings.Fields(`a an and the of in to with for from by on at as is are was were be been
		his her their its it he she they them who which that this these those not
		no but or if then than so such into over under out up down after before
		during between against about there here when where what how all any both
		each few more most other some only own same very can will just don should
		now`) {
		stopWords[word] = true
	}
}

// isWordRune keeps letters (including non-Latin scripts) and digits.
func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// terms tokenizes text for overlap scoring: lowercased words longer than
// two characters, stop words dropped.
func terms(text string) map[string]bool {
	words := map[string]bool{}
	for _, field := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !isWordRune(r)
	}) {
		if len(field) > 2 && !stopWords[field] {
			words[field] = true
		}
	}
	return words
}

// latinFold maps common accented Latin letters to their base form so
// titles like "Fëanor" slug to "feanor" without a normalization
// dependency. Applied after ToLower.
var latinFold = map[rune]rune{
	'á': 'a', 'à': 'a', 'â': 'a', 'ä': 'a', 'ã': 'a', 'å': 'a',
	'é': 'e', 'è': 'e', 'ê': 'e', 'ë': 'e',
	'í': 'i', 'ì': 'i', 'î': 'i', 'ï': 'i',
	'ó': 'o', 'ò': 'o', 'ô': 'o', 'ö': 'o', 'õ': 'o',
	'ú': 'u', 'ù': 'u', 'û': 'u', 'ü': 'u',
	'ñ': 'n', 'ç': 'c', 'ý': 'y', 'ÿ': 'y',
}

func slugify(title string) string {
	slug := strings.Builder{}
	upper := false
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		if folded, ok := latinFold[r]; ok {
			r = folded
		}
		switch {
		case 'a' <= r && r <= 'z', '0' <= r && r <= '9':
			slug.WriteRune(r)
			upper = false
		default:
			// separators and any remaining non-ASCII rune delimit words
			if !upper && slug.Len() > 0 {
				slug.WriteByte('-')
				upper = true
			}
		}
	}
	out := strings.Trim(slug.String(), "-")
	if out == "" {
		out = "untitled"
	}
	return out
}
