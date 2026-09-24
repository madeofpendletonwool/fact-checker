-- The Arda corpus shape as a test fixture (MAD-456 acceptance).
--
-- Loads against the generic core schema with no schema changes: the Tolkien
-- ontology (entity kinds, relationship categories, locator schemes, tier
-- meanings) rides entirely in row data. A Silmarillion-vs-later-writings
-- divergence is present, so the contradiction machinery carries real weight.
--
-- Loads clean: integrity checks report 0 errors and 0 warnings.

-- ============================================================ sources

INSERT INTO sources (id, tier, title, citation_form, license, locator_scheme) VALUES
    ('the-silmarillion', 1, 'The Silmarillion', 'Silmarillion', NULL,
     '{"required_keys": ["chapter"], "optional_keys": ["book", "section"]}'::jsonb),
    ('unfinished-tales', 1, 'Unfinished Tales of Númenor and Middle-earth',
     'Unfinished Tales', NULL,
     '{"required_keys": ["part"], "optional_keys": ["chapter"]}'::jsonb),
    ('tolkien-gateway', 3, 'Tolkien Gateway', 'Tolkien Gateway', 'CC BY-SA 4.0',
     '{"required_keys": ["url"]}'::jsonb);

-- ============================================================ documents

INSERT INTO raw_documents (source_id, title, url, fetched_at, license, revision,
                           content_text, content_hash) VALUES
    ('tolkien-gateway', 'Galadriel',
     'https://tolkiengateway.net/wiki/Galadriel', '2026-09-01T10:00:00Z',
     'CC BY-SA 4.0', 'oldid=123456',
     'Galadriel was a Noldorin princess…', 'sha256:2f1d0b…c9');

-- ============================================================ entities

INSERT INTO entities (id, kind, canonical_name) VALUES
    ('galadriel', 'person', 'Galadriel'),
    ('feanor', 'person', 'Fëanor'),
    ('finwe', 'person', 'Finwë'),
    ('noldor', 'people', 'Noldor'),
    ('valinor', 'place', 'Valinor'),
    ('helcaraxe', 'place', 'Helcaraxë'),
    ('alqualonde', 'place', 'Alqualondë');

INSERT INTO entity_names (entity_id, name, name_kind, language) VALUES
    ('galadriel', 'Altáriel', 'translation', 'Quenya'),
    ('galadriel', 'Nerwen', 'variant', 'Quenya'),
    ('feanor', 'Curufinwë', 'variant', 'Quenya'),
    ('feanor', 'Fëanáro', 'translation', 'Quenya');

-- ============================================================ events

INSERT INTO events (id, kind, summary, location_entity_id, attributes) VALUES
    ('first-kinslaying', 'battle',
     'Fëanor''s host seizes the ships of the Teleri at Alqualondë.',
     'alqualonde',
     '{"dating": {"label": "Y.T. 1495"}}'::jsonb),
    ('crossing-of-the-helcaraxe', 'migration',
     'Fingolfin''s host crosses the Grinding Ice into Middle-earth.',
     'helcaraxe',
     '{"dating": {"label": "Y.T. 1495–1500"}}'::jsonb);

INSERT INTO event_participants (event_id, entity_id, role) VALUES
    ('first-kinslaying', 'feanor', 'leader'),
    ('first-kinslaying', 'noldor', 'aggressor'),
    ('crossing-of-the-helcaraxe', 'galadriel', 'participant');

INSERT INTO event_links (from_event_id, to_event_id, link_kind) VALUES
    ('first-kinslaying', 'crossing-of-the-helcaraxe', 'cause');

INSERT INTO event_sources (event_id, source_id, locator) VALUES
    ('first-kinslaying', 'the-silmarillion',
     '{"chapter": "Of the Flight of the Noldor"}'::jsonb),
    ('crossing-of-the-helcaraxe', 'the-silmarillion',
     '{"chapter": "Of the Flight of the Noldor"}'::jsonb);

-- ============================================================ claims

INSERT INTO claims (id, statement, subject_entity_id, predicate,
                    object_entity_id, confidence) VALUES
    ('feanor-son-of-finwe', 'Fëanor is a son of Finwë.',
     'feanor', 'son_of', 'finwe', 'established'),
    ('galadriel-role-in-rebellion',
     'Galadriel''s part in the rebellion of the Noldor.',
     'galadriel', 'role_in', NULL, 'contested');

INSERT INTO claim_versions (id, claim_id, label, statement) OVERRIDING SYSTEM VALUE VALUES
    (1, 'galadriel-role-in-rebellion', 'published-silmarillion',
     'Galadriel was eager to leave Valinor and crossed the Helcaraxë with Fingolfin''s host.'),
    (2, 'galadriel-role-in-rebellion', 'later-writings',
     'Galadriel took no part in the Kinslaying and left Valinor opposed to Fëanor.');

INSERT INTO claim_sources (claim_id, version_id, source_id, locator) VALUES
    ('feanor-son-of-finwe', NULL, 'the-silmarillion',
     '{"chapter": "Of Fëanor"}'::jsonb),
    ('galadriel-role-in-rebellion', 1, 'the-silmarillion',
     '{"chapter": "Of the Flight of the Noldor"}'::jsonb),
    ('galadriel-role-in-rebellion', 2, 'unfinished-tales',
     '{"part": "The History of Galadriel and Celeborn"}'::jsonb);

