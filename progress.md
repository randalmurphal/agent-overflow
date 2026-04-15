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

- **sendRequest returned full JSON-RPC envelope** instead of just the result — callers `readStringFromResponse(resp, "thread", "id")` could never find the thread ID because it was nested under "result". Fixed to return `rpcResp.Result`.
- **handleServerRequest put error inside "result"** instead of at JSON-RPC top level — violated JSON-RPC 2.0 spec. Fixed to write proper `{"error":{...}}` at top level.
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

### Review 3 — Test Coverage
**Category**: Functions below 80%, missing edge case tests  
**Scope**: All 6 Go packages (provider, claude, codex, store, triage, root)  
**Findings** (2 packages below 80%, 2 bugs found during test writing):

**Coverage before**: claude 56.2%, codex 47.5% — both failing the 80% threshold  
**Root cause**: Session lifecycle methods (NewSession, Send, Interrupt, RespondToApproval, readLoop, Close, sendRequest, writeNotification, writeResponse, handleServerRequest) had 0% coverage because they require subprocess interaction

**Approach**: Used `cat` as a mock subprocess for both providers. Cat echoes stdin to stdout, letting tests exercise readLoop dispatch, write verification, and close/disconnect flows. For Codex NewSession, created a bash mock script that responds to JSON-RPC requests with proper results.

**Bugs found and fixed:**
1. `codex/codex.go:sendRequest` — returned the full JSON-RPC envelope (`{"jsonrpc":"2.0","id":1,"result":{...}}`) instead of just the result. All callers (`readStringFromResponse(resp, "thread", "id")`) could never find nested keys because they weren't prefixing with "result". Fixed sendRequest to extract and return `rpcResp.Result`.
2. `codex/codex.go:handleServerRequest` — for unknown server requests, put the error inside the `result` field (`{"result":{"error":{...}}}`) instead of at the JSON-RPC top level (`{"error":{...}}`). This violates JSON-RPC 2.0 spec. Fixed to write proper error response directly.

**Tests added:**
- Claude (12 new tests): NewSessionSpawnsAndRunsReadLoop, SessionSend, SessionInterrupt, SessionRespondToApproval (3 subtests), SessionIDAccessor, ReadLoopDispatchesTextDelta, ReadLoopDispatchesTurnComplete, ReadLoopContinuesOnParseError, ReadLoopEmitsDisconnectedOnExit
- Codex (19 new tests): DispatchLineInvalidJSON, DispatchLineResponseNonIntegerID, DispatchLineResponseNoMatchingPending, DispatchLineServerRequest, DispatchLineServerRequestNonIntegerID, WriteNotification, WriteResponse, RespondToApprovalMethod (3 subtests), ThreadIDAccessor, ReadLoopDispatchesNotification, ReadLoopRoutesResponseToPending, HandleServerRequestApproval, HandleServerRequestFileApproval, HandleServerRequestUnknown, SendRequestViaCat, SendRequestContextCancel, Send, Interrupt, ReadLoopEmitsDisconnectedOnExit, ReadLoopCleansPendingOnExit, NewSessionWithMock
- Codex (2 new tests): ReadStringFromResponseInvalidJSON, ReadStringFromResponseNonStringValue

**Coverage after**: provider 84.2%, claude 87.0%, codex 87.8%, store 81.7%, triage 93.7% — all >= 80%  
**Quality gate**: go build ✅, go vet ✅, go test ✅, npm run check ✅ (0 errors, 0 warnings, 111 files)

### Review 4 — Code Consistency
**Category**: Same patterns across all packages  
**Scope**: All 14 Go source files, all frontend TypeScript/Svelte files  
**Findings** (20 total — 7 fixed, 13 acceptable design decisions):

**Fixed:**
1. **Status color mismatch** (ComposerControls.svelte vs StatusBar.svelte): `connected`/`ready` states mapped to `bg-green-400` in ComposerControls but `bg-accent` in StatusBar. Aligned to `bg-accent`.
2. **Unused `pane` prop** in WorkGroup.svelte: declared in `$props()` but never referenced. Removed.
3. **Duplicated JSON helper** `readStringFromResponse` (codex.go) was identical to `readNestedString` (protocol.go) in the same package. Replaced body with delegation to `readNestedString`.
4. **Error wrapping inconsistency** in `app.go:StartSession`: `GetThread` error was wrapped with `"start session: %w"` but `NewSession` errors returned bare. Fixed both to wrap consistently.
5. **`ApprovalRequest.TurnID` never populated**: Neither Claude nor Codex providers set this field, but Go struct had no `omitempty` and TypeScript type declared it required. Added `omitempty` to Go tag and made TS field optional (`turnId?`).
6. **Lost error chain** in `claude/protocol.go:21`: `fmt.Errorf("missing or invalid type field")` discarded the underlying unmarshal error. Fixed to `%w`.
7. **Unhandled marshal error** in `parseControlRequest`: `data, _ := json.Marshal(raw)` was inconsistent with `parseResult`/`parseStreamEvent` which both return errors. Fixed to match.

