# Backend Loop — Progress Tracker

## Status: IN PROGRESS

## Codebase Patterns

- `internal/store/migrate.go` owns schema setup through ordered `Migration` entries plus small helper functions; legacy pre-version databases are backfilled into `migration_versions` before newer migrations run.
- `internal/git/core.go` centralizes bounded command execution while `internal/git/status.go` keeps git porcelain parsing and repository queries separate from process management.
- `internal/discussion/registry.go` owns discussion-definition validation and persistence rules, while `internal/discussion/channel.go` and `internal/discussion/deliberation.go` keep channel lifecycle and turn-state logic split into separate services.

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
- `WI-3.5: Git Wails bindings`
- `WI-2.1: Provider binary detection`
- `WI-2.2: Settings service (JSON file)`
- `WI-2.4: Cost calculation`
- `WI-2.5: Session lifecycle improvements`
- `WI-2.6: Thread title auto-generation`
- `WI-2.7: Updated Wails bindings`
- `WI-4.1: Discussion store operations`
- `WI-4.2: Discussion registry`
- `WI-4.3: Channel service`
- `WI-4.4: Deliberation engine`
- `WI-4.5: Discussion Wails bindings`
- `WI-5.1: Design artifact storage`
- `WI-6.2: Provider event logging`

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
- 2026-04-15: Completed `WI-2.2` by fixing the settings service cache to reload after external file edits/deletes using file-state checks, preserving sparse atomic writes, and extending settings tests to cover on-disk invalidation. `go build ./...`, `go vet ./...`, and `go test -coverprofile=coverage.out ./... -count=1` all passed, with `internal/settings` at 80.6% coverage.
- 2026-04-15: Completed `WI-2.4` by enriching triage-routed `EventTokenUsage` events with calculated `totalCostUsd` from the thread's persisted model, while preserving the raw usage payload for unknown models or lookup failures. `go build ./...`, `go vet ./...`, and `go test -coverprofile=coverage.out ./... -count=1` all passed, with `internal/provider` at 86.4% coverage and `internal/triage` at 87.6%.
- 2026-04-15: Completed `WI-2.5` by distinguishing intentional session shutdown from unexpected provider process exits in both Claude and Codex read loops, emitting `session_status` `"error"` events with structured exit metadata before the existing disconnect event, and extending process/logging coverage to keep internal packages above the 80% floor. `go build ./...`, `go vet ./...`, and `go test -coverprofile=coverage.out ./... -count=1` all passed, with `internal/provider` at 90.4% coverage, `internal/provider/claude` at 87.3%, `internal/provider/codex` at 85.9%, and `internal/logging` at 85.2%.
- 2026-04-15: Completed `WI-2.6` by generating Claude thread titles from the first persisted user message when the default `"New Thread"` title survives the first turn, reusing the existing rename event path so the store and frontend update together. `go build ./...`, `go vet ./...`, and `go test -coverprofile=coverage.out ./... -count=1` all passed, with `internal/triage` at 85.7% coverage and `internal/store` at 83.9% coverage.
- 2026-04-15: Completed `WI-2.7` by replacing the app-level approval binding with the structured `provider.ApprovalResponse` contract, wiring `GetSettings`/`UpdateSettings`/`GetModelsForProvider` plus `SwitchThread` and `ReconnectSession`, and teaching Codex approval responses to serialize user-input and permission resolutions in Forge-compatible JSON-RPC shapes. `go build ./...`, `go vet ./...`, and `go test -coverprofile=coverage.out ./... -count=1` all passed, with `internal/provider` at 86.4% coverage and `internal/provider/codex` at 85.7%.
- 2026-04-15: Completed `WI-3.5` by adding Wails git bindings for status/diff/branch/action/worktree flows, resolving repo-level vs workspace-level paths from thread metadata with sensible fallbacks, and covering branch-path resolution plus worktree lifecycle in app-level tests. `go build ./...`, `go vet ./...`, and `go test -coverprofile=coverage.out ./... -count=1` all passed, with `internal/git` at 80.6% coverage.
- 2026-04-15: Completed `WI-4.1` by adding store-layer discussion/channel types plus SQLite CRUD/query methods for discussion definitions, channels, and ordered channel messages, including monotonic channel status timestamps and regression coverage for discussion CRUD plus message ordering/filtering. `go build ./...`, `go vet ./...`, and `go test -coverprofile=coverage.out ./... -count=1` all passed, with `internal/store` at 83.0% coverage.
- 2026-04-15: Completed `WI-4.2` through `WI-4.5` by adding the discussion registry validation layer, a channel service with ordered human/agent posting and close semantics, an in-memory deliberation engine for turn alternation and unanimous conclusion tracking, and app bindings for discussion CRUD/start/message flows. `go build ./...`, `go vet ./...`, and `go test -coverprofile=coverage.out ./... -count=1` all passed, with `internal/discussion` at 84.2% coverage.
- 2026-04-15: Completed `WI-5.1` by adding SQLite-backed design artifact metadata operations in `internal/store`, a filesystem-backed `design.ArtifactStore` that writes `<baseDir>/<threadID>/<artifactID>.html` and rolls back orphaned files on metadata failures, plus regression tests for round-trip reads, kind filtering, validation, and cleanup/error paths. `go build ./...`, `go vet ./...`, and `go test -coverprofile=coverage.out ./... -count=1` all passed, with `internal/design` at 95.1% coverage and `internal/store` at 82.8% coverage.
- 2026-04-15: Completed `WI-6.2` by adding a dedicated NDJSON provider-event log schema, wiring per-session raw stdin/stdout logging through `provider.Spawn`, and enabling the logger from app startup when `AGENT_OVERFLOW_DEBUG` includes `provider` or `all`. `go build ./...`, `go vet ./...`, and `go test -coverprofile=coverage.out ./... -count=1` all passed, with `internal/provider` at 90.1% coverage and `internal/logging` at 86.2% coverage.

## Review Log

(Entries added during review phase.)
