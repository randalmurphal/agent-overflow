# Advanced Features + Parity Loop — Progress Tracker

## Status: IN PROGRESS

## Codebase Patterns

(Populated as iterations discover important patterns.)

## Known Issues

(Issues found during review phase or parity audit. Highest severity first. Agent fixes these BEFORE doing scheduled work items.)

### From Parity Audit (pre-populated)

- IMPORTANT: No stacked/compound git actions (forge has commit+push+pr compound flows)
- IMPORTANT: No PR-based thread creation (forge can take a PR URL, fetch branch, set up workspace)
- IMPORTANT: No settings config issue surfacing to UI (forge tracks and displays malformed config errors)
- IMPORTANT: No mid-turn guard when user sends message while turn is active

## Resolved Issues

(Issues moved here after being fixed and committed.)

- CRITICAL: Worktree threads now get forge-style generated branch names. Blank worktree creation uses a temporary `forge/<8-hex>` branch, and the first user turn renames it to a descriptive `forge/<fragment>` branch before the provider send.
- CRITICAL: Proposed plans are now first-class items. Codex `item/completed` plan items and Claude `ExitPlanMode` tool uses emit dedicated `proposed_plan` payloads, triage persists lightweight preview metadata, and the frontend renders an expandable plan card with copy, download, and workspace save actions.
- CRITICAL: Thread forking now has backend and UI parity. Forks clone the local timeline, persist `forked_from_thread_id`, create immediate provider-side resume state for Codex, and use Claude's resume-plus-`--fork-session` flow via persisted pending fork state on first start.
- CRITICAL: Claude threads now get forge-style AI-generated first-turn titles. The first user send launches a one-shot Claude structured-output prompt, applies the result only if the title is still the default, and emits the same `thread_renamed` event path the frontend already listens to.
- CRITICAL: Active thread model changes now restart provider sessions instead of only updating defaults. The backend exposes `UpdateThreadModel`, restarts active sessions against the stored resume reference, rolls the model back on restart failure, and the header model picker now switches the live thread instead of editing future-thread defaults.
- IMPORTANT: File-change tool rows now persist forge-style inline diff artifacts. Summary-only rows are stored immediately, upgraded in place to exact patches from later turn diffs when the file set is unambiguous, and existing exact tool patches are preserved instead of being overwritten by native turn diffs.
- IMPORTANT: Command mutation rows now capture forge-style pre-command inline diffs for supported `rm`/`git rm` and `mv`/`git mv` operations. Triage snapshots workspace state on command start, skips dependent/unsupported/directory/overwrite cases like forge, and persists exact or summary-only tool-result patches only after successful completion.

## Completed Work Items

- 2026-04-16: Fixed the top critical known issue: Claude AI-generated thread title parity.
- 2026-04-16: Fixed the next critical known issue: auto-generated worktree branch naming parity.
- 2026-04-16: Fixed the top critical known issue: proposed plan rendering and handling parity.
- 2026-04-16: Fixed the next critical known issue: thread forking support.
- 2026-04-16: Fixed the top critical known issue: session restart for model changes mid-conversation.
- 2026-04-16: Fixed the top important known issue: inline diff upgrade parity for file-change tool rows.
- 2026-04-16: Fixed the next important known issue: command mutation inline diff capture parity.

## Iteration Log

