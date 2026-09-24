-- 0005: audited statements — the content the engine fact-checks.
--
-- Generic port of Arda's paragraph IR. In Arda the audited unit was a book
-- paragraph; in the generic engine it is whatever the adapter declares a
-- statement to be (a narrative paragraph, a doc sentence, a rendered row).
-- Statements are the *audited narrative* side of the pipeline: they carry
-- their own provenance, and the fact-check stage (stage 3) re-checks each
-- one against exactly that provenance. Under the docs-drift inversion the
-- documentation itself loads as statements.

CREATE TABLE statements (
    id                     BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    scope_kind             TEXT NOT NULL,     -- adapter-defined grouping,
                                           -- e.g. 'chapter', 'doc_page'
    scope_id               TEXT NOT NULL,     -- slug within that grouping
    ordinal                INTEGER NOT NULL,
    content                TEXT NOT NULL,     -- markdown
    checksum               TEXT NOT NULL,     -- sha256 of content; the
                                           -- fact-check ledger keys on it
    interpretive_additions TEXT,              -- NULL = none; declared
                                           -- interpretive additions otherwise
    attributes             JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (scope_kind, scope_id, ordinal)
);

CREATE INDEX idx_statements_scope ON statements (scope_kind, scope_id);

CREATE TABLE statement_provenance (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    statement_id BIGINT NOT NULL REFERENCES statements (id) ON DELETE CASCADE,
    claim_id     TEXT REFERENCES claims (id),
    event_id     TEXT REFERENCES events (id),
    source_id    TEXT REFERENCES sources (id),
    locator      JSONB,
    confidence   TEXT CHECK (confidence IN ('established', 'derived_probable',
                                            'contested', 'abandoned_version')),
                                           -- optional per-row confidence;
                                           -- statement-level indicator =
                                           -- worst confidence among its rows
    CHECK (claim_id IS NOT NULL OR event_id IS NOT NULL OR source_id IS NOT NULL),
    UNIQUE NULLS NOT DISTINCT (statement_id, claim_id, event_id, source_id)
);

CREATE INDEX idx_statement_provenance_claim ON statement_provenance (claim_id);
CREATE INDEX idx_statement_provenance_event ON statement_provenance (event_id);

COMMENT ON TABLE statements IS
    'Audited content units with provenance. A statement with neither '
    'provenance rows nor a declared interpretive_additions note fails the '
    'integrity check statements_without_provenance.';
