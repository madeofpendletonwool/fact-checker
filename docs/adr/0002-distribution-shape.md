# ADR-0002: Distribution shape and adapter loading

- Status: Accepted
- Date: 2026-09-07
- Deciders: collinp (kickoff decision for the platform build-out)

## Context

The engine needs a distribution shape (who runs it, and how) and an adapter
loading story (how a domain plugs in). Options considered:

**Distribution**

1. Library only — stages as importable packages; consumers write their own
   driver. Maximum flexibility, minimum operability.
2. Library + CLI — packages plus one binary that drives them. The library
   boundary keeps stages testable; the CLI gives non-engineers and CI a
   stable entry point.
3. Queue-worker service — a long-running daemon consuming a job queue.
   Good for continuous operation, but premature: every stage run is
   idempotent and resumable by design (ledgers keyed on prompt version +
   input hash + content checksum), so batch invocations already resume
   cleanly.

**Adapter loading**

1. Compiled Go plugins (`plugin` package) — fragile across platforms and
   toolchain versions; rejected.
2. Out-of-process adapter services — operationally heavy before there is a
   second adapter; deferred, not rejected.
3. Declarative config files + Go code against an SDK contract — adapters are
   ordinary Go packages plus a versioned manifest (source registry, ontology,
   check registration, hooks) loaded at startup.

## Decision

**Library + CLI now; worker later.** The pipeline stages live as importable
packages (`sources`, `extract`, `validate`, `factcheck`, `review`); the
`fact-checker` binary drives them (`serve`, `migrate` today; one subcommand
per stage as they land). When continuous operation is genuinely needed, the
same binary grows a `worker` subcommand consuming a queue — the ledger
machinery already provides the resume semantics, so no stage code changes.

**Adapters load via config + SDK**: versioned manifests reference Go packages
compiled into the binary. The adapter SDK (Stage 4 of the build-out) defines
the Go interfaces and the manifest format; out-of-process adapters over a
defined protocol remain a future option if a domain cannot or should not
compile in.

## Consequences

- One artifact: `fact-checker` binary + container image. CI gates invoke the
  CLI and use its exit codes.
- Stage packages must keep their logic pure of flag parsing and I/O wiring —
  the CLI composes them; tests import them directly.
- Adapter manifests are versioned artifacts reviewed like code; the engine
  never special-cases a domain.