- 2026-04-16: Fixed the top critical known issue by reading Forge's `ProviderCommandReactor`, `Prompts.ts`, and `ClaudeTextGeneration` flow first, then replacing agent-overflow's old post-turn heuristic with a first-turn Claude CLI structured-output title generator. The backend now kicks off title generation when the first Claude user turn is sent, sanitizes the returned title to forge-style sidebar constraints, applies it only if the title is still the default via a store compare-and-swap update, emits the standard `thread_renamed` event for the frontend, and removes the stale triage heuristic path. `IMPLEMENTATION-PARITY.md` was updated to reflect the real behavior. Verification: `go build ./...`, `go vet ./...`, `go test ./... -count=1`, `cd frontend && npm run build`, `cd frontend && npm run check`. Playwright UI verification was not run because this iteration did not change the frontend and no `wails dev` process was running.
- 2026-04-16: Fixed worktree branch naming parity by reading Forge's `ProviderCommandReactor`, shared git branch helpers, and temporary worktree flow first, then matching the visible lifecycle in agent-overflow. Blank worktree creation now uses a temporary `forge/<8-hex>` branch, the first user turn renames temporary worktree branches to a descriptive `forge/<fragment>` name before the provider send, git branch rename support now resolves collisions with numeric suffixes, and the sidebar worktree UI now treats the branch input as optional with the same first-turn rename behavior documented in `IMPLEMENTATION-PARITY.md`. Verification: `go build ./...`, `go vet ./...`, `go test ./... -count=1`, `cd frontend && npm run build`, `cd frontend && npm run check`. Playwright UI verification was not run because no `wails dev` process was running in this iteration.
- 2026-04-16: Implemented proposed-plan parity ahead of the scheduled phase work because Known Issues take priority. Backend now normalizes Codex plan completions and Claude ExitPlanMode into `proposed_plan` events, strips embedded `<proposed_plan>` blocks from assistant text on turn completion, persists plan preview metadata, and exposes a thread-scoped workspace file write binding for save-to-workspace. Frontend now renders a dedicated `ProposedPlanCard` with forge-style collapse/expand behavior plus copy, download, and save actions. Verification: `go build ./...`, `go vet ./...`, `go test ./... -count=1`, `cd frontend && npm run build && npm run check`. Playwright UI verification was not run because `wails dev` was not running in this iteration.
- 2026-04-16: Implemented thread-fork parity by reading Forge's `thread.fork` flow, `ProviderCommandReactor`, Codex `thread/fork`, and Claude fork-session behavior first, then adding local fork metadata and resume-state support directly in code. Backend now stores `pending_fork_session_ref` and `forked_from_thread_id`, clones a source thread's timeline into the fork, auto-resumes fork-pending threads on switch, uses Codex provider-side `thread/fork` immediately, and launches Claude forks with `--fork-session` on first start. The sidebar now exposes a fork action, Wails bindings were regenerated, and regression coverage was added across app, provider, and store layers. Verification: `go build ./...`, `go vet ./...`, `go test ./... -count=1`, `go run github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0-alpha.74 generate bindings`, `cd frontend && npm run build && npm run check`. Playwright UI verification was not run because `wails dev` was not running in this iteration.
- 2026-04-16: Fixed model-switch parity by reading Forge's `ProviderCommandReactor` restart logic and Claude/Codex provider capability behavior first, then wiring an explicit active-thread model update flow in agent-overflow. The backend now exposes `UpdateThreadModel`, validates and persists the requested model, restarts active sessions using the stored resume reference, and rolls the model back if the restart fails; inactive threads simply store the new model for the next start. The header `ModelPicker` now switches the live thread instead of only changing future-thread defaults, and `IMPLEMENTATION-PARITY.md` was updated to describe the real behavior. Verification: `go build ./...`, `go vet ./...`, `go test ./... -count=1`, `cd frontend && npm run build`, `cd frontend && npm run check`. Playwright UI verification was not run because no `wails dev` process was running in this iteration.
- 2026-04-16: Fixed inline-diff upgrade parity by reading Forge's `ProviderRuntimeIngestion` diff tests and work-log expectations first, then wiring the same behavior into agent-overflow without copying Forge's orchestration model. Codex file-change updates now persist stable `tool_result` rows keyed by provider item ID, summary-only inline diff metadata upgrades in place to exact patches when a later `turn/diff/updated` can be filtered to that row's files, and the frontend renders those persisted file-change rows with an exact-patch expander. Existing exact tool patches are preserved instead of being overwritten by broader turn diffs, `IMPLEMENTATION-PARITY.md` now documents the real behavior, and focused regression tests were added for the upgrade and no-overwrite paths. Verification: `go test ./internal/triage ./internal/store -count=1`, `cd frontend && npm run check`, then full quality gate to follow in this iteration. Playwright UI verification was not run because no `wails dev` process was running in this iteration.
- 2026-04-16: Fixed command-mutation inline diff parity by reading Forge's `commandInlineDiffArtifacts.ts`, `ProviderRuntimeIngestion.ts`, and the corresponding diff tests first, then matching the supported-command behavior directly in agent-overflow. The triage layer now snapshots pre-command workspace state for supported `rm`/`git rm` and `mv`/`git mv` commands on tool start, rejects the same unsupported/dependent/directory/overwrite cases Forge skips, and persists a stable `tool_result` row with exact or summary-only inline diff metadata only after a successful command completion. `IMPLEMENTATION-PARITY.md` was updated to note that Forge's helper covers rename/delete command mutations rather than `cp`, and focused parser plus integration tests were added before the full quality gate. Playwright UI verification was not run because this iteration did not require a running `wails dev` session.

## Review Log

(Entries added during review phase.)
