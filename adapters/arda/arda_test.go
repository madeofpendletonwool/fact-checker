package arda

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/madeofpendletonwool/fact-checker/adapters"
	"github.com/madeofpendletonwool/fact-checker/extract"
)

type fakeStore struct {
	sources    map[string]extract.Source
	documents  []extract.Document
	entities   []string
	relTypes   []string
	relAliases map[string]string
}

func (s *fakeStore) LatestDocuments(_ context.Context, sourceID string) ([]extract.Document, error) {
	newest := map[string]extract.Document{}
	for _, d := range s.documents {
		if d.SourceID != sourceID {
			continue
		}
		if current, ok := newest[d.URL]; !ok || d.FetchedAt.After(current.FetchedAt) {
			newest[d.URL] = d
		}
	}
	out := make([]extract.Document, 0, len(newest))
	for _, d := range newest {
		out = append(out, d)
	}
	return out, nil
}

func (s *fakeStore) Sources(context.Context) (map[string]extract.Source, error) {
	return s.sources, nil
}

func (s *fakeStore) KnownEntityIDs(context.Context) ([]string, error) { return s.entities, nil }

func (s *fakeStore) RelationshipTypes(context.Context) ([]string, error) { return s.relTypes, nil }

func (s *fakeStore) RelationshipTypeAliases(context.Context) (map[string]string, error) {
	return s.relAliases, nil
}

func pilotStore() *fakeStore {
	return &fakeStore{
		sources: map[string]extract.Source{
			"the-silmarillion": {ID: "the-silmarillion", Tier: 1, Title: "The Silmarillion", CitationForm: "Silmarillion",
				LocatorScheme: map[string]any{"required_keys": []any{"chapter"}}},
			"unfinished-tales": {ID: "unfinished-tales", Tier: 1, Title: "Unfinished Tales", CitationForm: "Unfinished Tales",
				LocatorScheme: map[string]any{"required_keys": []any{"part"}}},
			"tolkien-gateway": {ID: "tolkien-gateway", Tier: 3, Title: "Tolkien Gateway", CitationForm: "Tolkien Gateway",
				LocatorScheme: map[string]any{"required_keys": []any{"url"}}},
		},
		documents: []extract.Document{
			{ID: 1, SourceID: "tolkien-gateway", Title: "Galadriel", URL: "https://tolkiengateway.net/wiki/Galadriel",
				FetchedAt: time.Now(), ContentText: "Galadriel was a Noldorin princess who crossed the Helcaraxë…"},
			{ID: 2, SourceID: "tolkien-gateway", Title: "Fëanor", URL: "https://tolkiengateway.net/wiki/Fëanor",
				FetchedAt: time.Now(), ContentText: "Fëanor crafted the Silmarils in Valinor…"},
		},
		relTypes:   []string{"father_of"},
		relAliases: map[string]string{"parent_of": "father_of"},
	}
}

