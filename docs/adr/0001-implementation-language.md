# ADR-0001: Implementation language

- Status: Accepted
- Date: 2026-09-07
- Deciders: collinp (kickoff decision for the platform build-out)

## Context

This platform generalizes a proven pipeline originally written in Python (the
reference implementation, `the-history-of-arda`: extract → validate →
factcheck, with ledger-backed idempotency, budgets, and a human review
workflow). That code is a working spec — prompt contracts, drop reasons,
ledger keys, and the downgrade-only discipline are all battle-tested there.

Two credible paths:

1. **Port the Python implementation.** Fastest to correctness: the code is
   the spec, the LLM ecosystem (SDKs, structured-output tooling) is mature,
   and behavioral parity is nearly free.
2. **Rewrite in Go.** Single static binary, cross-compilation, and a
   deployment story that matches the self-hosting ecosystem this platform is
   meant to live in. Strong static typing over the ledger keys, drop-reason
   enums, and confidence states; real concurrency primitives for the
   rate-gated, resumable pipeline workers. Cost: reimplementation risk —
   every behavior must be re-derived from the reference, and the Python code
   remains the tiebreaker when behavior is ambiguous.

The LLM-dependency surface is thin and HTTP-shaped (an OpenAI-compatible
endpoint with structured outputs), so Go's weaker LLM SDK ecosystem is not a
real penalty here.

## Decision

**Rewrite in Go.** The reference Python pipeline is the executable spec, not
the codebase: ports must replicate its behavior, and its fixtures become the
port-proof tests. Python is used for reading, not shipping.

## Consequences

- Every pipeline stage lands as a careful port with fixture tests that pin
  reference behavior (drop reasons, verdict ledgers, contradiction handling).
- Prompts, prompt versions, and ledger keys carry the semantics across
  languages; they must be ported verbatim, not "improved" in transit.
- Single-binary distribution: `fact-checker` serves, migrates, and runs
  pipeline stages; container image is a scratch-thin alpine artifact.
- If a Python-only capability ever becomes load-bearing (a parsing lib with
  no Go equivalent), it comes back as an adapter-side subprocess, not a
  language change.
