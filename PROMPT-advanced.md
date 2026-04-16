# PROMPT: agent-overflow advanced features + parity (Loop 3)

## Housekeeping

- Ignore `ralph-*.log`, `coverage.out`, `node_modules/`, `dist/`
- Pre-existing uncommitted changes are not the agent's problem

## Prime Directive

This loop has two missions:

1. **Parity audit and fix**: Read the actual forge source code to discover behavioral gaps between forge and agent-overflow. When a gap is found, fix the agent-overflow code directly — do NOT just update a spec. If IMPLEMENTATION-PARITY.md is wrong or incomplete, update it too, but the code is what ships.

2. **Advanced features**: Wire discussions (multi-agent deliberation) and design mode (visual artifact iteration) end-to-end. Backend infrastructure was built in Loop 1. Frontend shared components and core UI were built in Loop 2. This loop wires everything together.

The parity audit runs first (Phase 0) because it may reveal bugs or missing features that affect the discussion/design work.

### Authority Hierarchy

1. **Forge behavior** -- the ground truth. Read forge source code. If forge does X and agent-overflow doesn't, that's a gap to fix.
2. **ARCHITECTURE.md** -- architectural constraints. Keep Go triage+pipe, no event sourcing, no orchestration (except deliberation). Never copy forge's architecture (Effect, event sourcing, read models). Copy its behavior.
3. **IMPLEMENTATION-PARITY.md** -- implementation reference. Useful but may be wrong or incomplete. If it conflicts with observed forge behavior, update the spec AND fix the code.
4. **This PROMPT file** -- work items and rules.

### The Parity Rule

When implementing ANY feature: read the corresponding forge source first. Do not guess at behavior based on the spec alone. The spec was written by an AI that may have missed nuance. The forge code is the authoritative reference for:
- What events to handle and how
- What the UI should look like and how it should behave
- What edge cases to handle
- What the user experience should be

### Mission

In priority order:
1. Fix all behavioral gaps between forge and agent-overflow (event processing, UI rendering, session lifecycle, thread management, git operations)
2. Wire discussion and design mode features end-to-end
3. Polish everything to forge-level quality

## Rules of Engagement

### Non-Negotiable

1. Read progress-advanced.md first every iteration
2. **Forge is truth**: before implementing any work item, read the referenced forge source files. Do not rely solely on IMPLEMENTATION-PARITY.md.
3. Both Go and frontend changes are in scope — this loop touches everything
4. Svelte 5 runes exclusively — no legacy reactive syntax
5. Quality gate: both Go and frontend must pass
6. Test with Playwright MCP for UI verification when `wails dev` is running
7. Test Go changes with `go test`
8. One work item per iteration
9. When you find a gap not listed in the work items, add it as a Known Issue in progress-advanced.md. If it's critical, fix it before continuing with scheduled work items.
10. When you find IMPLEMENTATION-PARITY.md is wrong, update the spec as part of the fix commit.

### Prohibited

- Adding orchestration logic beyond deliberation coordination (Go is triage + pipe, ARCHITECTURE.md principle 2)
- No event sourcing, no in-memory read models, no command/event split
- Copying forge's architecture (Effect layers, event sourcing, read models, worker queues) — copy its BEHAVIOR
- No TODO/FIXME comments — finish the work or explain what's blocking
- No God files (>400 lines Go, >300 lines Svelte) or God functions (>60 lines)
- No fmt.Println for debugging — use log.Printf
- No global mutable state — dependency injection only
- No dead code — if you build it, wire it
- No unverified test assertions
- No guessing at wire formats — read forge's actual protocol handling or test against real providers

### How to Research Forge

When a work item says "read forge file X", actually read it. Extract:
1. What data does this handle? (types, shapes, fields)
2. What edge cases does it cover? (error paths, fallbacks, validation)
3. What does the user see? (UI layout, text, colors, interactions)
4. What does it NOT do? (explicit skips, intentional omissions)

If you need to understand how forge processes a specific event or handles a specific scenario, test it:
- Write a throwaway test in /tmp that exercises the parsing/handling
- Read the actual Codex or Claude SDK docs at the reference URLs in AGENTS.md
- Cross-reference forge's handling with the provider's documentation

## Environment

