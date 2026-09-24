# Table reference

Per-table notes for the core schema (migrations `0002_sources_and_documents`
through `0006_pipeline_ledgers`). Types are abbreviated; see the migration
SQL for exact constraints.

## Source registry and raw store

### `sources`
Tiered source registry, owned by the adapter's source-registry
configuration.

- `id` (slug PK), `tier` (SMALLINT ≥ 1 — meaning defined by the adapter;
  the docs-drift adapter makes live infrastructure tier 1), `title`
- `citation_form` — canonical short citation in prose
- `license` — e.g. `All rights reserved`, `CC BY-SA 4.0`
- `locator_scheme` (JSONB) — citation-locator grammar:
  `{"required_keys": ["chapter"], "optional_keys": ["book", "section"]}`.
  Citations against the source must include every required key (integrity
  check `invalid_locators`). Typical schemes:
  `["chapter"]`, `["url"]`, `["url","char_start","char_end"]`,
  `["file","line"]`, `["file","commit"]`
- `notes` (JSONB), `created_at`, `updated_at`

### `raw_documents`
Ingested material with full provenance. Revisions append, never overwrite.

- `source_id` → sources; `url` (URI — http(s) URL or file path),
  `fetched_at`, `license` (at fetch time), `revision` (e.g. MediaWiki
  revision id, git commit SHA), `title`
- `content_text`, `content_hash` (dedupe), `metadata` (JSONB)
- `UNIQUE NULLS NOT DISTINCT (url, revision)` — idempotent re-ingest

## Knowledge graph

### `entities`
Graph nodes; the ontology is adapter config.

- `id` (slug PK), `kind` (free text — adapter vocabulary, e.g. `person`,
  `service`), `canonical_name`
- `attributes` (JSONB, GIN-indexed) — flexible per-kind facts

### `entity_names`
Name variants for resolution and alias detection.

- (`entity_id`, `name`) PK; `name_kind` in
  `variant | alias | translation | other`; `language`

### `events`
- `id` (slug PK), `kind` (free text), `summary`, `description`
- `location_entity_id` → entities
- `attributes` (JSONB) — structured dating convention:
  `attributes.dating` (e.g. `{"label": "Y.T. 1495"}` or adapter-defined
  structured fields); no engine-level dating columns

### `event_participants`
(`event_id`, `entity_id`) PK + free-text `role`.

### `event_links`
Directed links between events: `link_kind` free text (adapter vocabulary,
e.g. `cause`, `part_of`, `precedes`). PK (from, to, kind); no self-links.

### `event_sources`
Event citations: (`event_id`, `source_id`) PK + `locator` JSONB + `note`.
Every event needs ≥1 row (integrity check `events_without_sources`).

### `claims`
Atomic, cited statements of fact.

- `id` (slug PK), `statement`
- Optional machine-checkable triple: `subject_entity_id`, `predicate`,
  `object_entity_id` XOR `object_value` — what deterministic conflict
  detection matches on (same predicate, conflicting objects, credible
  sources)
- `confidence` — four fixed codes, ordered; adapters remap display names
- `notes` (JSONB)

### `claim_versions`
Per-tradition versions of a contested claim, one row per source tradition.
(`claim_id`, `label`) unique; each version carries its own phrasing
(`statement`) and its own citations via `claim_sources.version_id`.

### `claim_sources`
Claim citations. `version_id` NULL = supports the claim as a whole; set =
supports that version of a contested claim (composite FK pins the version to
its claim). `UNIQUE NULLS NOT DISTINCT (claim_id, version_id, source_id)`.
Every claim needs ≥1 row (integrity check `claims_without_sources`).

### `relationship_types` / `relationship_type_aliases`
The edge vocabulary, owned by adapter config: `rel_type` PK, free-text
`category`, `description`, optional `inverse_of`. Aliases normalize surface
forms (`parent_of` → `father_of`) at load; authored reference data, never
inferred at runtime.

### `relationships`
Graph edges: (from, to, `rel_type`) PK; no self-edges; optional `claim_id`
— the claim that justifies this edge.

## Contradiction register

### `contradictions`
Register of preserved disagreements. `status` `open | resolved_by_review`;
`resolution`/`resolved_at` are human review outcomes, never set by a
machine pass.

### `contradiction_claims`
Links register entries to claims, optionally pinned to a specific
`claim_versions` row, with a `tradition_label`. One row per whole claim plus
one per version pin — a single contested claim links all of its traditions.
Registration downgrades the living versions to `contested` and never
resolves to a winner.

## Audited statements

### `statements`
The content the engine fact-checks — whatever the adapter declares a
statement to be (a narrative paragraph, a doc sentence). Under the
docs-drift inversion, documentation itself loads as statements.

- (`scope_kind`, `scope_id`) adapter-defined grouping (e.g. `chapter`,
  `doc_page`); `ordinal`; UNIQUE (scope_kind, scope_id, ordinal)
- `content`, `checksum` (sha256 of content — the fact-check ledger keys on
  it), `interpretive_additions` (NULL = none; declared additions
  otherwise), `attributes`

### `statement_provenance`
Provenance rows backing each statement: optional `claim_id`, `event_id`,
`source_id` + `locator`, per-row `confidence` (statement-level indicator =
worst among its rows). CHECK: cites at least one of claim/event/source.
Integrity check `statements_without_provenance`.

## Ledgers and audit

### `pipeline_runs`
One row per stage run: `run_kind`
(`ingest|extract|validate|factcheck|review|integrity_check|seed`),
`status` (`running|ok|failed`), `parameters`/`stats` (JSONB), timing.

### `model_outputs`
Raw model responses verbatim: `record_kind`, `prompt_version`, `model`,
`input_ref`, `raw_output` (JSONB), `parsed_ref`, plus `input_tokens`/
`output_tokens`/`cost_usd` so cost reporting never re-parses responses.

### `extraction_units`
Per-work-unit extraction state (idempotent, resumable): a unit is the
adapter's natural chunk of one document (`unit_key`); UNIQUE
(`unit_key`, `prompt_version`) is the resume key; `input_hash` detects
input drift; `records`/`dropped` carry per-unit stats and structured drop
reasons.

### `validation_verdicts`
Adversarial second-model verdicts: `agree | downgrade | flag_review`,
`agreement` (0–1), `target_confidence` (downgrades only — and only ever
down the confidence order), `applied`, `rationale`. UNIQUE
(`claim_id`, `prompt_version`).

### `validation_flags`
Deterministic-check findings: `check_code`, `record_kind`, `record_id`,
`severity` (`error|warning|review`), `message`, `details`,
`review_status` (`open|accepted|dismissed|cleared`). Re-runs refresh the
finding but never clobber a human decision; findings the engine stops
reporting are marked `cleared`.

### `factcheck_verdicts`
Entailment verdicts, one row per (statement, prompt version, text checksum,
revision round): `pass | fail | flag_review`, `unsupported`
(`[{quote, reason}]`), `revised` (this verdict triggered a rewrite). An
unchanged settled statement is never billed twice; a rewritten statement is
re-checked as new content.

### `factcheck_flags`
Citation spot-checks over audited statements; same lifecycle contract as
`validation_flags`.

### `review_items`
The human review queue: `kind` (`entailment_failure`,
`interpretive_addition`, `contested_as_settled`, `model_disagreement`),
statement context, `suggested_action`, `status`
(`open|accepted|dismissed`). One open item per (kind, statement); a decided
item keeps its decision forever, and a reappearing finding opens a new item.
