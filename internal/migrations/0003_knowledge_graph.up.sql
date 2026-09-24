-- 0003: the knowledge graph — entities, events, claims, relationships.
--
-- Ported from the Arda lore-graph core with the ontology stripped out of the
-- tables: entity kinds, event kinds, event link kinds, and relationship
-- categories are free text whose vocabularies live in adapter configuration.
-- Engine-fixed semantics only: claims carry the four-level confidence order,
-- citations carry locators, and the subject/predicate/object triple keeps its
-- XOR shape for deterministic conflict detection (stage 2).

-- ============================================================ entities

CREATE TABLE entities (
    id             TEXT PRIMARY KEY,          -- slug, e.g. 'feanor'
    kind           TEXT NOT NULL,             -- adapter ontology, e.g. 'person',
                                           -- 'service', 'repository'
    canonical_name TEXT NOT NULL,
    attributes     JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_entities_kind ON entities (kind);
CREATE INDEX idx_entities_attributes ON entities USING gin (attributes);

CREATE TABLE entity_names (
    entity_id TEXT NOT NULL REFERENCES entities (id) ON DELETE CASCADE,
    name      TEXT NOT NULL,                  -- variant / alias / translation
    name_kind TEXT NOT NULL DEFAULT 'variant'
              CHECK (name_kind IN ('variant', 'alias', 'translation', 'other')),
    language  TEXT,                           -- e.g. 'Quenya', 'en-US'
    PRIMARY KEY (entity_id, name)
);

CREATE INDEX idx_entity_names_name ON entity_names (name);

-- ============================================================ events

CREATE TABLE events (
    id                 TEXT PRIMARY KEY,      -- slug, e.g. 'first_kinslaying'
    kind               TEXT,                  -- adapter ontology
    summary            TEXT NOT NULL,
    description        TEXT,
    location_entity_id TEXT REFERENCES entities (id),
    attributes         JSONB NOT NULL DEFAULT '{}'::jsonb,
                                           -- structured dating is adapter-
                                           -- defined; the convention is
                                           -- attributes.dating (see docs/schema)
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_events_kind ON events (kind);

CREATE TABLE event_participants (
    event_id  TEXT NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    entity_id TEXT NOT NULL REFERENCES entities (id) ON DELETE CASCADE,
    role      TEXT NOT NULL DEFAULT 'participant',
    PRIMARY KEY (event_id, entity_id)
);

CREATE TABLE event_links (
    from_event_id TEXT NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    to_event_id   TEXT NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    link_kind     TEXT NOT NULL,              -- adapter vocabulary, e.g.
                                           -- 'cause', 'part_of', 'precedes'
    PRIMARY KEY (from_event_id, to_event_id, link_kind),
    CHECK (from_event_id <> to_event_id)
);

CREATE TABLE event_sources (
    event_id  TEXT NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    source_id TEXT NOT NULL REFERENCES sources (id),
    locator   JSONB,
    note      TEXT,
    PRIMARY KEY (event_id, source_id)
);

CREATE INDEX idx_event_sources_source ON event_sources (source_id);

-- ============================================================ claims

CREATE TABLE claims (
    id                TEXT PRIMARY KEY,       -- slug
    statement         TEXT NOT NULL,          -- atomic statement of fact
    subject_entity_id TEXT REFERENCES entities (id),
    predicate         TEXT,                   -- machine-checkable predicate
    object_entity_id  TEXT REFERENCES entities (id),
    object_value      TEXT,
    confidence        TEXT NOT NULL
                      CHECK (confidence IN ('established', 'derived_probable',
                                            'contested', 'abandoned_version')),
                                           -- four-level order is engine-fixed;
                                           -- display names are adapter config
    notes             JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (object_entity_id IS NULL OR object_value IS NULL)
);

CREATE INDEX idx_claims_confidence ON claims (confidence);
CREATE INDEX idx_claims_subject ON claims (subject_entity_id);
CREATE INDEX idx_claims_predicate ON claims (subject_entity_id, predicate);

COMMENT ON TABLE claims IS
    'Atomic, cited statements of fact. Every claim must carry at least one '
    'claim_sources row (integrity check claims_without_sources); the LLM '
    'extracts and cites, it never decides what is true.';

-- Per-tradition versions of a contested claim: one row per source tradition,
-- each with its own pinned citations, so traditions stay separable end to end.
CREATE TABLE claim_versions (
    id        BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    claim_id  TEXT NOT NULL REFERENCES claims (id) ON DELETE CASCADE,
    label     TEXT NOT NULL,                  -- e.g. 'published-silmarillion'
    statement TEXT NOT NULL,                  -- this tradition's phrasing
    UNIQUE (claim_id, label)
);

CREATE UNIQUE INDEX uq_claim_versions_id_claim ON claim_versions (id, claim_id);

CREATE TABLE claim_sources (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    claim_id   TEXT NOT NULL REFERENCES claims (id) ON DELETE CASCADE,
    version_id BIGINT,                        -- NULL = supports the claim as a
                                           -- whole; set = supports this
                                           -- version of a contested claim
    source_id  TEXT NOT NULL REFERENCES sources (id),
    locator    JSONB,
    note       TEXT,
    UNIQUE NULLS NOT DISTINCT (claim_id, version_id, source_id),
    FOREIGN KEY (version_id, claim_id)
        REFERENCES claim_versions (id, claim_id) ON DELETE CASCADE
);

CREATE INDEX idx_claim_sources_source ON claim_sources (source_id);
CREATE INDEX idx_claim_sources_version ON claim_sources (version_id);

-- ============================================================ relationships

CREATE TABLE relationship_types (
    rel_type    TEXT PRIMARY KEY,             -- e.g. 'father_of', 'depends_on'
    category    TEXT NOT NULL,                -- adapter vocabulary, e.g.
                                           -- 'genealogy', 'deployment'
    description TEXT NOT NULL,
    inverse_of  TEXT REFERENCES relationship_types (rel_type)
);

CREATE TABLE relationship_type_aliases (
    alias    TEXT PRIMARY KEY,                -- surface forms normalized at
    rel_type TEXT NOT NULL REFERENCES relationship_types (rel_type) ON DELETE CASCADE
);

CREATE INDEX idx_relationship_type_aliases_type ON relationship_type_aliases (rel_type);

CREATE TABLE relationships (
    from_entity_id TEXT NOT NULL REFERENCES entities (id) ON DELETE CASCADE,
    to_entity_id   TEXT NOT NULL REFERENCES entities (id) ON DELETE CASCADE,
    rel_type       TEXT NOT NULL REFERENCES relationship_types (rel_type),
    claim_id       TEXT REFERENCES claims (id), -- the claim that justifies this edge
    attributes     JSONB NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (from_entity_id, to_entity_id, rel_type),
    CHECK (from_entity_id <> to_entity_id)
);

CREATE INDEX idx_relationships_to ON relationships (to_entity_id);
CREATE INDEX idx_relationships_claim ON relationships (claim_id);