- Working directory: /Users/randy/repos/agent-overflow
- Backend: Go 1.25
- Frontend: Svelte 5 / TypeScript, located at /Users/randy/repos/agent-overflow/frontend
- Module: agent-overflow
- Specs: ARCHITECTURE.md, IMPLEMENTATION-PARITY.md
- Test framework: Go testing (`go test`), Playwright MCP for frontend
- Database: SQLite via modernc.org/sqlite (in-memory `:memory:` for Go tests)
- Reference: ~/repos/forge (behavioral reference — read for patterns and behavior, NOT architecture)
- Codex docs: https://developers.openai.com/codex/sdk/#app-server
- Codex repo: https://github.com/openai/codex
- CodexMonitor: https://github.com/Dimillian/CodexMonitor

## Quality Gate

```bash
cd /Users/randy/repos/agent-overflow && go build ./... && go vet ./... && go test ./... -count=1 && cd frontend && npm run build && npm run check
```

Both Go and frontend must pass. Coverage must be increasing with every iteration on new packages.

## Workflow Per Iteration

1. Read progress-advanced.md — check Known Issues and Completed Work Items
2. If known issues exist, fix ALL known issues (highest severity first)
3. Pick next work item from the phases below (phases are ordered)
4. Read the forge source files referenced in the work item — understand what forge actually does
5. Implement: fix the gap, add the feature, or update behavior to match forge
6. If IMPLEMENTATION-PARITY.md is wrong or incomplete for this area, update it in the same commit
7. Run quality gate
8. Test with Playwright MCP if UI changes and `wails dev` is running
9. Commit with descriptive message
10. Update progress-advanced.md
11. Repeat

## Work Items

### Phase 0: Parity Audit — Event Processing

Fix all event processing gaps. For each item, read the forge source to understand what events are handled, then fix agent-overflow's handling to match.

#### WI-0.1: Codex approval decision translation — ALREADY FIXED

This was fixed in a prior commit (translate allow→accept, deny→decline). No work needed. Marked complete.

#### WI-0.2: Codex session state tracking

**Forge reference**: `~/repos/forge/apps/server/src/orchestration/Layers/ProviderRuntimeIngestion.ts` — search for `session.started`, `session.configured`, `session.state.changed`, `session.exited`
**Target**: `internal/provider/codex/protocol.go`, `internal/provider/codex/session.go`
**Gap**: Forge tracks full session lifecycle (connecting→ready→running→error→closed) from provider notifications. Agent-overflow only detects process exit crashes. Non-exit state transitions (`session.state.changed`) are silently skipped.
**Deliver**:
- Handle Codex `session.started`, `session.configured`, `session.state.changed`, `session.exited` notifications
- Map to appropriate `EventSessionStatus` values
- Frontend already handles `session_status` events — just need backend to emit them
**Done when**: Session state changes flow through to the frontend provider status banner

#### WI-0.3: Claude tool_complete and tool summary events

**Forge reference**: `~/repos/forge/apps/server/src/provider/Layers/claude/sdkMessageParsing.ts` — search for `item.completed` with tool item types, `tool_use_summary`
**Target**: `internal/provider/claude/protocol.go`
**Gap**: Claude tool result completions are not emitted as `EventToolComplete`. Tool use summaries are not surfaced.
**Deliver**:
- Emit `EventToolComplete` when Claude reports a tool result completion
- Handle `tool_use_summary` — surface as item metadata
**Done when**: Tool call lifecycle (start → progress → complete) renders correctly for Claude sessions

#### WI-0.4: Claude stream event subtypes

**Forge reference**: `~/repos/forge/apps/server/src/provider/Layers/claude/sdkMessageParsing.ts` — all stream event handling
**Target**: `internal/provider/claude/protocol.go`
**Gap**: Agent-overflow only extracts `data.delta.text` from stream events. Forge also handles tool input streaming and thinking deltas from stream events.
**Deliver**:
- Parse tool input deltas from stream events (tool call arguments being built)
- Parse thinking content from stream events
- Route through appropriate event kinds
**Done when**: Real-time tool input preview works during Claude streaming

#### WI-0.5: Stale interactive request detection

