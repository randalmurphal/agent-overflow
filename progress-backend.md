# Backend Loop — Progress Tracker

## Status: IN PROGRESS

## Codebase Patterns

- `internal/store/migrate.go` owns schema setup through ordered `Migration` entries plus small helper functions; legacy pre-version databases are backfilled into `migration_versions` before newer migrations run.
- `internal/git/core.go` centralizes bounded command execution while `internal/git/status.go` keeps git porcelain parsing and repository queries separate from process management.

## Known Issues

(Issues found during review phase. Highest severity first.)

## Resolved Issues

(Issues moved here after being fixed and committed.)

## Completed Work Items

- `WI-0.1: Database migration system`
- `WI-0.2: Backend file reorganization`
- `WI-1.5: Diff accumulation fix`
- `WI-1.6: Triage router updates`
- `WI-3.1: Git core`

## Iteration Log

- 2026-04-15: Completed `WI-0.1` by wiring `store.New` through a versioned migration runner, adding legacy v1 seeding for pre-version databases, and expanding migration tests to cover fresh, versioned, and legacy upgrade paths. `internal/store` coverage is now 85.4%.
- 2026-04-15: Completed `WI-0.2` by verifying the parity directory layout, confirming the provider/session and triage/router renames are wired, and adding the missing `internal/settings/doc.go` package documentation file. `go build ./...`, `go vet ./...`, and `go test -coverprofile=coverage.out ./... -count=1` all passed.
- 2026-04-15: Completed `WI-1.5` by marking Codex `turn/diff/updated` notifications as replaceable, adding turn-scoped payload upsert/update paths in store+triage, and covering both replace and append diff persistence flows with regression tests. `go build ./...`, `go vet ./...`, and `go test -coverprofile=coverage.out ./... -count=1` all passed, with `internal/store` now at 84.2% coverage.
- 2026-04-15: Completed `WI-1.6` by routing the new inline event kinds in triage, adding thread title/model store updates for rename and reroute events, and buffering Codex reasoning deltas until turn completion while keeping Claude thinking persistence immediate. `go build ./...`, `go vet ./...`, and `go test -coverprofile=coverage.out ./... -count=1` all passed, with `internal/store` at 83.9% coverage and `internal/triage` at 90.2%.
- 2026-04-15: Completed `WI-3.1` by adding `internal/git` command execution with timeout and output limits, implementing status/diff/branch queries, and adding repo-backed tests plus command-mock coverage. `internal/git` coverage is now 82.7%.

## Review Log

(Entries added during review phase.)
