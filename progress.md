# Agent Overflow v1 — Progress Tracker

## Status: IN PROGRESS — Review Phase

## Codebase Patterns

- Go module: `agent-overflow`
- Wails bindings regenerated from Go structs → `store.Thread`, `store.Item`, `store.PayloadMeta`
- `wails dev` has a `sysctl` error in sandboxed environments (not a code issue)
- Claude CLI wire format: assistant messages come in multiple lines per message ID (thinking + text separately)
- `rate_limit_event` is an undocumented Claude CLI event type — silently skipped
- Codex app-server requires initialize handshake → initialized notification → thread/start before any turn

## Known Issues

(Issues found during review phase. Highest severity first. Agent resolves these before doing new adversarial reviews.)

## Resolved Issues

- **7 missing frontend components** (WorkEntry, WorkGroup, DiffPreview, CommandOutput, BackgroundTray, ComposerControls, Markdown) — created and wired into ChatView
- **2 missing utility files** (format.ts, diff.ts) — created; time.ts consolidated into format.ts
- **models.ts used auto-generated re-exports** — replaced with explicit interfaces per spec; Thread.provider now typed as `'claude' | 'codex'`
- **parseResult struct missing SessionID field** in claude/protocol.go — added to match spec
- **Extra CodexThreadID() method** in codex/codex.go — removed (not in spec, not used)
- **AssistantMessage.svelte** had inline markdown parser — replaced with Markdown.svelte component
- **StatusBar.svelte** had inline formatTokens/formatCost — moved to shared format.ts

## Completed Work Items

### 17-24. Frontend: types, stores, events, and all UI components ✅
- TypeScript types: events.ts (ProviderEvent, EventKind, ApprovalRequest, TokenUsage), models.ts (re-exports + DiffMeta/CommandOutputMeta/ThinkingMeta)
- Stores: thread.svelte.ts (createThreadPane factory), panes.svelte.ts (pane registry), threads.svelte.ts (thread list), events.ts (event router), bindings.ts
- Components: Sidebar, ThreadList, ThreadRow, ChatView, MessageTimeline, UserMessage, AssistantMessage, Composer, ApprovalPrompt, StatusBar
- App.svelte: root layout with Sidebar + ChatView, setupEventListeners + refreshThreads on mount
- Wails bindings regenerated with proper types
- All components use pane: ThreadPane prop pattern (tiling-ready)
- svelte-check: 0 errors, 0 warnings (103 files)
- Commits: 76a6074, f7c560e

### 15-16. App struct and Wails wiring ✅
- `app.go` — Full App struct with store, triage, sessions map, mutex
- Thread/Item/Payload/Session operations all wired
- `main.go` — OnShutdown callback registered
- Compiles and vets clean
- Commit: 500d283

### 12-14. Claude + Codex sessions and approval flow ✅
- `internal/provider/claude/claude.go` — Session, Config, NewSession, Send, Interrupt, RespondToApproval, readLoop
- `internal/provider/codex/codex.go` — Session, Config, NewSession with initialize handshake, Send, Interrupt, RespondToApproval, sendRequest, readLoop, dispatchLine, handleServerRequest
- Wire formats verified: Claude control_response allow/deny, Codex JSON-RPC accept/decline/acceptForSession
- 25 claude tests, 33 codex tests
- Commit: 2a0000f

### 9-11. Triage layer (meta extraction + router + heavy payloads) ✅
- `internal/triage/meta.go` — ExtractDiffMeta, ExtractCommandOutputMeta, ExtractThinkingMeta
- `internal/triage/triage.go` — Router, Handle (all EventKind routing), persistHeavy, buildSummary
- Text accumulation per-thread, persisted on turn_complete
- Heavy payloads (diff/command_output/thinking) → SQLite payload+item + meta emit
- 26 tests, 92.0% coverage
- Commit: e6b1411

