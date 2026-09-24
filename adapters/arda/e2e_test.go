package arda

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/madeofpendletonwool/fact-checker/extract"
	"github.com/madeofpendletonwool/fact-checker/internal/integrity"
	"github.com/madeofpendletonwool/fact-checker/internal/llm"
	"github.com/madeofpendletonwool/fact-checker/internal/migrations"
	"github.com/madeofpendletonwool/fact-checker/internal/testdb"
)

// The pilot corpus: the Galadriel article from the reference
// implementation's pilot scope (Books I–II), with the source registry and
// relationship vocabulary the Arda fixture established in stage 1.
const pilotSeed = `
INSERT INTO sources (id, tier, title, citation_form, license, locator_scheme) VALUES
  ('the-silmarillion', 1, 'The Silmarillion', 'Silmarillion', NULL,
   '{"required_keys": ["chapter"], "optional_keys": ["book", "section"]}'::jsonb),
  ('unfinished-tales', 1, 'Unfinished Tales of Númenor and Middle-earth',
   'Unfinished Tales', NULL, '{"required_keys": ["part"], "optional_keys": ["chapter"]}'::jsonb),
  ('tolkien-gateway', 3, 'Tolkien Gateway', 'Tolkien Gateway', 'CC BY-SA 4.0',
   '{"required_keys": ["url"]}'::jsonb);

INSERT INTO raw_documents (source_id, title, url, fetched_at, license, revision,
                           content_text, content_hash) VALUES
  ('tolkien-gateway', 'Galadriel', 'https://tolkiengateway.net/wiki/Galadriel',
   '2026-09-01T10:00:00Z', 'CC BY-SA 4.0', 'oldid=123456',
   'Galadriel was a Noldorin princess, daughter of Finarfin, born in Valinor during the Years of the Trees. She took part in the rebellion of the Noldor and crossed the Helcaraxë with Fingolfin''s host, though later writings maintain she took no part in the Kinslaying at Alqualondë and left Valinor opposed to Fëanor. In Middle-earth she dwelt in Lindon, Eregion, and finally Lothlórien with Celeborn.',
   'sha256:galadriel'),
  ('tolkien-gateway', 'Fëanor', 'https://tolkiengateway.net/wiki/Fëanor',
   '2026-09-01T10:00:00Z', 'CC BY-SA 4.0', 'oldid=123457',
   'Fëanor was the eldest son of Finwë, maker of the Silmarils in Valinor. After their theft he led the rebellion of the Noldor, seized the ships of the Teleri at Alqualondë, and was slain by Morgoth in Middle-earth.',
   'sha256:feanor');

INSERT INTO entities (id, kind, canonical_name) VALUES
  ('finwe', 'person', 'Finwë');

INSERT INTO relationship_types (rel_type, category, description) VALUES
  ('father_of', 'genealogy', 'The subject is the father of the object.');

INSERT INTO relationship_type_aliases (alias, rel_type) VALUES
  ('parent_of', 'father_of'),
  ('father-of', 'father_of');`