**Forge reference**: `~/repos/forge/apps/server/src/orchestration/Layers/ProviderCommandReactor.ts` lines 119-149 — "unknown pending approval request" handling
**Target**: `internal/provider/codex/session.go`, `internal/provider/claude/session.go`
**Gap**: After app restart with pending approvals, sending the response fails silently or with a generic error. Forge detects this and shows a specific "restart the turn" message.
**Deliver**:
- Detect stale request IDs (request ID not in pending map)
- Return a specific error that surfaces to the user: "Stale request — restart the turn to continue"
**Done when**: Stale approval responses show a helpful error instead of failing silently

### Phase 1: Parity Audit — UI Rendering

Fix all UI rendering gaps. For each item, read the forge component to understand what it renders and how, then build the equivalent in agent-overflow.

#### WI-1.1: Work group collapsing

**Forge reference**: `~/repos/forge/apps/web/src/components/chat/MessagesTimeline.logic.ts` lines 107-156 — work-group row grouping, `MAX_VISIBLE_WORK_LOG_ENTRIES`
**Target**: `frontend/src/lib/components/chat/MessageTimeline.svelte`
**Gap**: Forge groups consecutive tool entries into collapsible work-group rows with overflow (max 6 visible). Agent-overflow renders each tool as an individual WorkEntry without grouping.
**Deliver**:
- Group consecutive tool/command/diff items into collapsible work groups
- Show max 6 entries by default, "Show N more" expander
- Group header with summary (e.g., "5 tool calls")
**Done when**: Tool entries are grouped and collapsible, matching forge's visual pattern

#### WI-1.2: Turn-level diff summary

**Forge reference**: `~/repos/forge/apps/web/src/components/chat/MessagesTimeline.logic.ts` lines 169-177 — turn-diff row kind
**Target**: `frontend/src/lib/components/chat/MessageTimeline.svelte`, new `TurnDiffSummary.svelte`
**Gap**: Forge renders an aggregated diff summary card at the end of each turn showing all files changed with stats. Agent-overflow has per-item diffs but no turn-level summary.
**Deliver**:
- Aggregate file changes per turn boundary
- Render a collapsible summary card: file tree with +/- stats, click to expand individual diffs
- Use existing ChangedFilesTree component
**Done when**: Turn boundaries show aggregated diff summaries

#### WI-1.3: Background task/subagent detail

**Forge reference**: `~/repos/forge/apps/web/src/components/chat/SubagentHeading.tsx`, `~/repos/forge/apps/web/src/components/chat/LazySubagentEntries.tsx`, `~/repos/forge/apps/web/src/components/chat/ComposerBackgroundTaskTray.tsx`
**Target**: `frontend/src/lib/components/shared/BackgroundTray.svelte`
**Gap**: Agent-overflow shows background task IDs with a pulsing dot. Forge shows agent type, model, prompt preview, child activity feed.
**Deliver**:
- Parse background task metadata: agent type, model, description
- Show meaningful task descriptions instead of raw IDs
- Expandable detail showing child activity
**Done when**: Background tasks show descriptive info matching forge's level of detail

#### WI-1.4: MCP elicitation UI

**Forge reference**: `~/repos/forge/apps/web/src/components/chat/ComposerPendingMcpElicitationPanel.tsx`
**Target**: `frontend/src/lib/components/composer/ApprovalPrompt.svelte` or new `McpElicitationPanel.svelte`
**Gap**: Agent-overflow routes elicitation as a generic approval. Forge has a dedicated panel with URL mode, form questions, and elicitation-specific actions (approve/deny/cancel).
**Deliver**:
- Detect `kind === 'elicitation'` in approval events
- Render URL display for URL-type elicitations
- Render form questions for form-type elicitations
- Specific actions: Approve, Deny, Cancel
**Done when**: MCP elicitation requests render with full detail, not as generic approvals

#### WI-1.5: Enhanced permission panel

**Forge reference**: `~/repos/forge/apps/web/src/components/chat/ComposerPendingPermissionPanel.tsx`
**Target**: `frontend/src/lib/components/composer/ApprovalPrompt.svelte`
**Gap**: Forge has per-path checkboxes for filesystem read/write permissions, network toggle. Agent-overflow shows a simpler display.
**Deliver**:
- Per-path toggle checkboxes for filesystem permissions
- Network access toggle
- Turn/session scope remains
**Done when**: Permission requests show per-path controls matching forge

