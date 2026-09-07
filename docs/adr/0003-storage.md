# ADR-0003: Storage

- Status: Accepted
- Date: 2026-09-07
- Deciders: collinp (kickoff decision for the platform build-out)

## Context

The reference implementation stores everything in PostgreSQL, and the data
model leans on it: per-source claim versions, a contradiction register,
ledger tables with unique keys (prompt version + input hash + content
checksum), and single-writer discipline per run. The open question is whether
to also carry an embedded SQLite profile for small self-hosted deployments.

Options:

1. **PostgreSQL only.** One SQL dialect, one test matrix, full use of
   Postgres features (advisory locks for single-writer, rich constraints,
   `jsonb` for verbatim model outputs).
2. **PostgreSQL primary + SQLite profile.** Nice story for tiny
   self-hostings, but it doubles the migration test matrix and forces
   lowest-common-denominator SQL before the schema even exists.

## Decision

**PostgreSQL is the primary and only supported datastore for now; the SQLite
profile is deferred, not carried.** Revisit when a real small-footprint
deployment need exists — the door is explicitly open.

To keep the door open: Postgres-specific features (advisory locks,
dialect-specific DDL) stay isolated behind the database layer rather than
scattered through stage code, and migrations avoid gratuitously
non-portable SQL where a portable form costs nothing.

## Consequences

- Migrations target Postgres only; docker-compose ships Postgres 17 and the
  stack is fully runnable with no external datastore.
- The claims graph, contradiction register, and ledgers can use Postgres
  features freely, provided usage stays behind the data-access layer.
- A future SQLite profile means a new migration compatibility story and a
  decision about which features degrade (advisory locks → connection-level
  locking discipline); that is a new ADR, not a patch.