**Accepted as design decisions (not bugs):**
- Store CRUD naming (`Create*` vs `Insert*`): follows IMPLEMENTATION.md spec verbatim
- `Spawn` vs `New*` constructor naming: `Spawn` is conventional for process creation
- Session method signature differences (`Interrupt`, `RespondToApproval`): inherent to different provider protocols, spec-driven
- `ParseLine` vs `ClassifyNotification`: different abstractions for fundamentally different wire formats (NDJSON vs JSON-RPC notifications)
- `SessionID()` vs `ThreadID()` accessor asymmetry: expose different concepts, spec-driven
- `makeThread` test helper without `t.Helper()`: pure factory function that never fails, no testing assertions
- Two logging systems (`log.Printf` vs `runtime.LogFatalf`): Wails requires its logger for startup fatals
- JSDoc inconsistency across frontend: not adding docs to unchanged components (CLAUDE.md rule)
- Section header formatting (`---` vs `--`): cosmetic
- Different JSON parsing approaches between claude/ and codex/: fundamentally different wire formats
- Test structure (flat vs table-driven): table-driven used where it adds value (approval subtests), flat elsewhere
- Duplicated `testThread` const across test packages: separate packages, no shared test helpers package warranted
- `ProviderEvent.timestamp` format difference (RFC 3339 for events vs Unix millis for store models): matches Go type system (time.Time vs int64)

**Quality gate**: go build ✅, go vet ✅, go test ✅, npm run check ✅ (0 errors, 0 warnings, 111 files)  
**Coverage**: provider 84.2%, claude 86.5%, codex 88.7%, store 81.7%, triage 93.7%

### Review 5 — Dead Code
**Category**: Unused exports, unreferenced types, implemented-but-unwired components  
**Scope**: All 14 Go source files, all frontend TypeScript/Svelte files  
**Findings** (19 Go + 20 frontend items identified — 12 fixed, rest accepted as API surface or spec-defined):

**Bugs found and fixed:**
1. **`ProviderKind` constants unused** — `app.go` used string literals `"claude"`/`"codex"` instead of `provider.Claude`/`provider.Codex`. Wired constants.
2. **`MaxLineSize` unnecessarily exported** — only used within `provider` package. Unexported to `maxLineSize`.
3. **`ReadLine` race with `cmd.Wait()`** — `TestReadLineEOFAfterExit` was failing because `cmd.Wait()` closes stdout pipe before scanner reads, producing "file already closed" instead of `io.EOF`. Added `isClosedPipeErr` helper to treat closed-pipe errors as EOF.
4. **`freezeStreamingContent` never called** — streaming text was never frozen into items on `turn_complete`. This was a functional bug: after a turn completed, streaming content stayed visible and was never replaced by persisted items. Replaced with `finalizeTurn()` method that clears streaming state, clears active tool calls, and reloads items from DB.
5. **`appendItem` never called** — dead method on ThreadPane. Removed.
6. **`DiffPreview.svelte` never imported** — component was built (Review 1) but never wired into MessageTimeline. Now renders for items with `kind === 'diff'` using payload meta.
7. **`CommandOutput.svelte` never imported** — same pattern. Now renders for items with `kind === 'command_execution'`.
8. **`WorkEntry.svelte` only used by dead WorkGroup** — now directly imported by MessageTimeline to render active tool calls.
9. **`WorkGroup.svelte` dead** — tool calls aren't persisted as items (forwarded inline during turn, not stored), so grouping is unnecessary. Deleted.
10. **`activeToolCalls` getter never read** — no component displayed running tool calls. MessageTimeline now renders active tool calls as WorkEntry items during a turn.
11. **Dead binding re-exports** — `bindings.ts` re-exported 15 functions but only 7 were imported. Removed: `GetThread`, `DeleteThread`, `RenameThread`, `ListItems`, `ListPayloadMetas`, `StopSession`, `GetSettings`, `ListThreads`.
12. **Dead store exports** — `getPane()` in panes.svelte.ts and `updateThreadInList()` in threads.svelte.ts never imported. Removed.

**Accepted as API surface / spec-defined (not removed):**
- `SessionID()` (claude), `ThreadID()` (codex) — session accessor methods, tested, part of session contract
- `Process.Done()`, `Process.Err()`, `Process.Kill()` — public process lifecycle API, tested
- `store.GetPayloadMeta()` — valid store operation, tested, may be needed for single-payload lookup
- `EventApprovalResolved`, background events (`EventBackgroundStart`/`Delta`/`Complete`) — spec-defined EventKind constants, handled in triage router, providers don't emit them yet but routing is in place
- `ItemToolCall`, `ItemToolResult`, `ItemBackgroundStarted` — spec-defined ItemKind constants for DB schema
- `ThinkingMeta` type (frontend) — matches Go type, kept for type consistency
- `payloadMetas` getter — now wired (used by MessageTimeline's `parseMeta` helper to render DiffPreview/CommandOutput)
- Config fields not set in production (SystemPrompt, AllowedTools, etc.) — intentional extensibility points for UI

**Quality gate**: go build ✅, go vet ✅, go test ✅, npm run check ✅ (0 errors, 0 warnings, 110 files), vite build ✅  
**Coverage**: provider 80.0%, claude 86.5%, codex 88.3%, store 81.7%, triage 93.7%
