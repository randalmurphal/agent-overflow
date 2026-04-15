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
- `WI-1.4: Codex structured user-input and permission handling`
- `WI-1.5: Diff accumulation fix`
- `WI-1.6: Triage router updates`
- `WI-1.1: Claude protocol additions`
- `WI-1.2: Codex protocol additions`
- `WI-3.1: Git core`
- `WI-3.2: Git actions`
- `WI-3.3: Git worktree operations`
- `WI-3.4: GitHub CLI integration`
- `WI-2.1: Provider binary detection`

## Iteration Log

- 2026-04-15: Completed `WI-0.1` by wiring `store.New` through a versioned migration runner, adding legacy v1 seeding for pre-version databases, and expanding migration tests to cover fresh, versioned, and legacy upgrade paths. `internal/store` coverage is now 85.4%.
- 2026-04-15: Completed `WI-0.2` by verifying the parity directory layout, confirming the provider/session and triage/router renames are wired, and adding the missing `internal/settings/doc.go` package documentation file. `go build ./...`, `go vet ./...`, and `go test -coverprofile=coverage.out ./... -count=1` all passed.
- 2026-04-15: Completed `WI-1.5` by marking Codex `turn/diff/updated` notifications as replaceable, adding turn-scoped payload upsert/update paths in store+triage, and covering both replace and append diff persistence flows with regression tests. `go build ./...`, `go vet ./...`, and `go test -coverprofile=coverage.out ./... -count=1` all passed, with `internal/store` now at 84.2% coverage.
- 2026-04-15: Completed `WI-1.6` by routing the new inline event kinds in triage, adding thread title/model store updates for rename and reroute events, and buffering Codex reasoning deltas until turn completion while keeping Claude thinking persistence immediate. `go build ./...`, `go vet ./...`, and `go test -coverprofile=coverage.out ./... -count=1` all passed, with `internal/store` at 83.9% coverage and `internal/triage` at 90.2%.
- 2026-04-15: Completed `WI-3.1` by adding `internal/git` command execution with timeout and output limits, implementing status/diff/branch queries, and adding repo-backed tests plus command-mock coverage. `internal/git` coverage is now 82.7%.
- 2026-04-15: Completed `WI-3.2` by adding `internal/git/actions.go` for commit/push/pull/checkout/create-branch flows, including upstream-aware push behavior and input validation, and covering the actions with temp-repo tests. `go build ./...`, `go vet ./...`, and `go test -coverprofile=coverage.out ./... -count=1` all passed, with `internal/git` at 82.1% coverage.
- 2026-04-15: Completed `WI-3.3` by adding worktree create/remove/list support to `internal/git/core.go`, parsing `git worktree list --porcelain`, and covering the flow with parser assertions plus temp-repo create/list/remove tests. `go build ./...`, `go vet ./...`, and `go test -coverprofile=coverage.out ./... -count=1` all passed, with `internal/git` at 82.2% coverage.
- 2026-04-15: Completed `WI-3.4` by adding `internal/git/github.go` for `gh pr create` and `gh pr list` flows, reusing the new list path for status open-PR lookup, and covering JSON parsing, URL returns, and missing-`gh` behavior with mocked CLI tests. `go build ./...`, `go vet ./...`, and `go test -coverprofile=coverage.out ./... -count=1` all passed, with `internal/git` at 80.6% coverage.
- 2026-04-15: Completed `WI-1.4` by normalizing Codex `item/tool/requestUserInput` and `item/permissions/requestApproval` server requests into structured `ApprovalRequest` payloads, propagating request route metadata (`turnId`/`itemId`), and adding direct/meta + session-dispatch tests for both flows. `go build ./...`, `go vet ./...`, and `go test -coverprofile=coverage.out ./... -count=1` all passed, with `internal/provider/codex` at 87.5% coverage.
- 2026-04-15: Completed `WI-1.1` and `WI-1.2` by normalizing Claude `tool_progress` and `compact_boundary` metadata into contract-shaped payloads, flattening Codex `account/rateLimits/updated` notifications into `RateLimitsSnapshot` entries, and tightening provider protocol tests around the new event metadata contracts. `go build ./...`, `go vet ./...`, and `go test -coverprofile=coverage.out ./... -count=1` all passed, with `internal/provider` at 84.8% coverage, `internal/provider/claude` at 87.1%, and `internal/provider/codex` at 85.3%.
- 2026-04-15: Completed `WI-2.1` by wiring `App` startup to initialize the settings service, exposing a real `GetProviderStatuses` binding that detects Claude and Codex using configured binary paths, and adding app-level tests to verify both configured-path and default-path detection flows. `go build ./...`, `go vet ./...`, and `go test -coverprofile=coverage.out ./... -count=1` all passed, with `internal/provider` at 84.8% coverage.

## Review Log

(Entries added during review phase.)