#### WI-1.6: Thread search and filtering

**Forge reference**: `~/repos/forge/apps/web/src/components/UnifiedThreadPicker.tsx`
**Target**: `frontend/src/lib/components/sidebar/Sidebar.svelte`, `frontend/src/lib/components/sidebar/ThreadList.svelte`
**Gap**: No search or filter in the thread list.
**Deliver**:
- Search input at top of thread list
- Filter threads by title match (client-side, no DB query needed)
- Clear button
**Done when**: Users can search threads by title

### Phase 2: Parity Audit — Session & Thread Lifecycle

#### WI-2.1: Thread archiving — add unarchive and listing

**Target**: `internal/store/threads.go` (add ListArchivedThreads, UnarchiveThread), `app.go` (add bindings), `frontend/src/lib/components/settings/ArchivedThreads.svelte`
**Gap**: Archiving is one-way. No way to list or unarchive archived threads.
**Deliver**:
- `ListArchivedThreads()` store method + Wails binding
- `UnarchiveThread(id)` store method + Wails binding
- Frontend ArchivedThreads.svelte: remove the blocker state, wire to real data
- Re-run `wails generate` to produce TypeScript bindings
**Done when**: Archived threads can be listed and unarchived

#### WI-2.2: Session restart for model changes

**Forge reference**: `~/repos/forge/apps/server/src/orchestration/Layers/ProviderCommandReactor.ts` lines 362-428 — session restart logic
**Target**: `app.go` or new binding
**Gap**: No way to change a thread's model or restart the session with new config.
**Deliver**:
- `UpdateThreadModel(threadID, model)` binding that updates the thread and restarts the provider session with the new model
- Wire ModelPicker to call this instead of just updating settings
- Carry conversation state via session resume if the provider supports it
**Done when**: Users can switch models mid-conversation

#### WI-2.3: Regenerate Wails bindings

**Target**: Run `wails generate` to pick up all new Go methods (discussion, design, archived threads, model update, etc.)
**Deliver**:
- All Go exported App methods have corresponding TypeScript bindings
- `stores/bindings.ts` re-exports all new bindings
- Discussion and design type files reference generated binding types where applicable
**Done when**: Frontend can call every backend method

### Phase 3: Discussion Frontend

(Existing items WI-0.1 through WI-0.5 renumbered)

#### WI-3.1: Discussion store

**Spec sections**: IMPLEMENTATION-PARITY.md Section 10.5, Section 4.2 (frontend types)
**Package**: frontend
**Files**: `frontend/src/lib/stores/discussions.svelte.ts`
**Deliver**:
- Reactive state for discussion definitions list, active discussion, active channel
- Load discussion list from `ListDiscussions` binding
- Create/update/delete discussions via bindings
- Track active channel ID and channel messages for the current discussion thread
- Channel message append (called from event listener)
**Done when**: discussion state management works, `npm run check` passes

#### WI-3.2: DiscussionEditor

**Spec sections**: IMPLEMENTATION-PARITY.md Section 10.5
**Reference**: ~/repos/forge/apps/web/src/components/DiscussionEditor.logic.ts (validation rules, default participants, sorting)
**Package**: frontend
**Files**: `frontend/src/lib/components/discussion/DiscussionEditor.svelte`
**Deliver**:
- Form for creating/editing discussion definitions
- Fields: name, description, scope (global/project), participants list with add/remove
- Per-participant: role name, description, system prompt, provider/model picker (reuse ModelPicker/ProviderPicker from Loop 2)
- Settings: maxTurns input
- Validation before save (mirroring forge rules: non-empty name, >=2 participants, each has role + system prompt + model, maxTurns >= 1, no name conflicts)
- Default empty discussion: 2 participants (advocate + critic) with default maxTurns of 20
- Call CreateDiscussion/UpdateDiscussion/DeleteDiscussion bindings
**Done when**: discussions can be created, edited, deleted; validation prevents invalid saves

#### WI-3.3: ParticipantCard

**Package**: frontend
**Files**: `frontend/src/lib/components/discussion/ParticipantCard.svelte`
**Deliver**:
- Reusable card showing participant role, description, model selection
- Editable fields: role name (text input), description (text input), system prompt (textarea), provider picker, model picker
- Remove button (disabled if only 2 participants)
- Two-way binding: parent DiscussionEditor passes participant object, card edits in place
**Done when**: participant cards render and edit correctly, `npm run check` passes