// The model's reply for the Galadriel unit: a realistic mix of kept
// records and every headline drop path.
const galadrielPayload = `{
  "entities": [
    {"id": "galadriel", "kind": "person", "canonical_name": "Galadriel",
     "names": [{"name": "Altáriel", "name_kind": "translation", "language": "Quenya"},
               {"name": "Nerwen", "name_kind": "epithet"}],
     "attributes": {}},
    {"id": "fingolfin", "kind": "person", "canonical_name": "Fingolfin", "names": [], "attributes": {}},
    {"id": "feanor", "kind": "person", "canonical_name": "Fëanor", "names": [], "attributes": {}},
    {"id": "noldor", "kind": "people", "canonical_name": "Noldor", "names": [], "attributes": {}},
    {"id": "helcaraxe", "kind": "place", "canonical_name": "Helcaraxë", "names": [], "attributes": {}}
  ],
  "events": [
    {"id": "crossing-of-the-helcaraxe", "kind": "migration",
     "summary": "Fingolfin's host, including Galadriel, crosses the Grinding Ice into Middle-earth.",
     "description": "After Fëanor's ships were withheld, the remaining Noldor crossed the Helcaraxë.",
     "dating": {"label": "Y.T. 1495–1500"},
     "location_entity_id": "helcaraxe",
     "participants": {"fingolfin": "leader", "galadriel": "participant", "noldor": "party"},
     "causes": ["kinslaying-at-alqualonde"],
     "consequences": [],
     "sources": [
       {"source_id": "tolkien-gateway", "locator": {"url": "https://tolkiengateway.net/wiki/Galadriel"}},
       {"source_id": "the-silmarillion", "locator": {"chapter": "Of the Flight of the Noldor"}}
     ]},
    {"id": "kinslaying-at-alqualonde", "kind": "battle",
     "summary": "Fëanor's host seizes the Teleri ships.",
     "sources": [
       {"source_id": "the-silmarillion", "locator": {"chapter": "Of the Flight of the Noldor"}}
     ]}
  ],
  "claims": [
    {"id": "galadriel-crossed-the-helcaraxe",
     "statement": "Galadriel crossed the Helcaraxë with Fingolfin's host.",
     "confidence": "established", "subject_entity_id": "galadriel", "predicate": "crossed",
     "object_entity_id": "helcaraxe",
     "sources": [
       {"source_id": "tolkien-gateway", "locator": {"url": "https://tolkiengateway.net/wiki/Galadriel"}},
       {"source_id": "the-silmarillion", "locator": {"chapter": "Of the Flight of the Noldor"}}
     ]},
    {"id": "galadriel-role-in-rebellion",
     "statement": "Galadriel's part in the rebellion of the Noldor.",
     "confidence": "contested", "subject_entity_id": "galadriel", "predicate": "role_in",
     "versions": [
       {"label": "published-silmarillion",
        "statement": "Galadriel was eager to leave Valinor and crossed the Helcaraxë with Fingolfin's host.",
        "sources": [
          {"source_id": "tolkien-gateway", "locator": {"url": "https://tolkiengateway.net/wiki/Galadriel"}},
          {"source_id": "the-silmarillion", "locator": {"chapter": "Of the Flight of the Noldor"}}
        ]},
       {"label": "later-writings",
        "statement": "Galadriel took no part in the Kinslaying and left Valinor opposed to Fëanor.",
        "sources": [
          {"source_id": "tolkien-gateway", "locator": {"url": "https://tolkiengateway.net/wiki/Galadriel"}},
          {"source_id": "unfinished-tales", "locator": {"part": "The History of Galadriel and Celeborn"}}
        ]}
     ],
     "sources": [
       {"source_id": "tolkien-gateway", "locator": {"url": "https://tolkiengateway.net/wiki/Galadriel"}},
       {"source_id": "the-silmarillion", "locator": {"chapter": "Of the Flight of the Noldor"}}
     ]},
    {"id": "galadriel-dwelt-in-lorien",
     "statement": "Galadriel dwelt in Lothlórien with Celeborn.",
     "confidence": "established", "subject_entity_id": "galadriel",
     "sources": [
       {"source_id": "tolkien-gateway", "locator": {"url": "https://tolkiengateway.net/wiki/Galadriel"}}
     ]},
    {"id": "galadriel-born-in-valinor",
     "statement": "Galadriel was born in Valinor.",
     "confidence": "established",
     "sources": []},
    {"id": "galadriel-saw-namo",
     "statement": "Galadriel received a warning from Mandos.",
     "confidence": "derived_probable",
     "sources": [
       {"source_id": "tolkien-gateway", "locator": {"url": "https://tolkiengateway.net/wiki/Galadriel"}},
       {"source_id": "unfinished-tales", "locator": {"chapter": "The History of Galadriel and Celeborn"}}
     ]},
    {"id": "galadriel-gift-of-foresight",
     "statement": "Galadriel was granted foresight beyond that of her kin.",
     "confidence": "contested", "subject_entity_id": "galadriel",
     "sources": [
       {"source_id": "tolkien-gateway", "locator": {"url": "https://tolkiengateway.net/wiki/Galadriel"}},
       {"source_id": "the-silmarillion", "locator": {"chapter": "Of the Flight of the Noldor"}}
     ]}
  ],
  "relationships": [
    {"from_entity_id": "galadriel", "to_entity_id": "noldor", "rel_type": "member_of"}
  ]
}`