### 7+8. Codex JSON-RPC protocol (framing + notification classification) ✅
- `internal/provider/codex/protocol.go` — ClassifyNotification, readNestedString, readTopLevelString
- Handles all methods from Section 10: turn lifecycle, item lifecycle, text deltas, command output, diffs, token usage, errors, plan updates
- 20 explicitly skipped notification types
- 21 tests, 94.4% coverage
- Commit: 45d1719

### 5+6. Claude NDJSON parser (core + result/stream/control) ✅
- `internal/provider/claude/protocol.go` — ParseLine, parseSystem, parseAssistant, parseResult, parseStreamEvent, parseControlRequest, extractSessionInfo
- Handles all message types from Sections 7-8
- Real fixture captured from `claude --output-format stream-json` saved in testdata/
- 18 tests, 89.2% coverage
- Wire format validated against real CLI: no deviations
- Commit: 11fa5b1

### 4. Item and payload persistence ✅
- 9 new tests: InsertItem/ListItems ordering, FK constraints, NextItemIndex, LastTurnIndex, InsertItem bumps thread updated_at, payload meta round-trip, GetPayloadData, ListPayloadMetas cross-thread isolation
- Store coverage: 82.4%
- Commit: 2e36176

### 3. SQLite store setup and thread operations ✅
- `internal/store/store.go` — Store struct, New, Close, migrations, model types
- `internal/store/threads.go` — CreateThread, GetThread, ListThreads, UpdateThread, DeleteThread, ArchiveThread, UpdateSessionRef
- `internal/store/items.go` — InsertItem, ListItems, NextItemIndex, LastTurnIndex (also implemented, tests in item 4)
- `internal/store/payloads.go` — InsertPayload, GetPayloadMeta, GetPayloadData, ListPayloadMetas (also implemented, tests in item 4)
- 13 thread tests, DDL matches Section 3 exactly, provider CHECK constraint verified
- Commit: dd50743

### 2. Subprocess process management ✅
- `internal/provider/process.go` — Spawn, WriteLine, ReadLine, Close, Kill, Done, Err
- SpawnConfig with Binary, Args, Dir, Env; process group isolation via Setpgid
- Graceful shutdown: stdin close → 3s SIGTERM → 2s SIGKILL
- 13 tests, 84.2% coverage on provider package
- Commit: 1ad4f19

### 1. Shared provider types ✅
- `internal/provider/types.go` — all types from IMPLEMENTATION.md Section 4
- ProviderKind, EventKind (17 constants), ItemKind (8 constants), ProviderEvent, ApprovalRequest, ApprovalResponse, SessionInfo, TokenUsage
- Tests: uniqueness, JSON round-trip, omitempty behavior
- Commit: 45176e9

## Iteration Log

### Iteration 11 — Item 25: Integration verification
- `go build ./...` ✅, `go vet ./...` ✅, all tests pass ✅
- `npm run check` ✅ (103 files, 0 errors)
- `npx vite build` ✅ (56.7kB JS + 12.9kB CSS gzipped)
- `wails dev` starts frontend dev server but Go binary fails in sandbox (sysctl PATH issue — environment, not code)
- Coverage: provider 84.2%, claude 57.1%, codex 48.7%, store 82.4%, triage 92.0%

### Iteration 10 — Items 17-24: Frontend types, stores, components
- Created all TypeScript types, stores, event router, and UI components
- 10 Svelte components, all tiling-ready with pane prop pattern
- svelte-check: 0 errors, vite build: success

### Iteration 9 — Items 15-16: App struct + Wails wiring
- Replaced stub app.go with full implementation
- Added OnShutdown to main.go
- Quality gate: build ✅, vet ✅, all tests pass, npm check ✅

### Iteration 8 — Items 12-14: Sessions + approval flow
- Created claude.go and codex.go session managers
- Combined items 12, 13, 14 (Claude session, Codex session, approval wiring)
- Quality gate: all build/vet pass, tests pass
- Coverage note: session lifecycle code (readLoop, NewSession) requires real binaries for integration testing; protocol/wire format coverage is high