func loadPilot(t *testing.T) *Adapter {
	t.Helper()
	raw, err := os.ReadFile("testdata/arda_pilot.json")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Parse(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return loaded.(*Adapter)
}

func TestRegisteredByName(t *testing.T) {
	if _, err := adapters.LoadManifest("testdata/arda_pilot.json"); err != nil {
		t.Fatal(err)
	}
}

func TestParseRejectsIncomplete(t *testing.T) {
	cases := []string{
		`{}`,
		`{"reference_source": "x"}`,
		`{"reference_source": "x", "domain": {"name": "n", "role": "r"}}`,
		`{"reference_source": "x", "domain": {"name": "n", "role": "r", "mission": "m"}, "candidate_units": [{"source": "s"}]}`,
	}
	for _, raw := range cases {
		if _, err := Parse(json.RawMessage(raw)); err == nil {
			t.Fatalf("expected error for %s", raw)
		}
	}
}

func TestBuildUnitsShape(t *testing.T) {
	adapter := loadPilot(t)
	units, err := adapter.BuildUnits(context.Background(), pilotStore())
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 2 {
		t.Fatalf("want 2 units, got %d", len(units))
	}
	galadriel := units[0]
	for i := range units {
		if units[i].Title == "Galadriel" {
			galadriel = units[i]
		}
	}
	if galadriel.Title != "Galadriel" {
		t.Fatalf("galadriel unit missing: %+v", units)
	}
	if galadriel.UnitKey != "tolkien-gateway:galadriel" {
		t.Fatalf("unit key = %q", galadriel.UnitKey)
	}
	if galadriel.Reference.SourceID != "tolkien-gateway" ||
		galadriel.Reference.Locator["url"] != "https://tolkiengateway.net/wiki/Galadriel" {
		t.Fatalf("reference citation wrong: %+v", galadriel.Reference)
	}
	if !galadriel.Policy.RequireReference || !galadriel.Policy.RequirePrimary {
		t.Fatalf("pilot policy must be the both-tier rule: %+v", galadriel.Policy)
	}
	if len(galadrelScopeCheck(&galadriel)) == 0 {
		t.Fatal("scope notes missing")
	}
}

func galadrelScopeCheck(u *extract.Unit) []string { return u.ScopeNotes }

func TestBuildUnitsCandidateSelection(t *testing.T) {
	adapter := loadPilot(t)
	units, _ := adapter.BuildUnits(context.Background(), pilotStore())

	find := func(title string) extract.Unit {
		for _, u := range units {
			if u.Title == title {
				return u
			}
		}
		t.Fatalf("unit %q missing", title)
		return extract.Unit{}
	}

	galadriel := find("Galadriel")
	sources := map[string]bool{}
	for _, c := range galadriel.Candidates {
		sources[c.SourceID] = true
	}
	if !sources["the-silmarillion"] || !sources["unfinished-tales"] {
		t.Fatalf("lexical overlap must pick silmarillion + unfinished tales: %+v", galadriel.Candidates)
	}
	if sources["tolkien-gateway"] {
		t.Fatalf("tier-3 probe must never be a candidate: %+v", galadriel.Candidates)
	}
	for _, c := range galadriel.Candidates {
		if c.Tier > 2 {
			t.Fatalf("tier filter leaked: %+v", c)
		}
		if c.CitationForm == "" {
			t.Fatalf("citation form lost: %+v", c)
		}
	}

	feanor := find("Fëanor")
	feanorTitles := map[string]bool{}
	for _, c := range feanor.Candidates {
		feanorTitles[c.Title] = true
	}
	if !feanorTitles["Of Fëanor"] {
		t.Fatalf("Fëanor doc should overlap Of Fëanor: %+v", feanor.Candidates)
	}
}

func TestCandidateCap(t *testing.T) {
	adapter := loadPilot(t)
	adapter.config.MaxCandidates = 1
	units, _ := adapter.BuildUnits(context.Background(), pilotStore())
	for _, unit := range units {
		if len(unit.Candidates) > 1 {
			t.Fatalf("cap exceeded: %+v", unit.Candidates)
		}
	}
}

func TestFallbackDefaultsWhenNoOverlap(t *testing.T) {
	adapter := loadPilot(t)
	store := pilotStore()
	store.documents = []extract.Document{{
		ID: 9, SourceID: "tolkien-gateway", Title: "Utterly Unrelated Topic",
		URL:       "https://tolkiengateway.net/wiki/Unrelated",
		FetchedAt: time.Now(), ContentText: "zqxj wvkq bbq",
	}}
	units, _ := adapter.BuildUnits(context.Background(), store)
	if len(units) != 1 {
		t.Fatalf("want 1 unit, got %d", len(units))
	}
	titles := map[string]bool{}
	for _, c := range units[0].Candidates {
		titles[c.Title] = true
	}
	if !titles["Of the Flight of the Noldor"] || !titles["The History of Galadriel and Celeborn"] {
		t.Fatalf("defaults must carry the run: %+v", units[0].Candidates)
	}
}

func TestTruncationFlag(t *testing.T) {
	adapter := loadPilot(t)
	adapter.config.MaxUnitChars = 10
	store := pilotStore()
	units, _ := adapter.BuildUnits(context.Background(), store)
	for _, unit := range units {
		if !unit.Truncated || len(unit.Text) != 10 {
			t.Fatalf("truncation wrong: %d/%v", len(unit.Text), unit.Truncated)
		}
	}
}

func TestInputHashTracksCitationSurface(t *testing.T) {
	adapter := loadPilot(t)
	units, _ := adapter.BuildUnits(context.Background(), pilotStore())
	base := units[0].InputHash()

	units[0].Text += " appended"
	if changed := units[0].InputHash(); changed == base {
		t.Fatal("text change must change the hash")
	}

	units, _ = adapter.BuildUnits(context.Background(), pilotStore())
	trimmed := units[0]
	trimmed.Candidates = trimmed.Candidates[:1]
	if trimmed.InputHash() == base {
		t.Fatal("candidate-set change must change the hash")
	}

	again, _ := adapter.BuildUnits(context.Background(), pilotStore())
	if again[0].InputHash() != base {
		t.Fatal("identical input must hash identically")
	}
}

func TestLatestRevisionOnly(t *testing.T) {
	adapter := loadPilot(t)
	store := pilotStore()
	old := store.documents[0]
	old.ID = 77
	old.FetchedAt = time.Now().Add(-24 * time.Hour)
	store.documents = append(store.documents, old)

	units, _ := adapter.BuildUnits(context.Background(), store)
	count := 0
	for _, u := range units {
		if u.Title == "Galadriel" {
			count++
			if u.DocumentID != 1 {
				t.Fatalf("must use the newest revision, got doc %d", u.DocumentID)
			}
		}
	}
	if count != 1 {
		t.Fatalf("one unit per URL, got %d", count)
	}
}

func TestBriefCarriesDomain(t *testing.T) {
	brief := loadPilot(t).Brief()
	if brief.Name != "The History of Arda" || brief.Role != "extraction historian" || len(brief.DomainRules) == 0 {
		t.Fatalf("brief wrong: %+v", brief)
	}
}

func TestTermsStopWords(t *testing.T) {
	got := terms("The Fëanor and the Noldor of Valinor")
	want := map[string]bool{"fëanor": true, "noldor": true, "valinor": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("terms = %v, want %v", got, want)
	}
}