const feanorPayload = `{
  "entities": [
    {"id": "feanor", "kind": "person", "canonical_name": "Fëanor", "names": [], "attributes": {}}
  ],
  "events": [],
  "claims": [
    {"id": "feanor-son-of-finwe",
     "statement": "Fëanor is the eldest son of Finwë.",
     "confidence": "established", "subject_entity_id": "feanor", "predicate": "son_of",
     "object_entity_id": "finwe",
     "sources": [
       {"source_id": "tolkien-gateway", "locator": {"url": "https://tolkiengateway.net/wiki/Fëanor"}},
       {"source_id": "the-silmarillion", "locator": {"chapter": "Of Fëanor"}}
     ]}
  ],
  "relationships": [
    {"from_entity_id": "finwe", "to_entity_id": "feanor", "rel_type": "father_of", "claim_id": "feanor-son-of-finwe"}
  ]
}`

// TestArdaPilotPortProof is the acceptance test for this stage: an
// Arda-shaped adapter config extracts the Arda pilot fixtures through the
// generic engine with no engine changes.
func TestArdaPilotPortProof(t *testing.T) {
	url := testdb.Acquire(t, "arda_port")
	if err := migrations.Up(url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), pilotSeed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	raw, err := os.ReadFile("testdata/arda_pilot.json")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Parse(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	adapter := loaded.(extract.Adapter)

	client := &promptRecorder{model: "reference-model"}
	stats, err := extract.Run(context.Background(), pool, adapter, client, extract.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}

	if stats.UnitsDone != 2 || stats.UnitsFailed != 0 || stats.UnitsSkipped != 0 {
		t.Fatalf("units wrong: %+v", stats)
	}
	if stats.Records["claims"] != 3 || stats.Records["events"] != 1 || stats.Records["contradictions"] != 1 {
		t.Fatalf("records wrong: %+v", stats.Records)
	}

	// The headline drop paths all fired.
	for _, reason := range []string{
		"event:missing_reference_citation",
		"claim:missing_primary_citation",
		"claim:uncited",
		"citation:bad_locator: locator for unfinished-tales missing required keys ['part']",
		"claim:contested_without_versions",
		"relationship:unknown rel_type 'member_of'",
		"event_link:dangling cause ref; dropped",
	} {
		if stats.DropLog[reason] == 0 {
			t.Fatalf("drop %q missing from log: %+v", reason, stats.DropLog)
		}
	}

	// The contested tradition registered with both versions cited.
	assertQuery(t, pool, 1, `SELECT count(*) FROM contradictions WHERE id = 'galadriel-role-in-rebellion-tradition' AND status = 'open'`)
	assertQuery(t, pool, 1, `SELECT count(*) FROM contradiction_claims WHERE claim_id = 'galadriel-role-in-rebellion'`)
	assertQuery(t, pool, 2, `SELECT count(*) FROM claim_versions cv WHERE cv.claim_id = 'galadriel-role-in-rebellion'`)
	assertQuery(t, pool, 1, `SELECT count(*) FROM claim_sources cs
		JOIN claim_versions cv ON cv.id = cs.version_id
		WHERE cs.source_id = 'unfinished-tales' AND cs.locator->>'part' = 'The History of Galadriel and Celeborn'`)

	// The kept event carries both tiers with a valid locator.
	assertQuery(t, pool, 1, `SELECT count(*) FROM events e
		JOIN event_sources es ON es.event_id = e.id
		WHERE e.id = 'crossing-of-the-helcaraxe' AND es.source_id = 'the-silmarillion'
		  AND es.locator->>'chapter' = 'Of the Flight of the Noldor'`)

	// Dropped records never landed.
	assertQuery(t, pool, 0, `SELECT count(*) FROM claims WHERE id IN
		('galadriel-dwelt-in-lorien', 'galadriel-born-in-valinor', 'galadriel-saw-namo',
		 'galadriel-gift-of-foresight')`)
	assertQuery(t, pool, 0, `SELECT count(*) FROM events WHERE id = 'kinslaying-at-alqualonde'`)
	assertQuery(t, pool, 0, `SELECT count(*) FROM relationships WHERE rel_type = 'member_of' OR to_entity_id = 'noldor'`)

	// The relationship from the Fëanor unit landed (known entity ends).
	assertQuery(t, pool, 1, `SELECT count(*) FROM relationships r
		JOIN claims c ON c.id = r.claim_id
		WHERE r.from_entity_id = 'finwe' AND r.to_entity_id = 'feanor' AND r.rel_type = 'father_of'`)

	// The ledger recorded per-unit outcomes with structured drops.
	assertQuery(t, pool, 2, `SELECT count(*) FROM extraction_units WHERE status = 'done' AND prompt_version = 'extract-001'`)
	assertQuery(t, pool, 2, `SELECT count(*) FROM model_outputs WHERE record_kind = 'extraction' AND prompt_version = 'extract-001'`)
	assertQuery(t, pool, 1, `SELECT count(*) FROM pipeline_runs WHERE run_kind = 'extract' AND status = 'ok'`)

	// The prompt carried the adapter's citation surface, not engine
	// knowledge of the domain.
	var galadrielPrompt string
	for _, prompt := range client.prompts {
		if strings.Contains(prompt, "https://tolkiengateway.net/wiki/Galadriel") {
			galadrielPrompt = prompt
		}
	}
	if galadrielPrompt == "" {
		t.Fatal("no prompt for the Galadriel unit")
	}
	for _, needle := range []string{
		"https://tolkiengateway.net/wiki/Galadriel",
		"the-silmarillion",
		"Of the Flight of the Noldor",
		"The History of Galadriel and Celeborn",
		"finwe",
		"father_of",
	} {
		if !strings.Contains(galadrielPrompt, needle) {
			t.Fatalf("prompt missing %q", needle)
		}
	}

	// The graph the run produced is internally clean.
	results, err := integrity.Run(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if n := integrity.ErrorCount(results); n != 0 {
		t.Fatalf("integrity errors after extraction: %d", n)
	}
	if n := integrity.WarningCount(results); n != 0 {
		t.Fatalf("integrity warnings after extraction: %d", n)
	}

	// Re-running is a full skip: the corpus is stable under its contract.
	stats2, err := extract.Run(context.Background(), pool, adapter, client, extract.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if stats2.UnitsSkipped != 2 || stats2.UnitsDone != 0 {
		t.Fatalf("re-run must skip: %+v", stats2)
	}
}

type promptRecorder struct {
	model   string
	prompts []string
}

func (p *promptRecorder) Model() string { return p.model }

func (p *promptRecorder) Complete(_ context.Context, system, user string) (llm.Response, error) {
	p.prompts = append(p.prompts, system+"\n"+user)
	payload := feanorPayload
	if strings.Contains(user, "https://tolkiengateway.net/wiki/Galadriel") {
		payload = galadrielPayload
	}
	return llm.Response{
		Text: payload, Model: p.model, InputTokens: 4200, OutputTokens: 310,
		FinishReason: "stop", LatencySeconds: 0.5,
	}, nil
}

func assertQuery(t *testing.T, pool *pgxpool.Pool, want int, query string) {
	t.Helper()
	var got int
	if err := pool.QueryRow(context.Background(), query).Scan(&got); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	if got != want {
		t.Fatalf("query %q returned %d, want %d", query, got, want)
	}
}