### Iteration 7 — Items 9-11: Triage layer
- Created meta.go and triage.go router
- Combined items 9, 10, 11 since they're tightly coupled
- 26 tests, triage coverage 92.0%
- Quality gate: all pass

### Iteration 6 — Items 7+8: Codex JSON-RPC protocol
- Created protocol.go with ClassifyNotification and JSON helpers
- 21 tests, codex coverage 94.4%
- Quality gate: all pass

### Iteration 5 — Items 5+6: Claude NDJSON parser
- Created protocol.go with full parser covering all message types
- Items 5+6 combined since result/stream/control are part of same ParseLine switch
- Captured real CLI fixture, saved as testdata/real_output.ndjson
- Quality gate: all pass, claude coverage 89.2%

### Iteration 4 — Item 4: Item and payload persistence tests
- Added 9 tests for items and payloads
- Quality gate: go build ✅, go vet ✅, go test ✅ (store 82.4%, provider 84.2%)

### Iteration 3 — Item 3: SQLite store + thread operations
- Created store.go, threads.go, items.go, payloads.go matching Sections 3 and 5
- Also implemented items/payloads to unblock cascade test; dedicated tests in item 4
- Quality gate: go build ✅, go vet ✅, go test ✅ (store 52.8%, provider 84.2%)
- No deviations from spec

### Iteration 2 — Item 2: Subprocess process management
- Created `internal/provider/process.go` matching Section 6 exactly
- Added context parameter to Spawn (spec match)
- 13 tests covering full lifecycle, env, dir, copy safety
- Quality gate: go build ✅, go vet ✅, go test ✅ (84.2% coverage)
- No deviations from spec

### Iteration 1 — Item 1: Shared provider types
- Created `internal/provider/types.go` with all types matching Section 4 exactly
- Created `internal/provider/types_test.go` with 11 test functions
- Quality gate: go build ✅, go vet ✅, go test ✅, npm run check ✅
- No deviations from spec

## Review Log

### Review 1 — Spec Compliance
**Category**: Compare all types, interfaces, methods, and files against IMPLEMENTATION.md  
**Scope**: All 14 Go source files, all frontend types/stores/components/utils  
**Findings** (12 total — 5 functional, 7 cosmetic):

**Functional deviations fixed:**
1. 7 missing frontend components (WorkEntry, WorkGroup, DiffPreview, CommandOutput, BackgroundTray, ComposerControls, Markdown) — created
2. 2 missing utility files (format.ts, diff.ts) — created, time.ts consolidated into format.ts
3. models.ts used auto-generated Wails class re-exports instead of explicit interfaces — rewritten per spec
4. parseResult struct in claude/protocol.go missing SessionID field — added
5. Extra CodexThreadID() method in codex/codex.go — removed

**Cosmetic deviations noted (not bugs):**
- Import paths in store files use `../../../wailsjs/` (correct) vs spec's `../../wailsjs/` (spec error)
- Helper functions (nilIfEmpty, boolToInt, nowMillis) in store.go instead of threads.go — file organization only
- buildArgs extracted into named function in claude.go — structural, not logic
- Extra skip entry `local_command_output` in claude parseSystem — discovered from real CLI, harmless
- Extra util file time.ts existed (not in spec layout) — consolidated into format.ts
- Unused uuid import removed from codex.go — correct, spec had dead import

**Quality gate**: go build ✅, go vet ✅, go test ✅, npm run check ✅ (0 errors, 0 warnings, 111 files), vite build ✅  
**Coverage**: provider 84.2%, claude 57.1%, codex 48.9%, store 82.4%, triage 92.0%

### Review 2 — Error Handling
**Category**: Find every discarded error, every `_ = err`. Only acceptable for: error unwind cleanup, documented stdlib idioms, spec-designated non-critical (cite section)  
**Scope**: All 14 Go source files  
**Findings** (21 total — 7 bugs, 8 potential issues, 6 acceptable):

