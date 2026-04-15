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
- `WI-3.1: Git core`

## Iteration Log

- 2026-04-15: Completed `WI-0.1` by wiring `store.New` through a versioned migration runner, adding legacy v1 seeding for pre-version databases, and expanding migration tests to cover fresh, versioned, and legacy upgrade paths. `internal/store` coverage is now 85.4%.
- 2026-04-15: Completed `WI-3.1` by adding `internal/git` command execution with timeout and output limits, implementing status/diff/branch queries, and adding repo-backed tests plus command-mock coverage. `internal/git` coverage is now 82.7%.

## Review Log

(Entries added during review phase.)
