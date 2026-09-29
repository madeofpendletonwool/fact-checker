# Validation stage (MAD-458)

The quality gate between extraction and the graph: an adversarial second
model re-checks every claim against its cited evidence, and a
deterministic consistency engine catches what models miss. Contradictions
are **preserved and flagged, never smoothed away**; machine passes may
only downgrade confidence or flag for review — never upgrade, edit, or
delete.

Ported from the reference implementation's
`arda_pipeline/validate/` with the deterministic check set restructured
as a registry adapters extend.

## Adversarial second model

One work item = one extracted claim. The validator — an independent
model call in an explicitly skeptical stance — receives the claim
(statement, confidence, subject/predicate/object), its citations with
locators, and the reference passage it was extracted from (the newest
ingested revision of the cited article, per the adapter's
`ReferenceSource`, truncated at `--max-claims`'s sibling
`FACTCHECK_VALIDATE_REFERENCE_CHARS`). It returns one verdict per claim:

| Verdict | Agreement | Effect |
| ------- | --------- | ------ |
| `agree` | ≥ `FACTCHECK_VALIDATE_AGREEMENT_THRESHOLD` (default 0.8) | claim stands |
| `downgrade` | any | confidence moves **down** to `target_confidence`, and only if still downward vs the current value |
| `flag_review` or low agreement | — | `adversarial_review` flag, claim untouched |

Claims without a usable reference citation or passage are flagged
(`missing_reference_citation` / `missing_reference_text`) and retried on
the next run. Batches of `FACTCHECK_VALIDATE_BATCH_SIZE` claims per model
call; runs are idempotent and resumable via the `validation_verdicts`
ledger — first-write-wins per (claim, prompt version), so claims already
verdicted under the same prompt version are skipped. Bounded by
`--budget` / `FACTCHECK_BUDGET_PER_RUN_USD` and
`FACTCHECK_VALIDATE_MAX_CLAIMS`, priced at
`FACTCHECK_PRICE_MTOK_VALIDATE_INPUT/OUTPUT`; unpriced runs fall back to
the claim cap and track tokens. A budget or cap stop is a clean stop:
the run finishes `ok` with the remaining claims deferred. Prompts are
versioned code (`PromptVersion` = `validate-001`); raw responses are
stored verbatim in `model_outputs` (`record_kind = 'validation_verdict'`)
with prompt version, model, and token usage, kept even when parsing
fails.

The endpoint is the same OpenAI-compatible one extraction uses
(`FACTCHECK_AI_BASE_URL` / `FACTCHECK_AI_API_KEY` /
`FACTCHECK_MODEL_VALIDATE`).

## Deterministic consistency engine

Pure functions over a `GraphState` snapshot (`validate/graph.go`) — no
DB, no LLM — so every rule is unit-testable without a database. The
check set is a **registry** (`validate/engine.go`): the engine ships the
generic core (`CoreChecks`) and adapters register their own domain
invariants through `validate.Adapter.Checks()` (the reference
implementation's dating bounds and genealogy cycles are the worked
example of what an adapter adds). Adapter checks run after the core set
over the same snapshot.

| Check | Severity | Catches |
| ----- | -------- | ------- |
| `claim_shape_invalid` | review | claims with no predicate and no object; not machine-checkable |
| `dangling_reference` | error | event participants/locations and claim endpoints that do not resolve |
| `duplicate_claim` | review | the same subject/predicate/object on two claim ids — merge candidate |
| `predicate_value_conflict` | review | same predicate, conflicting objects, credible sources (below) |
| `established_without_tier1` | review | `established` without a Tier-1 citation → downgraded to `derived_probable` |

`dangling_reference` is defensive: the schema's foreign keys already
prevent most dangling states, but the engine works over arbitrary
snapshots (fixtures, future storage profiles) and the rule costs nothing.

## Contradiction preservation

When ≥2 claims share a subject and predicate but carry conflicting
objects, and each cites at least one credible (tier 1–2) source, the
divergence is genuine:

- living claims are downgraded to `contested` — never resolved to a
  winner;
- a register entry is created (or an existing one linking ≥2 of the
  claims reused) in `contradictions`;
- each claim gets a `claim_versions` row labeled by its best primary
  citation (citation form, else source id), with the claim's own
  citations pinned to that version — the traditions stay separable
  end-to-end;
- conflicts involving an `abandoned_version` claim are registered but
  left at their confidence: abandoned and contested are distinct
  tradition states, not strengths.

## Flag lifecycle

Findings land in `validation_flags` keyed by (check_code, record_kind,
record_id). Re-runs refresh severity/message/details but **never clobber
a human decision** (`open → accepted | dismissed` survives); findings
the engine stops reporting are marked `cleared`.

## Runs and reports

```
fact-checker validate run --adapter <manifest.json> \
    [--offline] [--dry-run] [--limit N] [--budget USD] [--batch-size N] \
    [--agreement-threshold F] [--max-claims N] [--max-attempts N] \
    [--report-dir reports/validation] [--no-report] [--strict]
fact-checker validate status
```

- `--offline` runs the deterministic engine only — no model calls;
  `--dry-run` plans the adversarial pass and reports engine findings
  without writing anything beyond the `pipeline_runs` row.
- Every run is recorded (`run_kind = 'validate'`) with the full stats
  JSON: claim accounting, verdict summary, machine actions taken, flag
  rates.
- Reports publish to `--report-dir` (default `reports/validation`):
  `flag_rates.{json,md}` (flag rates per check and per 100 records,
  adversarial verdict summary, machine actions),
  `contradictions.{json,md}` (the full register with per-source
  versions), and `review_queue.{json,md}` (open findings, most severe
  first, each with a suggested action).
- Exit codes gate CI: 1 on error-severity findings, warnings too under
  `--strict`. `validate status` summarizes the verdict ledger and open
  flags.

## Tests

- `validate/engine_test.go`, `validate/wire_test.go` — pure, no
  database: every deterministic rule (conflict registration, the
  abandoned-version semantics, downgrade-only movement, registry
  composition) and the verdict wire contract.
- `validate/runner_test.go` — end-to-end over a scratch Postgres with a
  fake adversarial client: verdict ledger + first-write-wins, applied
  downgrades, registered contradictions with per-source versions,
  published reports, resumability, budget halt, JSON repair, dry run,
  stale-flag clearing, human-decision survival, and a custom adapter
  check running alongside the core set.
- `adapters/arda/validate_test.go` — the port proof: the Arda-shaped
  adapter config validates the pilot corpus through the generic engine
  offline with no engine changes.

DB-backed tests skip unless `FACTCHECK_TEST_DATABASE_URL` points at a
scratch Postgres; CI provisions one.