#### WI-3.4: ChannelView

**Spec sections**: IMPLEMENTATION-PARITY.md Section 10.5
**Reference**: ~/repos/forge/apps/web/src/components/WorkflowTimeline.parts.tsx (role badges, transcript layout, message styling by role)
**Package**: frontend
**Files**: `frontend/src/lib/components/discussion/ChannelView.svelte`
**Deliver**:
- Timeline of channel messages, scrolling container with auto-scroll on new messages
- Each message shows: role badge (colored by participant), content rendered via Markdown component, timestamp
- Human input area at bottom: textarea + send button, calls PostChannelMessage binding
- Deliberation state display: turn counter ("Turn 3/20"), current speaker indicator, conclusion proposals section
- Empty state when no messages yet ("Waiting for participants...")
- Message styling: agent messages get background card style, human messages get primary tint, system messages get dashed border
**Done when**: channel messages display, human can post messages, deliberation state visible

#### WI-3.5: Discussion thread integration

**Spec sections**: IMPLEMENTATION-PARITY.md Section 10.4
**Target files**: `frontend/src/lib/components/sidebar/Sidebar.svelte`, `frontend/src/lib/components/chat/ChatView.svelte`, `frontend/src/lib/stores/events.ts`
**Deliver**:
- When creating a thread with `interaction_mode "discussion"`, show discussion picker
- On thread start with discussion mode, call `StartDiscussion` which creates child threads per participant
- Wire `provider:channel` events from backend to ChannelView via event listener
- ChatView routing: when thread is a discussion parent, show ChannelView instead of regular MessageTimeline
**Done when**: full discussion flow works end-to-end

### Phase 4: Discussion Backend Integration

#### WI-4.1: Channel event emission

**Target files**: discussion coordination logic, `frontend/src/lib/stores/events.ts`
**Deliver**:
- When a channel message is posted, emit `provider:channel` event via app.Event.Emit
- Frontend event listener subscribes to `provider:channel` and routes to discussion store
- Emit deliberation state changes (turn count, conclusion proposals) via `provider:channel`
**Done when**: channel messages appear in real-time in ChannelView

#### WI-4.2: Discussion turn management

**Spec sections**: IMPLEMENTATION-PARITY.md Section 10.3
**Reference**: ~/repos/forge/apps/server/src/channel/Layers/DeliberationEngine.ts (record post, transitions)
**Deliver**:
- When participant agent completes a turn, post response to channel
- Call deliberation engine RecordPost to get next speaker
- Send channel content to next speaker's session as user input
- Handle conclusion: when shouldConclude or all participants proposed, close channel
- Handle max turns: force conclusion
**Done when**: multi-turn deliberation flows correctly

### Phase 5: Design Mode Frontend

#### WI-5.1: DesignPanel

**Spec sections**: IMPLEMENTATION-PARITY.md Section 11.5
**Reference**: ~/repos/forge/apps/server/src/design/DesignModeReactor.ts
**Files**: `frontend/src/lib/components/design/DesignPanel.svelte`
**Deliver**:
- Side panel with sandboxed iframe showing latest design artifact
- Artifact history thumbnails, click to view older
- Auto-opens on first artifact event
- Loads HTML on demand via `GetDesignArtifactHTML` binding
**Done when**: design artifacts render in iframe

#### WI-5.2: DesignOptionPicker

**Spec sections**: IMPLEMENTATION-PARITY.md Section 11.5
**Files**: `frontend/src/lib/components/design/DesignOptionPicker.svelte`
**Deliver**:
- Cards for each design option with title, description, preview
- Click to choose — calls `ChooseDesignOption` binding
- Renders as interactive request in composer area
**Done when**: design options selectable, choice sent to backend

#### WI-5.3: Design mode thread integration

**Target files**: ChatView.svelte, Sidebar.svelte, events.ts
**Deliver**:
- Design-mode thread shows DesignPanel alongside MessageTimeline
- Wire artifact and option events to DesignPanel state
- Wire option choices back via binding
**Done when**: full design mode flow works end-to-end

### Phase 6: Integration & Polish

#### WI-6.1: Interaction mode UI

