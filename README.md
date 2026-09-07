# fact-checker

A generic, sourced-claims fact-checking engine: an LLM-audited research
pipeline where **the model never decides what is true**.

The engine ingests tiered sources, extracts atomic claims with mandatory
citations (uncited records are dropped, never kept), validates them with an
adversarial second model plus deterministic consistency checks, preserves
contradictions between credible sources as first-class data, and fact-checks
generated prose against its own provenance before anything reaches a human
review queue. Domains plug in through adapters: source registries, ontologies,
deterministic check sets, and ground-truth connectors. First application: a
docs-drift detector — internal documentation fact-checked against live
infrastructure.

Status: scaffold. The pipeline stages land incrementally; decisions are
recorded in the [ADRs](docs/adr/).

## Quickstart

```
docker compose up --build
curl -fsS http://localhost:8080/healthz
```

That builds the app, starts Postgres, applies the migration chain, and serves
the health endpoint. Configuration is environment-only — see
[`.env.example`](.env.example) for every variable (AI provider, per-stage
models, prices, budgets, rate limits) and copy it to a gitignored `.env` for
host-run development.

## Layout

| Path                | Purpose                                                          |
| ------------------- | ---------------------------------------------------------------- |
| `cmd/fact-checker/` | the binary: `serve`, `migrate`, `version`                        |
| `internal/config`   | environment configuration contract                               |
| `internal/database` | datastore connectivity                                            |
| `internal/migrations` | embedded, versioned SQL migrations                              |
| `internal/server`   | HTTP surface (health today, review/report later)                 |
| `sources/`          | tiered source registry (stage: extraction groundwork)             |
| `extract/`          | cite-or-drop claim extraction                                     |
| `validate/`         | adversarial + deterministic validation (downgrade/flag only)      |
| `factcheck/`        | entailment vs provenance, revision loop                           |
| `review/`           | human review queue                                                |
| `adapters/`         | the domain contract that makes the engine a platform              |

## Development

```
go build ./...
go test ./...
golangci-lint run
```

See [AGENTS.md](AGENTS.md) for conventions (commits, migrations, ADRs,
secrets handling).
