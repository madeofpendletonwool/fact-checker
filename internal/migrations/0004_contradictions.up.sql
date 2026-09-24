-- 0004: the contradiction register.
--
-- Ported unchanged in spirit from Arda: disagreements between credible
-- sources are preserved as first-class data, never averaged away or
-- auto-resolved. Living claims involved in a contradiction are downgraded to
-- 'contested' by the validation stage (stage 2); resolution happens only
-- through human review.

CREATE TABLE contradictions (
    id          TEXT PRIMARY KEY,             -- slug
    title       TEXT NOT NULL,
    description TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'open'
                CHECK (status IN ('open', 'resolved_by_review')),
    resolution  TEXT,                         -- human review outcome; never
                                           -- set by a machine pass
    resolved_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (status = 'open' OR resolution IS NOT NULL)
);

CREATE TABLE contradiction_claims (
    contradiction_id TEXT NOT NULL REFERENCES contradictions (id) ON DELETE CASCADE,
    claim_id         TEXT NOT NULL REFERENCES claims (id) ON DELETE CASCADE,
    version_id       BIGINT,                  -- optional pin to a specific
                                           -- claim_versions row
    tradition_label  TEXT,                    -- e.g. 'Published Silmarillion'
    UNIQUE NULLS NOT DISTINCT (contradiction_id, claim_id, version_id),
                                           -- one row per whole claim, plus one
                                           -- per version pin: a single contested
                                           -- claim links all its traditions
    FOREIGN KEY (version_id, claim_id)
        REFERENCES claim_versions (id, claim_id) ON DELETE CASCADE
);

CREATE INDEX idx_contradictions_status ON contradictions (status);

COMMENT ON TABLE contradictions IS
    'Register of preserved disagreements. Registration downgrades the living '
    'versions to contested and never resolves to a winner; only a human '
    'review decision sets status/resolution.';
