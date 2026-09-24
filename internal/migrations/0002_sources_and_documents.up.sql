-- 0002: source registry and the raw document store.
--
-- Ported from the Arda pipeline's lore-graph core, domain-stripped: what a
-- tier means and what a valid locator looks like are adapter-owned data
-- carried in these rows, not engine schema. The engine fixes only the
-- shape: tiers are small integers (1 = highest authority), and every
-- citation must satisfy the cited source's locator_scheme.

CREATE TABLE sources (
    id             TEXT PRIMARY KEY,          -- slug, e.g. 'the-silmarillion'
    tier           SMALLINT NOT NULL CHECK (tier >= 1),
                                           -- authority level; meaning defined
                                           -- by the adapter (docs-drift: live
                                           -- infrastructure is tier 1)
    title          TEXT NOT NULL,
    citation_form  TEXT,                      -- canonical short citation
    license        TEXT,                      -- e.g. 'CC BY-SA 4.0'
    locator_scheme JSONB NOT NULL DEFAULT '{}'::jsonb,
                                           -- per-source citation-locator
                                           -- grammar, e.g.
                                           -- {"required_keys": ["chapter"],
                                           --  "optional_keys": ["book", "section"]}
                                           -- other schemes: ["url"],
                                           -- ["file", "commit"], ["url", "char_start", "char_end"]
    notes          JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE raw_documents (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_id     TEXT NOT NULL REFERENCES sources (id),
    title         TEXT,
    url           TEXT NOT NULL,              -- URI: http(s) URL or file path
    fetched_at    TIMESTAMPTZ NOT NULL,
    license       TEXT,                       -- license at fetch time
    revision      TEXT,                       -- e.g. MediaWiki revision id, git commit SHA
    content_text  TEXT,                       -- extracted plain text
    content_hash  TEXT,                       -- dedupe key over content_text
    metadata      JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE NULLS NOT DISTINCT (url, revision) -- idempotent re-ingest;
                                               -- revisions append, never overwrite
);

CREATE INDEX idx_raw_documents_source ON raw_documents (source_id);
CREATE INDEX idx_raw_documents_hash ON raw_documents (content_hash);

COMMENT ON TABLE sources IS
    'Tiered source registry. Tier semantics and locator schemes are '
    'adapter-owned data; the engine only requires citations to satisfy '
    'locator_scheme.required_keys (integrity check invalid_locators).';