**Target files**: thread creation UI (Sidebar.svelte)
**Deliver**:
- Interaction mode selector: Default, Plan, Design, Discussion
- Discussion mode shows definition picker
- Mode stored on thread, immutable after creation
**Done when**: modes selectable, correctly configure threads

#### WI-6.2: Discussion e2e test

Go integration test: start discussion, mock provider responses, verify channel messages and deliberation

#### WI-6.3: Design mode e2e test

Go integration test: trigger artifact storage and option flow, verify resolution

---

## Review Phase

When all work items are complete, enter the Review Phase. This is NOT optional. You NEVER write "Loop Complete" or "Loop Done" in the progress file. The human decides when the loop is done.

### Review Iteration Workflow

1. Read progress-advanced.md — check "Known Issues" and "Review Log"
2. If known issues exist, fix ALL known issues (highest severity first)
3. If no known issues, perform a FULL SWEEP of one category:
   a. For parity categories: read the forge code, compare with agent-overflow, find gaps
   b. Collect ALL findings for this category before fixing anything
   c. Fix everything you found
   d. Write/fix tests for all changes
4. Run quality gate
5. Commit all fixes with descriptive message
6. Update progress-advanced.md

### Review Categories (cycle through)

1. **Forge Event Parity** — Read forge's event ingestion pipeline. For each event type forge handles, verify agent-overflow handles it. For each event forge renders in the UI, verify agent-overflow renders it. This is NOT a spec check — read forge's actual code.
2. **Forge UI Parity** — Open forge's chat components. For each rendering path (message types, tool calls, diffs, errors, approvals, background tasks), verify agent-overflow renders equivalently. Screenshot or describe differences.
3. **Forge Session Parity** — Compare session lifecycle: start, resume, crash recovery, model switch, disconnect/reconnect. Read forge's ProviderCommandReactor for the full set of scenarios.
4. **Error Handling** — Find every discarded error, every `_ = err`, every empty catch block. Only acceptable for documented stdlib idioms.
5. **Test Coverage** — Functions below 80%, missing edge case tests
6. **Code Consistency** — Same patterns across all packages, same naming conventions Go + Svelte
7. **Dead Code** — Unused exports, unreferenced types, implemented-but-unwired components
8. **Integration Wiring** — Every component started/registered/connected. Every event emitted has a listener. Every binding called from frontend exists in Go.
9. **Visual Polish** — Consistent styling, responsive layouts, accessible markup, no broken states

### Review Rules

- Known Issues are #1 priority
- One category per iteration, sweep completely
- Parity reviews (categories 1-3) must read forge source code, not just the spec
- Be adversarial — zero findings means you didn't look hard enough
- No rubber stamps without spec citations OR forge code citations
- "Noted but not fixed" IS a defect
- Dead code is a defect
- No self-referencing prior reviews
- If you find the spec is wrong, fix the spec AND the code
- NEVER mark the loop as complete

## Reminders

- **Forge is truth for behavior.** Read forge code before implementing. The spec may be wrong.
- **Keep our architecture.** Go triage+pipe, SQLite, Wails v3 services. Never copy Effect layers, event sourcing, or read models. Copy what forge does, not how it's structured.
- **Deliberation is coordination, not orchestration** (ARCHITECTURE.md principle 2). The deliberation engine tracks turns. It does NOT orchestrate providers.
- **Design mode uses MCP tools for Codex, system prompt injection for Claude.** Both converge at the Go design reactor.
- **Design mode must be set at thread creation time** (before provider start). Immutable after creation.
- **Channel messages are a NEW push event channel** (`provider:channel`), separate from `provider:event`.
- **Discussion participants are child threads** — each gets their own provider session.
- **Frontend memory is bounded by one thread.** Thread switch = full state replacement.
- **Heavy payloads are always on-demand.** Load via binding when viewed, not eagerly.
- **Thread state is per-pane, NOT a singleton.** Components receive `pane: ThreadPane`.
- **Project != workspace** (ARCHITECTURE.md principle 9).
- **Read progress-advanced.md.** Every iteration. No exceptions.
- **Known Issues in progress-advanced.md are your #1 priority.**
- **If you build it, wire it.** No dead code.
- **NEVER write "Loop Complete" in the progress file.**
