-- 0006: pipeline ledgers and audit.
--
-- The tables that make every stage idempotent, resumable, and auditable:
-- run stats, verbatim model outputs, per-work-unit extraction state, verdict
-- ledgers keyed on prompt version (+ content checksum for fact-check), flag
-- tables whose human decisions survive re-runs, and the review queue.
-- Ported from Arda migrations 0001/0003/0004/0008, generalized.

CREATE TABLE pipeline_runs (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_kind    TEXT NOT NULL
                CHECK (run_kind IN ('ingest', 'extract', 'validate',
                                    'factcheck', 'review', 'integrity_check',
                                    'seed')),
    status      TEXT NOT NULL DEFAULT 'running'
                CHECK (status IN ('running', 'ok', 'failed')),
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    parameters  JSONB NOT NULL DEFAULT '{}'::jsonb,
    stats       JSONB NOT NULL DEFAULT '{}'::jsonb,
    notes       TEXT
);

CREATE INDEX idx_pipeline_runs_kind ON pipeline_runs (run_kind, status);

-- Raw model responses, verbatim, with usage so cost reporting never has to
-- re-parse them. parsed_ref links a response to the record extracted from it.
CREATE TABLE model_outputs (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id         BIGINT NOT NULL REFERENCES pipeline_runs (id) ON DELETE CASCADE,
    record_kind    TEXT NOT NULL,             -- e.g. 'claim', 'event',
                                           -- 'validation_verdict'
    prompt_version TEXT,
    model          TEXT,
    input_ref      TEXT,                      -- what was sent in (e.g. document id)
    raw_output     JSONB NOT NULL,            -- verbatim model response
    parsed_ref     TEXT,                      -- id of the record parsed from it
    input_tokens   INTEGER,
    output_tokens  INTEGER,
    cost_usd       NUMERIC,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_model_outputs_run ON model_outputs (run_id);

-- Per-work-unit extraction state: idempotent, resumable batch runs. A unit
-- is the adapter's natural chunk of one document; (unit_key, prompt_version)
-- is the resume key. input_hash detects input drift so a done unit is
-- re-extracted rather than silently stale. Drop reasons are structured data.
CREATE TABLE extraction_units (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    raw_document_id  BIGINT NOT NULL REFERENCES raw_documents (id) ON DELETE CASCADE,
    unit_key         TEXT NOT NULL,           -- stable per source, e.g.
                                           -- 'tolkien-gateway:galadriel'
    prompt_version   TEXT NOT NULL,
    model            TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending', 'done', 'failed')),
    attempts         SMALLINT NOT NULL DEFAULT 0,
    last_error       TEXT,
    input_hash       TEXT NOT NULL,
    records          JSONB NOT NULL DEFAULT '{}'::jsonb,
    dropped          JSONB NOT NULL DEFAULT '[]'::jsonb,
    run_id           BIGINT REFERENCES pipeline_runs (id),
    completed_at     TIMESTAMPTZ,
    UNIQUE (unit_key, prompt_version)
);

CREATE INDEX idx_extraction_units_status ON extraction_units (status);
CREATE INDEX idx_extraction_units_document ON extraction_units (raw_document_id);

-- Validation verdict ledger: adversarial second model, keyed by claim +
-- prompt version so batches are idempotent. target_confidence is set only
-- for 'downgrade' and may only move DOWN the confidence order.
CREATE TABLE validation_verdicts (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id            BIGINT NOT NULL REFERENCES pipeline_runs (id) ON DELETE CASCADE,
    claim_id          TEXT NOT NULL REFERENCES claims (id) ON DELETE CASCADE,
    prompt_version    TEXT NOT NULL,
    model             TEXT NOT NULL,
    verdict           TEXT NOT NULL
                      CHECK (verdict IN ('agree', 'downgrade', 'flag_review')),
    agreement         NUMERIC NOT NULL CHECK (agreement >= 0 AND agreement <= 1),
    target_confidence TEXT CHECK (target_confidence IN
                      ('established', 'derived_probable', 'contested',
                       'abandoned_version')),
    applied           BOOLEAN NOT NULL DEFAULT FALSE,
    rationale         TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (claim_id, prompt_version)
);

CREATE INDEX idx_validation_verdicts_claim ON validation_verdicts (claim_id);
CREATE INDEX idx_validation_verdicts_run ON validation_verdicts (run_id);

-- Deterministic-check findings. Keyed by (check_code, record_kind,
-- record_id): re-runs refresh the finding but never clobber a human
-- decision; findings the engine stops reporting are marked 'cleared'.
CREATE TABLE validation_flags (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id        BIGINT NOT NULL REFERENCES pipeline_runs (id) ON DELETE CASCADE,
    check_code    TEXT NOT NULL,              -- e.g. 'predicate_value_conflict'
    record_kind   TEXT NOT NULL,              -- 'claim' | 'event' | 'entity' |
                                           -- 'relationship' | 'citation' | adapter kinds
    record_id     TEXT NOT NULL,
    severity      TEXT NOT NULL CHECK (severity IN ('error', 'warning', 'review')),
    message       TEXT NOT NULL,
    details       JSONB NOT NULL DEFAULT '{}'::jsonb,
    review_status TEXT NOT NULL DEFAULT 'open'
                  CHECK (review_status IN ('open', 'accepted', 'dismissed', 'cleared')),
    reviewed_at   TIMESTAMPTZ,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (check_code, record_kind, record_id)
);

CREATE INDEX idx_validation_flags_status ON validation_flags (review_status);
CREATE INDEX idx_validation_flags_run ON validation_flags (run_id);

-- Fact-check verdict ledger: one row per (statement, prompt version, text
-- checksum, revision round). An unchanged settled statement is never billed
-- twice; a rewritten statement is re-checked as new content.
CREATE TABLE factcheck_verdicts (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id         BIGINT REFERENCES pipeline_runs (id) ON DELETE SET NULL,
    statement_id   BIGINT NOT NULL REFERENCES statements (id) ON DELETE CASCADE,
    prompt_version TEXT NOT NULL,
    model          TEXT NOT NULL,
    checksum       TEXT NOT NULL,             -- statement content checksum at
                                           -- check time
    verdict        TEXT NOT NULL
                   CHECK (verdict IN ('pass', 'fail', 'flag_review')),
    agreement      REAL,
    unsupported    JSONB NOT NULL DEFAULT '[]'::jsonb,
                                           -- [{quote, reason}] — statements
                                           -- the provenance does not entail
    rationale      TEXT,
    revision_round SMALLINT NOT NULL DEFAULT 0,
    revised        BOOLEAN NOT NULL DEFAULT FALSE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (statement_id, prompt_version, checksum, revision_round)
);

CREATE INDEX idx_factcheck_verdicts_statement
    ON factcheck_verdicts (statement_id, prompt_version);

-- Fact-check spot-check flags: same lifecycle contract as validation_flags.
CREATE TABLE factcheck_flags (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id        BIGINT REFERENCES pipeline_runs (id) ON DELETE SET NULL,
    check_code    TEXT NOT NULL,
    record_kind   TEXT NOT NULL DEFAULT 'statement',
    record_id     TEXT NOT NULL,              -- statement id as text
    severity      TEXT NOT NULL CHECK (severity IN ('error', 'warning', 'review')),
    message       TEXT NOT NULL,
    details       JSONB NOT NULL DEFAULT '{}'::jsonb,
    review_status TEXT NOT NULL DEFAULT 'open'
                  CHECK (review_status IN ('open', 'accepted', 'dismissed', 'cleared')),
    decided_note  TEXT,
    reviewed_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (check_code, record_kind, record_id)
);

CREATE INDEX idx_factcheck_flags_open ON factcheck_flags (review_status);

-- The human review queue: material only a person can settle. One open item
-- per (kind, statement); a decided item that reappears opens a new row
-- instead of resurrecting the old decision.
CREATE TABLE review_items (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id           BIGINT REFERENCES pipeline_runs (id) ON DELETE SET NULL,
    kind             TEXT NOT NULL
                     CHECK (kind IN ('entailment_failure', 'interpretive_addition',
                                     'contested_as_settled', 'model_disagreement')),
    statement_id     BIGINT NOT NULL REFERENCES statements (id) ON DELETE CASCADE,
    title            TEXT NOT NULL,           -- one line, for the queue listing
    detail           TEXT NOT NULL,           -- what the reader would see
    suggested_action TEXT NOT NULL,           -- what a human might decide
    status           TEXT NOT NULL DEFAULT 'open'
                     CHECK (status IN ('open', 'accepted', 'dismissed')),
    decision_note    TEXT,
    decided_at       TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_review_items_open
    ON review_items (kind, statement_id) WHERE status = 'open';

CREATE INDEX idx_review_items_status ON review_items (status);

COMMENT ON TABLE review_items IS
    'Human review queue. Only open items can be decided; a decided item '
    'keeps its decision forever, and the same finding reappearing opens a '
    'new item rather than resurrecting the old one.';
