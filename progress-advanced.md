# Advanced Features + Parity Loop — Progress Tracker

## Status: IN PROGRESS

## Codebase Patterns

(Populated as iterations discover important patterns.)

## Known Issues

(Issues found during review phase or parity audit. Highest severity first. Agent fixes these BEFORE doing scheduled work items.)

### From Parity Audit (pre-populated)

- CRITICAL: No AI-generated thread titles for Claude sessions (Codex sends thread/name/updated but Claude doesn't — forge generates titles for both)
- CRITICAL: No session restart for model changes mid-conversation (forge restarts session with new model, carries conversation via resumeCursor)
- IMPORTANT: No inline diff upgrade from summary to exact patch (forge extracts filtered unified diffs from turn checkpoint)
- IMPORTANT: No command inline diff capture for mv/rm/cp (forge captures pre-command state)
- IMPORTANT: No stacked/compound git actions (forge has commit+push+pr compound flows)
- IMPORTANT: No PR-based thread creation (forge can take a PR URL, fetch branch, set up workspace)
- IMPORTANT: No settings config issue surfacing to UI (forge tracks and displays malformed config errors)
- IMPORTANT: No mid-turn guard when user sends message while turn is active

## Resolved Issues

(Issues moved here after being fixed and committed.)

- CRITICAL: Worktree threads now get forge-style generated branch names. Blank worktree creation uses a temporary `forge/<8-hex>` branch, and the first user turn renames it to a descriptive `forge/<fragment>` branch before the provider send.
- CRITICAL: Proposed plans are now first-class items. Codex `item/completed` plan items and Claude `ExitPlanMode` tool uses emit dedicated `proposed_plan` payloads, triage persists lightweight preview metadata, and the frontend renders an expandable plan card with copy, download, and workspace save actions.
- CRITICAL: Thread forking now has backend and UI parity. Forks clone the local timeline, persist `forked_from_thread_id`, create immediate provider-side resume state for Codex, and use Claude's resume-plus-`--fork-session` flow via persisted pending fork state on first start.

## Completed Work Items

- 2026-04-16: Fixed the next critical known issue: auto-generated worktree branch naming parity.
- 2026-04-16: Fixed the top critical known issue: proposed plan rendering and handling parity.
- 2026-04-16: Fixed the next critical known issue: thread forking support.

## Iteration Log

- 2026-04-16: Fixed worktree branch naming parity by reading Forge's `ProviderCommandReactor`, shared git branch helpers, and temporary worktree flow first, then matching the visible lifecycle in agent-overflow. Blank worktree creation now uses a temporary `forge/<8-hex>` branch, the first user turn renames temporary worktree branches to a descriptive `forge/<fragment>` name before the provider send, git branch rename support now resolves collisions with numeric suffixes, and the sidebar worktree UI now treats the branch input as optional with the same first-turn rename behavior documented in `IMPLEMENTATION-PARITY.md`. Verification: `go build ./...`, `go vet ./...`, `go test ./... -count=1`, `cd frontend && npm run build`, `cd frontend && npm run check`. Playwright UI verification was not run because no `wails dev` process was running in this iteration.
- 2026-04-16: Implemented proposed-plan parity ahead of the scheduled phase work because Known Issues take priority. Backend now normalizes Codex plan completions and Claude ExitPlanMode into `proposed_plan` events, strips embedded `<proposed_plan>` blocks from assistant text on turn completion, persists plan preview metadata, and exposes a thread-scoped workspace file write binding for save-to-workspace. Frontend now renders a dedicated `ProposedPlanCard` with forge-style collapse/expand behavior plus copy, download, and save actions. Verification: `go build ./...`, `go vet ./...`, `go test ./... -count=1`, `cd frontend && npm run build && npm run check`. Playwright UI verification was not run because `wails dev` was not running in this iteration.
- 2026-04-16: Implemented thread-fork parity by reading Forge's `thread.fork` flow, `ProviderCommandReactor`, Codex `thread/fork`, and Claude fork-session behavior first, then adding local fork metadata and resume-state support directly in code. Backend now stores `pending_fork_session_ref` and `forked_from_thread_id`, clones a source thread's timeline into the fork, auto-resumes fork-pending threads on switch, uses Codex provider-side `thread/fork` immediately, and launches Claude forks with `--fork-session` on first start. The sidebar now exposes a fork action, Wails bindings were regenerated, and regression coverage was added across app, provider, and store layers. Verification: `go build ./...`, `go vet ./...`, `go test ./... -count=1`, `go run github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0-alpha.74 generate bindings`, `cd frontend && npm run build && npm run check`. Playwright UI verification was not run because `wails dev` was not running in this iteration.

## Review Log

(Entries added during review phase.)
