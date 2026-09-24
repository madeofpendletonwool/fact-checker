# fact-checker schema

The core data model of the sourced-claims engine: what every pipeline stage
writes into and every adapter builds on. Implemented as PostgreSQL 17
migrations in `internal/migrations/` (up + down pairs, embedded in the
binary, applied by `fact-checker migrate`). See [erd.md](erd.md) for the
diagram and [tables.md](tables.md) for per-table notes.

Ported from the Arda pipeline's Lore Graph
(`the-history-of-arda/pipeline/src/arda_pipeline/db/migrations/`), with the
Tolkien ontology stripped out of the core: the Arda corpus loads as a fixture
(`internal/integrity/testdata/arda_fixture.sql`) with no schema changes.

## Design rules

- **Sources are part of the data model, not an afterthought.** Claims,
  events, and audited statements cite the tiered `sources` registry with
  structured locators; every citation must satisfy the cited source's
  `locator_scheme`.
- **The ontology lives in adapter config, not in these tables.** Entity
  kinds, event kinds, event link kinds, and relationship categories are free
  text whose vocabularies each adapter owns. What the engine fixes is only
  what it must: the four-level confidence order, the flag/verdict/status
  lifecycles, and the ledger keys.
- **Contradictions are preserved, never smoothed.** Contested claims carry
  per-tradition `claim_versions` with citations pinned per version, and are
  paired in the `contradictions` register; nothing resolves them
  automatically — only a human review decision can.
- **Provenance flows to the audited statement.** `statement_provenance`
  rows back every statement with claims and/or sources plus confidence.
- **Machine actions are downgrade/flag only.** No table or column in this
  schema lets a machine pass upgrade a confidence or resolve a
  contradiction; upgrades and resolutions are human review outcomes.
- **Model outputs are auditable.** `pipeline_runs` / `model_outputs` keep
  raw LLM responses verbatim, with prompt version, model, and token usage.
- **Everything is idempotent and resumable.** Ledgers key on prompt version
  + input hash (+ content checksum for fact-check verdicts); re-running a
  settled unit is a no-op, and changed inputs re-run under the new contract.

## Where the domain went

| Arda (hardcoded)                | fact-checker (adapter-owned data)            |
| ------------------------------- | -------------------------------------------- |
| `tier` 1/2/3 source policy      | `sources.tier ≥ 1`; meaning defined per adapter |
| `entity_kind` enum (person…)    | `entities.kind` free text; vocabulary in adapter ontology |
| `calendar_code` / dating model  | `events.attributes.dating` (adapter-defined convention) |
| `event_link_kind` enum          | `event_links.link_kind` free text (adapter vocabulary) |
| relationship `category` enum    | `relationship_types.category` free text      |
| confidence display names        | four fixed codes; labels remapped per adapter |
| narrative IR (books/chapters/…) | `statements` with adapter-defined `scope_kind` |

## Integrity

`fact-checker integrity` runs the deterministic check set
(`internal/integrity`) over the live database and exits 1 on error-severity
findings: uncited claims, citations with invalid locators, dangling
references. Warning-severity findings (orphan claims, contested claims not
in the register, uncited claim versions, unprovenanced statements, duplicate
entity names) demand human attention but do not fail the run.
