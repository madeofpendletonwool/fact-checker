# AGENTS.md

Working conventions for this repository. Follow these; ask before deviating
on anything architectural.

## Commands

| Task                | Command                              |
| ------------------- | ------------------------------------ |
| Build               | `go build ./...`                     |
| Vet                 | `go vet ./...`                       |
| Test (full suite)   | `go test -race ./...`                |
| Lint                | `golangci-lint run`                  |
| Format              | `gofmt -w .` / `goimports -w .`      |
| Run stack           | `docker compose up --build`          |
| Health check        | `curl -fsS http://localhost:8080/healthz` |
| Migrate only        | `go run ./cmd/fact-checker migrate`  |

Run lint and the full test suite before every commit push. CI runs the same
commands on every push and pull request, and builds the container image to
GHCR on merge to `main`.

## Conventions

- **Go style**: standard `gofmt` formatting, idiomatic error wrapping
  (`fmt.Errorf("...: %w", err)`), table-driven tests. No comments narrating
  the obvious; package docs explain intent.
- **Commits**: Conventional Commits, a single summary line (`feat: ...`,
  `fix: ...`, `chore: ...`). Short bullet list underneath only when the
  commit genuinely does more than one thing. No co-author or
  generated-by trailers, ever.
- **Branches**: short-lived feature branches off `main`; push `main` directly
  only for scaffold-level or mechanical changes.
- **Secrets**: configuration is environment-only. Real values live in a
  gitignored `.env`; `.env.example` carries placeholders for every variable
  and must be updated in the same commit as any new `internal/config` parsing.
  Never log `FACTCHECK_AI_API_KEY` or any secret.
- **Migrations**: add `NNNN_description.up.sql` / `.down.sql` pairs to
  `internal/migrations/` (embedded via `go:embed`). Never edit an applied
  migration — add a new one. Forward compatibility: migrations must run
  forward on an empty database.
- **ADRs**: architecture decisions go in `docs/adr/NNNN-title.md` — context,
  options, decision, consequences — before the code that embodies them lands.

## Engine invariants (non-negotiable)

These come from the reference implementation and hold in every stage:

1. The LLM extracts and cites; it never decides what is true. Records that
   cannot cite a source are dropped and logged, never kept.
2. Machine passes may only downgrade, flag, or request rewrite — never
   upgrade, edit, or delete.
3. Contradictions are preserved as first-class data, never averaged away or
   auto-resolved.
4. Deterministic checks are pure, unit-testable functions over the graph.
5. Provenance is part of the data model.
6. Everything is idempotent, resumable, and budgeted via ledgers keyed on
   prompt version + input hash + content checksum.