**Bugs fixed:**
1. `app.go:54` — `os.MkdirAll` error discarded; directory creation failure would cause confusing DB open error. Now checked and fatal.
2. `app.go:219` — `LastTurnIndex` error assigned to `_` in `SendMessage`; could cause duplicate turn indices. Now returns error.
3. `app.go:233` — `SendMessage` logged but didn't return error on user message persistence failure; user messages would be lost on disk full. Now returns error (blocks send to provider).
4. `app.go:302` — `fmt.Sscanf` error unchecked in `RespondToApproval`; invalid Codex request ID would silently send response with ID 0. Now returns error.
5. `claude/protocol.go:189-190` — `parseResult` double marshal/unmarshal with both errors discarded; malformed result would emit false `TurnComplete`. Now returns error.
6. `claude/protocol.go:222-224` — `parseStreamEvent` same pattern; malformed stream events silently dropped. Now returns error.
7. `triage/triage.go:177-207` — `persistHeavy` logged payload insertion failure but continued to insert item with orphaned payload reference; item insertion failure also logged but `persistHeavy` always returned `nil`. Now returns error on payload failure (preventing orphaned items) and on item failure. Meta emit moved before persistence so frontend still gets preview.

**Potential issues fixed:**
8. `main.go:32` — `println` without `os.Exit(1)` on `wails.Run` error; app would exit with code 0 on startup failure. Now uses `fmt.Fprintf(os.Stderr)` + `os.Exit(1)`.
9. `codex/codex.go:87` — `writeNotification("initialized")` error discarded; broken pipe during handshake would cause confusing thread/start failure. Now checked with cleanup on failure.
10. `codex/codex.go:358` — `handleServerRequest` discarded `id.Int64()` error; non-integer JSON-RPC IDs would silently produce rpcID=0. Now logs and returns.
11. `codex/codex.go:328` — `dispatchLine` silently dropped responses with non-integer IDs; pending requests would timeout. Now logs the error.
12. `codex/codex.go:377` — `writeResponse` error discarded for unknown server requests. Now logs the failure.
13. `store/items.go:19` — `InsertItem` discarded error from thread `updated_at` touch. Now logs the error.
14. `triage/triage.go:82-88` — `EventTurnComplete` and `persistHeavy` silently defaulted to index 0 on `LastTurnIndex`/`NextItemIndex` failure. Now logs with context before defaulting.
15. `app.go:162` — `triage.Handle` return value discarded in provider onEvent callback. Now logged.

**Acceptable patterns (with justification):**
- `json.Marshal` on plain structs/maps (`_ =` or `_, _`): `json.Marshal` only fails for channels, funcs, or cycles — all inputs are serializable types. (10+ instances across protocol.go, codex.go)
- `extractSessionInfo` discarding `json.Unmarshal` errors on individual fields: defensive parsing of external protocol; partial `SessionInfo` is better than failing the entire init event.
- `buildApprovalMeta` discarding `json.Unmarshal` on params: fallback to method name as tool description is reasonable.
- `stdin.Close()` in process shutdown: pipe close errors are always benign (broken pipe from exited process).
- `syscall.Kill` in `signalGroup`: ESRCH race is unavoidable during process cleanup; function falls through to SIGKILL.
- `readLoop` continue on parse errors: resilient streaming — one bad line should not kill the session.

**New tests added:**
- `TestPersistHeavyReturnsErrorOnClosedStore` — verifies `Handle` returns error when persistence fails and meta is still emitted
- `TestTurnCompleteReturnsErrorOnClosedStore` — verifies turn_complete event is still emitted even when text persistence fails

**Quality gate**: go build ✅, go vet ✅, go test ✅, npm run check ✅ (0 errors, 0 warnings, 111 files)  
**Coverage**: provider 84.2%, claude 56.2%, codex 47.5%, store 81.7%, triage 93.7%