-- ============================================================ relationships

INSERT INTO relationship_types (rel_type, category, description, inverse_of) VALUES
    ('father_of', 'genealogy', 'The subject is the father of the object.', NULL);

INSERT INTO relationship_type_aliases (alias, rel_type) VALUES
    ('parent_of', 'father_of'),
    ('father-of', 'father_of');

INSERT INTO relationships (from_entity_id, to_entity_id, rel_type, claim_id) VALUES
    ('finwe', 'feanor', 'father_of', 'feanor-son-of-finwe');

-- ============================================================ contradiction register

INSERT INTO contradictions (id, title, description) VALUES
    ('galadriel-rebellion-traditions',
     'Galadriel''s role in the rebellion of the Noldor',
     'The published Silmarillion presents Galadriel as an eager participant in the '
     'rebellion; later writings state she took no part in the Kinslaying and left '
     'Valinor opposed to Fëanor. Both traditions are preserved.');

INSERT INTO contradiction_claims (contradiction_id, claim_id, version_id, tradition_label) VALUES
    ('galadriel-rebellion-traditions', 'galadriel-role-in-rebellion', 1,
     'Published Silmarillion'),
    ('galadriel-rebellion-traditions', 'galadriel-role-in-rebellion', 2,
     'Later writings');

-- ============================================================ audited statements

INSERT INTO statements (scope_kind, scope_id, ordinal, content, checksum) VALUES
    ('chapter', 'ch-09-the-flight-of-the-noldor', 1,
     'Galadriel crossed the Helcaraxë with Fingolfin''s host; Fëanor, Finwë''s son, led the ships that abandoned them.',
     '352ff6fd9a61c7f2bf0224dc9c989170588245d80c5b1aa2a0b918ea70f5e659');

INSERT INTO statement_provenance (statement_id, claim_id, event_id, source_id,
                                  locator, confidence)
SELECT st.id, c.id, NULL, NULL, NULL, c.confidence
FROM statements st
JOIN claims c ON c.id IN ('galadriel-role-in-rebellion', 'feanor-son-of-finwe')
WHERE st.scope_kind = 'chapter' AND st.scope_id = 'ch-09-the-flight-of-the-noldor';

INSERT INTO statement_provenance (statement_id, claim_id, event_id, source_id, locator)
SELECT st.id, NULL, 'crossing-of-the-helcaraxe', 'the-silmarillion',
       '{"chapter": "Of the Flight of the Noldor"}'::jsonb
FROM statements st
WHERE st.scope_kind = 'chapter' AND st.scope_id = 'ch-09-the-flight-of-the-noldor';

-- ============================================================ ledgers

INSERT INTO pipeline_runs (run_kind, status, started_at, finished_at, stats) VALUES
    ('ingest', 'ok', '2026-09-01T10:00:00Z', '2026-09-01T10:00:05Z',
     '{"documents": 1}'::jsonb),
    ('extract', 'ok', '2026-09-01T11:00:00Z', '2026-09-01T11:02:00Z',
     '{"units": 1, "records": 2, "dropped": 1}'::jsonb);

INSERT INTO model_outputs (run_id, record_kind, prompt_version, model, input_ref,
                           raw_output, parsed_ref, input_tokens, output_tokens, cost_usd)
SELECT pr.id, 'claim', 'extract-v1', 'reference-model', 'tolkien-gateway:galadriel',
     '{"claims": [{"statement": "Galadriel''s part in the rebellion of the Noldor.", "citations": [{"source": "the-silmarillion", "locator": {"chapter": "Of the Flight of the Noldor"}}]}]}'::jsonb,
     'galadriel-role-in-rebellion', 4200, 310, 0.0121
FROM pipeline_runs pr WHERE pr.run_kind = 'extract';

INSERT INTO extraction_units (raw_document_id, unit_key, prompt_version, model,
                              status, input_hash, records, dropped, run_id, completed_at)
SELECT rd.id, 'tolkien-gateway:galadriel', 'extract-v1', 'reference-model',
       'done', 'sha256:9af3…01',
       '{"claims": 2, "entities": 7}'::jsonb,
       '[{"reason": "uncited", "record": "event:0"}]'::jsonb,
       pr.id, '2026-09-01T11:01:30Z'
FROM raw_documents rd
CROSS JOIN (SELECT id FROM pipeline_runs WHERE run_kind = 'extract') pr
WHERE rd.source_id = 'tolkien-gateway';

INSERT INTO validation_verdicts (run_id, claim_id, prompt_version, model, verdict, agreement)
SELECT pr.id, 'feanor-son-of-finwe', 'validate-v1', 'reference-model', 'agree', 0.97
FROM pipeline_runs pr WHERE pr.run_kind = 'extract';

INSERT INTO factcheck_verdicts (run_id, statement_id, prompt_version, model, checksum,
                                verdict, agreement)
SELECT pr.id, st.id, 'factcheck-v1', 'reference-model', st.checksum, 'pass', 0.91
FROM statements st
CROSS JOIN (SELECT id FROM pipeline_runs WHERE run_kind = 'extract') pr
WHERE st.scope_kind = 'chapter' AND st.scope_id = 'ch-09-the-flight-of-the-noldor';
