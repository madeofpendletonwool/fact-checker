# Extraction stage (MAD-457)

The first LLM stage: ingested documents become atomic, fully-cited
claims/entities/events/relationships. The platform's founding principle is
mechanical, not aspirational: **the model extracts and cites; it never
decides what is true.** Every rule below exists to make that checkable.

Ported from the reference implementation's
`arda_pipeline/extract/` with the domain parts (the Tolkien Gateway
reference source, the chapter scope, the both-tier rule) lifted into
adapter configuration.

## Work units

One work unit = one model call's scope. An adapter (the `extract.Adapter`
interface) turns the ingested documents into units: the unit's text, its
reference citation (normally the document itself), and the **candidate
set** of primary-source units the model may cite. The Arda-shaped adapter
(`adapters/arda`, manifest `testdata/arda_pilot.json`) ports the reference
behaviour: one unit per latest-revision document of the reference source,
candidates narrowed per document by lexical overlap (title + summary +
keywords against the document's first 4k chars), capped at
`max_candidates`, falling back to the authored `default` picks when
nothing matches, with a tier filter (`candidate_tier_max`) keeping only
primary tiers.

Unit text truncates at `max_unit_chars` (default 24k); truncation is a
lint flag counted in run stats.

## The citation contract (cite-or-drop)

Each unit's policy (`require_reference`, `require_primary`) fixes what a
citable record must carry. The Arda both-tier rule sets both: every claim
and event cites the reference document AND at least one primary
candidate, with locators carrying the keys the citation surface requires.

Records that cannot comply are **dropped and logged, never kept**, with a
structured reason in the run stats `drop_log` and the per-unit ledger's
`dropped` column:

- `uncited` — no citations at all
- `missing_reference_citation` / `missing_primary_citation` — one tier absent
- `bad_locator: …` — a citation missing the required locator keys for its source
- `unknown source '…'` — a citation outside the candidate set
- `dangling … ref` — references to ids that resolve neither in the payload nor the graph
- `contested_without_versions` — a contested/abandoned claim with no per-tradition versions
- plus id-shape, duplicate, self-relationship, and unknown-rel_type drops

Contested and abandoned-version claims must carry per-tradition
`versions`, each with its own citations; kept ones auto-register an open
entry in the `contradictions` register (resolution only ever by human
review).

## Prompts are code

`extract/prompts.go` carries `PromptVersion` (currently `extract-001`).
The version is the ledger key: done units are skipped only under the same
version AND an unchanged input hash (document text + candidate set). A
prompt change that affects outputs means bumping the version — never
editing in place — which re-extracts the corpus under the new contract.

Responses are parsed against the wire schema (`extract/wire.go`) before
anything is written: strict per-record decoding with salvage, so one
malformed record never sinks its valid siblings. An unparseable response
triggers a JSON-repair retry (up to `--max-attempts`); raw responses are
stored verbatim in `model_outputs` with prompt version, model, and token
usage.

## Runs: idempotent, resumable, bounded

```
fact-checker extract run --adapter adapters/arda/testdata/arda_pilot.json \
    [--limit N] [--dry-run] [--budget USD] [--concurrency N] [--max-units N]
fact-checker extract status
```

- `--limit N` bounds the run (remainder deferred, resumable); `--dry-run`
  plans units and reports estimates without calling the model or writing
  anything beyond the pipeline_runs row.
- Each unit commits in its own transaction (ledger row + model outputs +
  records). Interrupted runs resume where they stopped; failed units
  retry on the next run; input drift (changed text or candidate set)
  re-extracts rather than serving stale records.
- Only the model calls and wire parsing are concurrent; every database
  write stays on the main goroutine so per-unit resume semantics hold
  (the reference implementation's ADR-0023 pattern). The client's minimum
  request interval is one shared global rate gate.
- Guardrails: `FACTCHECK_EXTRACT_MAX_UNITS` hard cap per run
  (`--max-units`); soft USD budget (`--budget`, else
  `FACTCHECK_BUDGET_PER_RUN_USD`) enforced against the configured extract
  prices (`FACTCHECK_PRICE_MTOK_EXTRACT_INPUT/OUTPUT`) with a pre-call
  estimate; unpriced runs fall back to the unit cap and track tokens.
  A budget or cap stop is a clean stop: the run finishes `ok` with the
  remaining units deferred, and spend is reported.

## Model endpoint

Any OpenAI chat-completions-compatible endpoint:
`FACTCHECK_AI_BASE_URL` (default `https://api.openai.com/v1`),
`FACTCHECK_AI_API_KEY`, `FACTCHECK_MODEL_EXTRACT`. The client
(`internal/llm`) sends an identified User-Agent, honours
`Retry-After` with exponential backoff on 429/5xx and network errors,
maps oversized-input errors to a fast per-unit failure, and never logs
the API key.

## Tests

- `extract/wire_test.go`, `extract/validate_test.go` — pure: wire
  parsing/salvage and every drop rule.
- `extract/runner_test.go` — end-to-end over a scratch Postgres with a
  fake client: fixture extraction with drops, resume by `--limit`,
  failed-unit retry, budget halt, unpriced fallback, JSON repair,
  overload, concurrency, input drift, dry run.
- `adapters/arda` — the port proof: the Arda-shaped adapter config
  extracts the Arda pilot fixtures through the generic engine with no
  engine changes, and the resulting graph passes integrity with zero
  errors and zero warnings.

DB-backed tests skip unless `FACTCHECK_TEST_DATABASE_URL` points at a
scratch Postgres; CI provisions one.
