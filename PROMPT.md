# PROMPT: agent-overflow v1

## Housekeeping

- Ignore `ralph.log`, `coverage.out`
- Pre-existing uncommitted changes are not the agent's problem

## Prime Directive

Building the complete v1 of agent-overflow. No dependencies on other loops. Key constraint: Wire formats for Claude CLI and Codex app-server MUST be validated against real provider processes before committing protocol code -- test in /tmp with real `claude` and `codex` binaries.

### Authority Hierarchy

1. ARCHITECTURE.md -- behavioral authority
2. IMPLEMENTATION.md -- implementation authority
3. This PROMPT file -- work items and rules

### Mission

Build a compiling, tested, working desktop app where:
- Both Claude Code CLI and Codex app-server run as subprocesses with proper lifecycle management
- Provider events are triaged: inline events forwarded to frontend, heavy payloads stored in SQLite with meta previews
- SQLite persists threads, items, and payloads across restarts
- Svelte 5 frontend renders streaming text, tool calls, diffs, approvals, and background tasks
- The app is usable end-to-end: create thread, pick provider, send messages, see responses, approve actions

## Rules of Engagement

### Non-Negotiable

1. Read progress.md first every iteration
2. Match types from IMPLEMENTATION.md exactly
3. Every test must be thorough -- no TODOs, no skipped tests
4. One work item per iteration
5. Wire format validation: Before committing Claude or Codex protocol code, test against real provider processes. Spawn `claude --input-format stream-json --output-format stream-json --verbose` or `codex app-server` in /tmp, send a test message, capture the raw JSON output, and use that to validate your parsing code. If the real wire format differs from IMPLEMENTATION.md, update the parsing code to match reality and note the deviation in progress.md.
6. Frontend testing: Use the Playwright MCP tools (browser_navigate, browser_snapshot, browser_click, etc.) to verify UI components work correctly. Run `wails dev` and test the app in the browser. Don't just assume components render correctly -- verify with Playwright.
7. Reference repos: When implementing provider protocols, consult:
   - /Users/randy/repos/llmkit/ -- working Go implementations of both Claude and Codex protocols
   - /Users/randy/repos/forge/ -- reference for how provider events map to UI (but NOT the architecture -- forge uses event sourcing which we explicitly don't want)
   - /Users/randy/repos/codex-source/ -- Codex open source for protocol details
   - Community Claude agent SDKs (severity1/claude-agent-sdk-go on GitHub) for wire format reference

### Prohibited

- No event sourcing, no orchestration engine, no command/event split -- Go is a triage pipe
- No in-memory read models or state duplication -- SQLite is the history, provider is the runtime truth
- No fmt.Println for debugging -- use log.Printf
- No custom error types -- use standard error wrapping
- No modifying ARCHITECTURE.md or IMPLEMENTATION.md (except during Spec Compliance review where code proves spec wrong)
- No global mutable state -- dependency injection only
- No blaming pre-existing issues
- No dead code -- if you build it, wire it
- No deferrals -- do it now or cite why it's literally impossible
- No unverified test assertions
- No guessing at wire formats -- test against real providers

## Environment

- Working directory: /Users/randy/repos/agent-overflow
- Language: Go 1.25, Svelte 5 / TypeScript
- Module: agent-overflow
- Specs: ARCHITECTURE.md, IMPLEMENTATION.md
- Test framework: Go testing (go test), Playwright MCP for frontend
- Database: SQLite via modernc.org/sqlite (in-memory :memory: for Go tests)

## Quality Gate

```bash
cd /Users/randy/repos/agent-overflow && go build ./... && go vet ./... && go test -coverprofile=coverage.out ./... -count=1 && cd frontend && npm run check
```

Coverage must be >= 80% on internal/ packages and increasing with every iteration.

For frontend items, also verify with Playwright MCP by navigating to the dev server and checking component rendering.

## Workflow Per Iteration

Standard: read progress -> pick item -> read specs -> implement -> test -> quality gate -> commit -> update progress

## Work Items

### Phase 0: Foundation (items 1-4)

#### 1. Shared provider types

**Spec sections**: IMPLEMENTATION.md Section 4
**Package**: `internal/provider`
**Files**: `internal/provider/types.go`
**Deliver**:
- All type definitions from Section 4: ProviderKind, EventKind (all constants), ItemKind (all constants), ProviderEvent, ApprovalRequest, ApprovalResponse, SessionInfo, TokenUsage
- Types must be exactly as specified including JSON tags
**Tests**:
- Verify all EventKind constants have unique string values
- Verify all ItemKind constants have unique string values
- Verify ProviderEvent JSON round-trip (marshal/unmarshal preserves all fields)
- Verify ApprovalRequest JSON round-trip
**Done when**: types.go compiles, tests pass, matches IMPLEMENTATION.md Section 4 exactly

#### 2. Subprocess process management

**Spec sections**: IMPLEMENTATION.md Section 6
**Package**: `internal/provider`
**Files**: `internal/provider/process.go`, `internal/provider/process_test.go`
**Deliver**:
- SpawnConfig struct, Process struct
- Spawn function: creates subprocess with Setpgid, stdin/stdout pipes, 10MB scanner buffer, env override, stderr to os.Stderr
- WriteLine: writes data + newline to stdin, checks if process is dead
- ReadLine: reads next line, returns copy (not scanner buffer), io.EOF on exit
- Done channel, Err accessor
- Close: graceful shutdown (stdin close -> 3s wait -> SIGTERM group -> 2s wait -> SIGKILL group)
- Kill: immediate SIGKILL
**Tests**:
- Spawn a simple process (e.g., `cat`), write a line, read it back
- Spawn a process, close gracefully, verify Done channel closes
- Spawn a process, kill immediately, verify Done channel closes
- ReadLine returns io.EOF after process exits
- WriteLine returns error after process exits
- Verify process group isolation (Setpgid is set)
**Done when**: process.go handles full lifecycle, all tests pass

#### 3. SQLite store setup and thread operations

**Spec sections**: IMPLEMENTATION.md Sections 3, 5 (store.go, threads.go)
**Package**: `internal/store`
**Files**: `internal/store/store.go`, `internal/store/threads.go`, `internal/store/store_test.go`
**Deliver**:
- Store struct with New (opens DB, runs migrations), Close
- Full DDL from Section 3 (WAL mode, foreign keys, 3 tables with indexes)
- Thread model type
- All thread methods: CreateThread, GetThread, ListThreads, UpdateThread, DeleteThread, ArchiveThread, UpdateSessionRef
- Helper functions: nilIfEmpty, boolToInt, nowMillis
**Tests**:
- New with :memory: creates tables successfully
- CreateThread + GetThread round-trip
- ListThreads returns non-archived threads ordered by updated_at DESC
- ListThreads excludes archived threads
- DeleteThread removes thread
- DeleteThread cascades to items (insert a thread + item, delete thread, verify item gone)
- ArchiveThread sets archived=1 and bumps updated_at
- UpdateSessionRef updates ref and bumps updated_at
- GetThread on nonexistent ID returns error
- CreateThread with duplicate ID returns error
**Done when**: store.go + threads.go implement all methods from spec, tests pass with :memory:

#### 4. Item and payload persistence

**Spec sections**: IMPLEMENTATION.md Section 5 (items.go, payloads.go)
**Package**: `internal/store`
**Files**: `internal/store/items.go`, `internal/store/payloads.go`, `internal/store/store_test.go` (extend)
**Deliver**:
- Item, Payload, PayloadMeta model types
- InsertItem, ListItems, NextItemIndex, LastTurnIndex
- InsertPayload, GetPayloadMeta, GetPayloadData, ListPayloadMetas
- InsertItem touches parent thread's updated_at
**Tests**:
- InsertItem + ListItems: verify ordering by turn_index, item_index
- InsertItem with payload_id: verify FK constraint (payload must exist)
- NextItemIndex: returns 0 for empty turn, increments for existing items
- LastTurnIndex: returns 0 for empty thread, returns max for populated
- InsertPayload + GetPayloadMeta: round-trip meta without data
- GetPayloadData: returns only the data blob
- ListPayloadMetas: returns metas via JOIN on items for a specific thread, excludes other threads
- InsertItem bumps parent thread updated_at
**Done when**: items.go + payloads.go implement all methods, tests pass

### Phase 1: Protocols (items 5-8)

#### 5. Claude NDJSON parser -- core messages

**Spec sections**: IMPLEMENTATION.md Section 8
**Package**: `internal/provider/claude`
**Files**: `internal/provider/claude/protocol.go`, `internal/provider/claude/claude_test.go`
**Deliver**:
- ParseLine function: parses a single NDJSON line into ProviderEvents
- Handle system/init -> EventInit with SessionInfo in meta
- Handle assistant messages -> EventTextDelta for text blocks, EventToolStart for tool_use blocks, EventThinking for thinking blocks, EventTokenUsage for usage
- extractSessionInfo helper reading from message top level (not nested data key)
- parseSystem with explicit skip list for all known subtypes (compact_boundary, api_retry, hook_*, tool_progress, notification, files_persisted, tool_use_summary, memory_recall, local_command_output)
**Tests**:
- Parse system/init JSON -> EventInit with correct SessionInfo fields
- Parse assistant with text block -> EventTextDelta with correct content
- Parse assistant with tool_use block -> EventToolStart with toolName in meta
- Parse assistant with thinking block -> EventThinking
- Parse assistant with usage -> EventTokenUsage
- Parse assistant with multiple content blocks -> multiple events in correct order
- Parse system with skipped subtypes (compact_boundary, api_retry, etc.) -> empty events, no error
- Parse unknown system subtype -> empty events, no error
- Parse invalid JSON -> returns error
- **Validate against real Claude CLI**: Spawn `claude --input-format stream-json --output-format stream-json --verbose` in /tmp, send a test message, capture stdout, and verify ParseLine handles the real output. Save a sample as a test fixture.
**Done when**: ParseLine handles all core message types, tests pass including real CLI validation

#### 6. Claude NDJSON parser -- result, stream, control

**Spec sections**: IMPLEMENTATION.md Section 8
**Package**: `internal/provider/claude`
**Files**: `internal/provider/claude/protocol.go` (extend), `internal/provider/claude/claude_test.go` (extend)
**Deliver**:
- parseResult: success -> EventTurnComplete, error -> EventError
- parseStreamEvent: extract text delta from stream_event envelope
- parseControlRequest: parse can_use_tool -> EventApprovalRequest with correct request_id, tool_name, input
- Handle unknown message types gracefully (return nil, nil)
**Tests**:
- Parse result/success -> EventTurnComplete
- Parse result with is_error=true -> EventError with error message
- Parse stream_event with text delta -> EventTextDelta
- Parse stream_event without delta -> empty events
- Parse control_request/can_use_tool -> EventApprovalRequest with correct fields
- Parse control_request with unknown subtype -> empty events
- Parse completely unknown type -> empty events, no error
**Done when**: all result/stream/control parsing works, tests pass

#### 7. Codex JSON-RPC framing

**Spec sections**: IMPLEMENTATION.md Sections 9-10
**Package**: `internal/provider/codex`
**Files**: `internal/provider/codex/protocol.go`, `internal/provider/codex/codex_test.go`
**Deliver**:
- JSON-RPC message types (not full ClassifyNotification yet -- just the framing)
- readNestedString and readTopLevelString helpers
- dispatchLine logic: classify as response (id + no method), server request (id + method), or notification (method + no id)
**Tests**:
- Response message (has id, has result, no method) -> correctly identified
- Server request (has id, has method) -> correctly identified
- Notification (has method, no id) -> correctly identified
- Invalid JSON -> logged, no crash
- readNestedString: extracts from nested objects, returns "" on missing keys
- readTopLevelString: extracts from top level, returns "" on missing
**Done when**: JSON-RPC framing correctly classifies all three message types, tests pass

#### 8. Codex notification classification

**Spec sections**: IMPLEMENTATION.md Section 10
**Package**: `internal/provider/codex`
**Files**: `internal/provider/codex/protocol.go` (extend), `internal/provider/codex/codex_test.go` (extend)
**Deliver**:
- ClassifyNotification function covering all methods from Section 10
- Handle: turn/started, turn/completed (success + failed), turn/diff/updated, item/started, item/completed, item/agentMessage/delta, item/commandExecution/outputDelta, item/fileChange/outputDelta, thread/tokenUsage/updated, error, turn/plan/updated
- Skip: thread/started, thread/status/changed, thread/name/updated, thread/archived, thread/unarchived, thread/closed, thread/compacted, item/autoApprovalReview/*, item/reasoning/*, item/mcpToolCall/progress, serverRequest/resolved, account/*, model/rerouted, configWarning, deprecationNotice
- Unknown methods -> nil (silent skip)
**Tests**:
- turn/started -> EventTurnStart with turnID
- turn/completed (success) -> EventTurnComplete
- turn/completed (failed) -> EventError + EventTurnComplete
- item/agentMessage/delta -> EventTextDelta
- item/started -> EventToolStart with itemID and itemType
- item/completed -> EventToolComplete
- item/commandExecution/outputDelta -> EventCommandOutput
- turn/diff/updated -> EventDiff
- thread/tokenUsage/updated -> EventTokenUsage
- error -> EventError
- Each skipped method -> empty events
- Unknown method -> empty events
- **Validate against real Codex**: Spawn `codex app-server` in /tmp, perform initialize handshake and thread/start, send a simple turn, capture notifications, verify ClassifyNotification handles the real output. Save samples as test fixtures.
**Done when**: ClassifyNotification handles all known methods, tests pass

### Phase 2: Triage (items 9-11)

#### 9. Meta extraction

**Spec sections**: IMPLEMENTATION.md Section 11 (meta.go)
**Package**: `internal/triage`
**Files**: `internal/triage/meta.go`, `internal/triage/triage_test.go`
**Deliver**:
- DiffMeta, CommandOutputMeta, ThinkingMeta structs
- ExtractDiffMeta: parse unified diff -> file path, change kind, insertions, deletions, preview (first 20 lines)
- ExtractCommandOutputMeta: extract last 10 lines as preview, line count
- ExtractThinkingMeta: first 200 chars as preview, rough token estimate
**Tests**:
- ExtractDiffMeta with a real unified diff: correct file path from +++ header
- ExtractDiffMeta counts insertions and deletions correctly (skip +++ and --- lines)
- ExtractDiffMeta detects "new file" -> changeKind "added"
- ExtractDiffMeta detects "deleted file" -> changeKind "deleted"
- ExtractDiffMeta detects "rename from" -> changeKind "renamed"
- ExtractDiffMeta preview truncates at 20 lines
- ExtractDiffMeta with empty input -> zero values, no crash
- ExtractCommandOutputMeta with 50-line output -> preview is last 10 lines, lineCount=50
- ExtractCommandOutputMeta with 3-line output -> preview is all 3 lines
- ExtractCommandOutputMeta with empty output -> empty preview, lineCount=1 (empty string splits to 1)
- ExtractThinkingMeta with 500-char content -> preview truncated at 200 + "..."
- ExtractThinkingMeta with 100-char content -> preview is full content
- ExtractThinkingMeta token estimate: 400 chars -> ~100 tokens
**Done when**: all three meta extractors work correctly, edge cases covered

#### 10. Triage router -- inline events

**Spec sections**: IMPLEMENTATION.md Section 11 (triage.go)
**Package**: `internal/triage`
**Files**: `internal/triage/triage.go`, `internal/triage/triage_test.go` (extend)
**Deliver**:
- Router struct with store, emit callback, textAccumulators map
- NewRouter constructor
- Handle method: route inline events (EventTextDelta, EventToolStart, EventToolComplete, EventTurnStart, EventApprovalRequest, EventApprovalResolved, EventSessionStatus, EventTokenUsage, EventError, EventBackgroundStart) directly to emit
- EventInit: emit + call store.UpdateSessionRef
- EventTurnStart: emit to frontend
- EventTextDelta: accumulate in textAccumulators AND emit to frontend
- EventBackgroundDelta: accumulate only, don't emit
**Tests**:
- Inline event (EventTextDelta) -> emit called with "provider:event" and the event
- Inline event does NOT call any store method
- EventInit -> emit called AND store.UpdateSessionRef called with correct session ID
- EventTextDelta -> text accumulated in the accumulator for the thread
- Multiple EventTextDelta for same thread -> all accumulated in order
- EventBackgroundDelta -> NOT emitted, but accumulated (or dropped per spec)
- Use mock store and mock emit function
**Done when**: all inline routing works, tests verify both emit and store calls

#### 11. Triage router -- heavy payloads and turn complete

**Spec sections**: IMPLEMENTATION.md Section 11 (triage.go persistHeavy)
**Package**: `internal/triage`
**Files**: `internal/triage/triage.go` (extend), `internal/triage/triage_test.go` (extend)
**Deliver**:
- persistHeavy: extracts meta, inserts payload to store, inserts item to store, emits meta via "provider:meta"
- EventDiff -> persistHeavy with kind "diff"
- EventCommandOutput -> persistHeavy with kind "command_output"
- EventThinking -> persistHeavy with kind "thinking"
- EventTurnComplete: if text accumulator has content -> persist as text item (kind "text", role "assistant"), clear accumulator, then emit turn_complete
- EventBackgroundComplete -> persistHeavy with kind "full_text"
- buildSummary helper for human-readable summaries
**Tests**:
- EventDiff -> store.InsertPayload called with diff meta + data, store.InsertItem called, emit called with "provider:meta"
- EventCommandOutput -> same pattern, verify command output meta
- EventThinking -> same pattern, verify thinking meta
- EventTurnComplete with accumulated text -> store.InsertItem called with kind "text", role "assistant", content is accumulated text, accumulator cleared
- EventTurnComplete without accumulated text -> no item inserted, just emit
- EventBackgroundComplete -> payload + item persisted
- buildSummary: diff -> "modified: +5/-3 src/main.go" format
- buildSummary: command_output -> "$ go build (exit 0, 15 lines)" format
- Persistence failure -> logged, not fatal, emit still happens
**Done when**: heavy payload routing works end-to-end, turn completion persists accumulated text

### Phase 3: Sessions (items 12-14)

#### 12. Claude CLI session

**Spec sections**: IMPLEMENTATION.md Section 7
**Package**: `internal/provider/claude`
**Files**: `internal/provider/claude/claude.go`
**Deliver**:
- Session struct with proc, threadID, sessionID, model, onEvent, cancel
- Config struct with all fields from spec
- NewSession: spawn claude CLI with correct args, start readLoop goroutine
- Send: write user message in correct format `{"type":"user","message":{"role":"user","content":"..."}}`
- Interrupt: write control interrupt message
- RespondToApproval: write control_response with correct format (behavior: allow/deny)
- readLoop: reads stdout, calls ParseLine, dispatches events, emits disconnected on EOF
- Close: cancel context + close process
- SessionID accessor
**Tests**:
- Verify buildArgs produces correct CLI flags for various Config combos
- Verify Send produces correct JSON wire format
- Verify RespondToApproval produces correct JSON for allow and deny
- Verify Interrupt produces correct JSON
- Integration: if claude binary is available, spawn a real session, send "say hello", verify EventInit and EventTextDelta arrive via onEvent callback. If claude is not available, skip gracefully.
**Done when**: Claude session manages full lifecycle, tests pass

#### 13. Codex app-server session

**Spec sections**: IMPLEMENTATION.md Section 9
**Package**: `internal/provider/codex`
**Files**: `internal/provider/codex/codex.go`
**Deliver**:
- Session struct with proc, threadID, codexThreadID, nextID, pending map, onEvent, cancel
- Config struct with all fields from spec
- NewSession: spawn codex app-server, perform initialize handshake, send initialized notification, thread/start or thread/resume, extract codex thread ID
- Send: write turn/start with codexThreadID (not our internal threadID)
- Interrupt: send turn/interrupt
- RespondToApproval: write JSON-RPC response with decision (accept/decline/acceptForSession)
- sendRequest: correlate request/response via pending map with 30s timeout
- writeNotification: fire-and-forget
- writeResponse: respond to server requests
- readLoop: classify lines (response/server request/notification), dispatch accordingly
- handleServerRequest: build approval meta for known methods, error response for unknown
- Close: cancel + close process
**Tests**:
- Verify sendRequest correctly correlates request/response IDs
- Verify writeNotification produces valid JSON-RPC notification
- Verify Send produces correct turn/start params with codexThreadID
- Verify RespondToApproval produces correct decision values
- Verify readLoop correctly routes responses to pending waiters
- Integration: if codex binary is available, spawn real app-server, perform handshake, verify initialize response is received. If not available, skip gracefully.
**Done when**: Codex session manages full lifecycle including handshake, tests pass

#### 14. Approval flow wiring

**Spec sections**: IMPLEMENTATION.md Sections 7, 9, 12
**Package**: `internal/provider/claude`, `internal/provider/codex`, app.go
**Files**: Multiple -- primarily verifying the approval round-trip
**Deliver**:
- Verify Claude approval flow: CLI sends control_request -> parsed as EventApprovalRequest -> frontend shows prompt -> user responds -> App.RespondToApproval -> session.RespondToApproval -> correct JSON written to stdin
- Verify Codex approval flow: app-server sends server request -> parsed as EventApprovalRequest -> frontend shows prompt -> user responds -> App.RespondToApproval -> session.RespondToApproval -> correct JSON-RPC response written to stdin
- Codex requestID is int64 (JSON-RPC id), Claude requestID is string (request_id from control protocol)
**Tests**:
- Claude: construct a control_request fixture -> ParseLine -> verify EventApprovalRequest has correct requestID, toolName, input
- Claude: verify RespondToApproval(allow) produces correct JSON
- Claude: verify RespondToApproval(deny) produces correct JSON
- Codex: construct a server request fixture -> verify handleServerRequest produces correct EventApprovalRequest
- Codex: verify RespondToApproval with int64 ID produces correct JSON-RPC response
- Codex: verify unknown server request method -> error response sent back
**Done when**: approval flow verified for both providers, request/response formats correct

### Phase 4: Wails Binding (items 15-16)

#### 15. App struct and thread/session bindings

**Spec sections**: IMPLEMENTATION.md Section 12
**Package**: main (root)
**Files**: `app.go`
**Deliver**:
- App struct with ctx, store, triage router, sessions map, mutex
- startup: open SQLite in user config dir, create triage router with EventsEmit wrapper
- shutdown: close all sessions, close store
- Thread operations: CreateThread, ListThreads, GetThread, DeleteThread, ArchiveThread, RenameThread
- Item operations: ListItems, ListPayloadMetas
- Payload operations: GetPayloadData
- Session operations: StartSession (spawns claude or codex session based on thread.Provider), SendMessage (persists user message THEN forwards to provider), InterruptTurn, StopSession
- RespondToApproval (routes to correct provider, handles string->int64 conversion for Codex)
- GetSettings stub
**Tests**:
- This is the Wails binding layer -- tested via integration with Playwright MCP in Phase 6. Unit testing individual methods is covered by the store/provider tests.
**Done when**: app.go compiles with all bindings, wails dev starts without errors

#### 16. Main entry point and Wails wiring

**Spec sections**: IMPLEMENTATION.md Section 12 (main.go update)
**Package**: main (root)
**Files**: `main.go`
**Deliver**:
- Register OnShutdown callback
- Verify wails.json frontend commands work
- Run `wails dev` and verify the app launches with the frontend
- Add modernc.org/sqlite to go.mod: `go get modernc.org/sqlite`
**Tests**:
- `go build ./...` succeeds
- `wails dev` launches without crash (verify manually or via Playwright MCP -- navigate to localhost, take snapshot)
**Done when**: app compiles and runs, frontend loads in the webview

### Phase 5: Frontend (items 17-24)

#### 17. TypeScript types and store foundation

**Spec sections**: IMPLEMENTATION.md Sections 13, 14
**Package**: frontend
**Files**: `frontend/src/lib/types/events.ts`, `frontend/src/lib/types/models.ts`, `frontend/src/lib/stores/thread.svelte.ts`, `frontend/src/lib/stores/threads.svelte.ts`, `frontend/src/lib/stores/bindings.ts`
**Deliver**:
- All TypeScript types matching Go types exactly (ProviderEvent, EventKind, ApprovalRequest, TokenUsage, Thread, Item, PayloadMeta, DiffMeta, CommandOutputMeta, ThinkingMeta)
- thread.svelte.ts: all state variables with $state runes, all mutation functions (appendTextDelta, freezeStreamingContent, addToolCall, completeToolCall, addApproval, removeApproval, addBackgroundTask, completeBackgroundTask, setSessionStatus, setTokenUsage, addPayloadMeta, appendItem, switchThread, clearThread)
- threads.svelte.ts: thread list state, refreshThreads, prependThread, removeThread, updateThreadInList
- bindings.ts: re-export all Wails-generated bindings
**Tests**:
- `npm run check` passes (svelte-check + TypeScript)
**Done when**: all types defined, stores compile, svelte-check passes

#### 18. Event listeners

**Spec sections**: IMPLEMENTATION.md Section 14 (events.ts)
**Package**: frontend
**Files**: `frontend/src/lib/stores/events.ts`
**Deliver**:
- setupEventListeners function using Wails EventsOn
- Route provider:event by kind to correct store mutation
- Route provider:meta to addPayloadMeta
- Route provider:error to console.error + setSessionStatus('error')
- Filter events by active thread ID (ignore events for non-active threads)
**Tests**:
- `npm run check` passes
- Verify with Playwright MCP: navigate to app, check console for no errors on load
**Done when**: event listeners wired, no console errors on startup

#### 19. Sidebar and thread list

**Spec sections**: Forge UI reference (sidebar section from research)
**Package**: frontend
**Files**: `frontend/src/lib/components/Sidebar.svelte`, `frontend/src/lib/components/ThreadList.svelte`, `frontend/src/lib/components/ThreadRow.svelte`, `frontend/src/App.svelte`
**Deliver**:
- Sidebar component: fixed left panel, resizable, with thread list and new thread button
- ThreadList: renders threads from threads store, ordered by updated_at
- ThreadRow: shows title, provider icon (C for Claude, X for Codex), relative time, archive button on hover
- New thread button: opens a form/dialog to pick provider, workspace path, model
- Click thread -> switchThread, load items
- App.svelte: sidebar + main content layout, call setupEventListeners and refreshThreads on mount
**Tests**:
- `npm run check` passes
- Playwright MCP: navigate to dev server, verify sidebar renders, click "new thread" button, verify form appears
**Done when**: sidebar shows threads, new thread creation works

#### 20. Message timeline and streaming

**Spec sections**: Forge UI reference (message timeline from research)
**Package**: frontend
**Files**: `frontend/src/lib/components/ChatView.svelte`, `frontend/src/lib/components/MessageTimeline.svelte`, `frontend/src/lib/components/UserMessage.svelte`, `frontend/src/lib/components/AssistantMessage.svelte`, `frontend/src/lib/components/Markdown.svelte`
**Deliver**:
- ChatView: container with MessageTimeline + Composer, starts session on mount if not already active
- MessageTimeline: renders items from store, keyed by id, append-only with auto-scroll
- UserMessage: right-aligned bubble with user text
- AssistantMessage: left-aligned with Markdown rendering, shows streaming content for active turn
- Markdown component: renders markdown with code syntax highlighting (using shiki or a lighter alternative)
- Streaming: text deltas append to streamingContent, rendered in a live AssistantMessage at the bottom
- On turn_complete: streaming content freezes into the item list
**Tests**:
- `npm run check` passes
- Playwright MCP: navigate to app, create a thread, verify empty state shows "Start a conversation"
- If a provider is available: send a message, verify streaming text appears and completes
**Done when**: messages render correctly, streaming works, auto-scroll on new content

#### 21. Work entries and diff previews

**Spec sections**: Forge UI reference (work entries, diff rendering)
**Package**: frontend
**Files**: `frontend/src/lib/components/WorkEntry.svelte`, `frontend/src/lib/components/WorkGroup.svelte`, `frontend/src/lib/components/DiffPreview.svelte`, `frontend/src/lib/components/CommandOutput.svelte`, `frontend/src/lib/utils/diff.ts`
**Deliver**:
- WorkEntry: renders a single tool call -- icon based on type, heading, preview text, status (running/completed), duration
- WorkGroup: groups consecutive tool calls into a collapsible card with "Operations (N)" header
- DiffPreview: inline card showing file path, +/- stats, first ~20 lines of patch with color coding (green for +, red for -), expandable to load full diff from payload via GetPayloadData binding
- CommandOutput: expandable card showing command, exit code, preview lines, expandable to full output via GetPayloadData
- diff.ts: utility to parse diff lines for color coding
**Tests**:
- `npm run check` passes
- Playwright MCP: verify DiffPreview renders correctly with sample diff data (may need to manually insert test data)
**Done when**: tool calls, diffs, and command outputs render correctly with expand/collapse

#### 22. Composer and controls

**Spec sections**: Forge UI reference (composer section)
**Package**: frontend
**Files**: `frontend/src/lib/components/Composer.svelte`, `frontend/src/lib/components/ComposerControls.svelte`
**Deliver**:
- Composer: textarea with send button, Enter to send (Shift+Enter for newline)
- Send button: disabled when empty or session not active, shows stop icon when turn is running
- Stop button: calls InterruptTurn when clicked during active turn
- ComposerControls: model display (from thread.model), session status indicator, provider label
- Calls SendMessage binding on submit
**Tests**:
- `npm run check` passes
- Playwright MCP: navigate to a thread, type in composer, click send, verify message appears in timeline
**Done when**: user can type and send messages, stop button works during turns

#### 23. Approval prompt

**Spec sections**: Forge UI reference (approval flow)
**Package**: frontend
**Files**: `frontend/src/lib/components/ApprovalPrompt.svelte`
**Deliver**:
- Renders above the composer when pendingApprovals is non-empty
- Shows tool name, description, input details
- "Approve" button (primary), "Deny" button (destructive), "Always allow" button (secondary)
- Calls RespondToApproval binding with appropriate decision string
- Removes from pendingApprovals on response
**Tests**:
- `npm run check` passes
- Playwright MCP: simulate an approval by manually triggering the store mutation, verify prompt renders with correct buttons
**Done when**: approval prompts appear and respond correctly

#### 24. Background tray and status bar

**Spec sections**: Forge UI reference (background tray, status indicators)
**Package**: frontend
**Files**: `frontend/src/lib/components/BackgroundTray.svelte`, `frontend/src/lib/components/StatusBar.svelte`
**Deliver**:
- BackgroundTray: collapsible section above composer, shows running background tasks with progress indicator, collapses when empty
- StatusBar: bottom bar or inline indicator showing session status (connected/running/error/disconnected), token usage (input/output tokens), model name
- Token usage updates reactively from tokenUsage store
**Tests**:
- `npm run check` passes
- Playwright MCP: verify status bar renders on the page with session status
**Done when**: background tray and status bar render correctly, update reactively

### Phase 6: Integration (item 25)

#### 25. End-to-end integration testing

**Spec sections**: All
**Package**: all
**Files**: multiple (bug fixes as needed)
**Deliver**:
- Run `wails dev`, verify app launches
- Create a thread with Claude provider, send a message, verify:
  - User message appears in timeline
  - Streaming assistant text appears
  - Tool calls render as work entries
  - Diffs render with previews
  - Turn completes, status returns to ready
  - Thread persists across app restart (stop wails dev, restart, verify thread + messages load)
- Create a thread with Codex provider, repeat the above
- Test approval flow: configure provider to require approvals, trigger a tool use, verify prompt appears, approve, verify tool continues
- Test thread switching: create two threads, switch between them, verify state replacement (no accumulation)
- Fix any bugs found during integration testing
- Use Playwright MCP for all UI verification
**Tests**:
- All existing Go tests pass
- `npm run check` passes
- Manual + Playwright verification of the above flows
**Done when**: the app works end-to-end with both providers, persists across restarts, and handles thread switching cleanly

---

## Review Phase

When all work items are complete, enter the Review Phase. This is NOT optional. You NEVER write "Loop Complete" or "Loop Done" in the progress file. The human decides when the loop is done.

### Review Iteration Workflow

1. Read progress.md -- check "Known Issues" and "Review Log"
2. If known issues exist, fix ALL known issues (highest severity first)
3. If no known issues, perform a FULL SWEEP of one category:
   a. Scan thoroughly -- search every relevant file, grep for patterns, compare against spec
   b. Collect ALL findings for this category before fixing anything
   c. Fix everything you found
   d. Write/fix tests for all changes
4. Run quality gate
5. Commit all fixes with descriptive message
6. Update progress.md

### Review Categories (cycle through)

1. **Spec Compliance** -- Compare all types, interfaces, methods against IMPLEMENTATION.md
2. **Error Handling** -- Find every discarded error, every `_ = err`. Only acceptable for: error unwind cleanup, documented stdlib idioms, spec-designated non-critical (cite section)
3. **Test Coverage** -- Functions below 80%, missing edge case tests
4. **Code Consistency** -- Same patterns across all packages
5. **Dead Code** -- Unused exports, unreferenced types, implemented-but-unwired components
6. **Integration Wiring** -- Every component started/registered/connected. Every implemented adapter instantiated.
7. **Security & Data Integrity** -- Injection, unsafe fallbacks, data corruption risks

### Review Rules

- Known Issues are #1 priority
- One category per iteration, sweep completely
- Be adversarial -- zero findings means you didn't look hard enough
- No rubber stamps without spec citations
- "Noted but not fixed" IS a defect
- Dead code is a defect
- No self-referencing prior reviews
- NEVER mark the loop as complete

## Reminders

- **Wire format validation is mandatory.** Test against real claude and codex binaries before committing protocol code. Save captured output as test fixtures.
- **Go is a triage pipe.** No orchestration, no event sourcing, no in-memory read models. If you're building business logic in Go, you're doing it wrong.
- **Provider process is the source of truth during a turn.** Don't duplicate its state.
- **Persist per-item on completion, not per-turn.** Text accumulates in the triage layer and is persisted on turn_complete.
- **Frontend memory is bounded by one thread.** Thread switch = full state replacement.
- **Heavy payloads are always on-demand.** meta column for previews, data column only on explicit load.
- **Use Playwright MCP for frontend testing.** Don't just assume components render -- navigate to the dev server, take snapshots, click elements.
- **Reference llmkit at /Users/randy/repos/llmkit/ for protocol implementations.** Port what's valuable, don't copy blindly.
- **Reference forge at /Users/randy/repos/forge/ for UI patterns.** Copy the visual approach, not the architecture.
- **Read progress.md.** Every iteration. No exceptions.
- **Known Issues in progress.md are your #1 priority.**
- **If you build it, wire it.** No dead code.
- **NEVER write "Loop Complete" in the progress file.**
