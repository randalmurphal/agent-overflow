# PROMPT: agent-overflow advanced features (Loop 3)

## Housekeeping

- Ignore `ralph-*.log`, `coverage.out`, `node_modules/`, `dist/`
- Pre-existing uncommitted changes are not the agent's problem

## Prime Directive

This loop implements discussions (multi-agent deliberation) and design mode (visual artifact iteration) as full-stack features. Backend infrastructure (discussion registry, channels, deliberation engine, design artifacts, reactor) was built in Loop 1. Frontend shared components and core UI were built in Loop 2. This loop wires everything together with dedicated frontend components and integration testing.

### Authority Hierarchy

1. ARCHITECTURE.md -- behavioral authority
2. IMPLEMENTATION-PARITY.md -- implementation authority
3. This PROMPT file -- work items and rules

### Mission

Wire discussion and design mode features end-to-end:
- Discussion definitions can be created, edited, and deleted via a dedicated editor
- Starting a discussion spawns child threads (one per participant) with their own provider sessions
- A channel view shows the deliberation timeline with role badges, turn tracking, and conclusion flow
- Design-mode threads show a side panel rendering HTML artifacts in a sandboxed iframe
- Design option picking works via interactive cards in the composer area
- Interaction mode is selectable at thread creation time

## Rules of Engagement

### Non-Negotiable

1. Read progress-advanced.md first every iteration
2. MUST read IMPLEMENTATION-PARITY.md sections 10-11 before any work
3. MUST read referenced forge files for UI patterns and behavior before implementing the corresponding work item
4. Both Go and frontend changes are in scope
5. Svelte 5 runes exclusively -- no legacy reactive syntax
6. Quality gate: both Go and frontend must pass
7. Deliberation is lightweight coordination (ARCHITECTURE.md principle 2 exception)
8. Design mode must be set at thread creation time (before provider start)
9. Codex design tools use MCP (not dynamic tools) -- register in `thread/start` params under `mcpServers`
10. Test with Playwright MCP for UI verification
11. Test Go changes with `go test`
12. Reference repos: when implementing features, consult:
    - ~/repos/forge/apps/web/src/components/DiscussionEditor.logic.ts -- validation rules, default participants
    - ~/repos/forge/apps/web/src/components/WorkflowTimeline.parts.tsx -- channel UI patterns (role badges, transcript layout)
    - ~/repos/forge/apps/server/src/channel/Services/ChannelService.ts -- channel service shape (create, post, get messages)
    - ~/repos/forge/apps/server/src/channel/Services/DeliberationEngine.ts -- deliberation engine shape (initialize, record post, record conclusion, transitions)
    - ~/repos/forge/apps/server/src/design/DesignModeReactor.ts -- design lifecycle (setup, artifact rendering, option flow, MCP registration)

### Prohibited

- Modifying Loop 1 or Loop 2 files unless fixing a bug or adding a missing interface
- Adding orchestration logic beyond deliberation coordination (Go is triage + pipe, ARCHITECTURE.md principle 2)
- No event sourcing, no in-memory read models, no command/event split
- No TODO/FIXME comments -- finish the work or explain what's blocking
- No God files (>400 lines Go, >300 lines Svelte) or God functions (>60 lines)
- No fmt.Println for debugging -- use log.Printf
- No global mutable state -- dependency injection only
- No dead code -- if you build it, wire it
- No unverified test assertions
- No guessing at wire formats -- test against real providers where applicable

## Environment

- Working directory: /Users/randy/repos/agent-overflow
- Backend: Go 1.25
- Frontend: Svelte 5 / TypeScript, located at /Users/randy/repos/agent-overflow/frontend
- Module: agent-overflow
- Specs: ARCHITECTURE.md, IMPLEMENTATION-PARITY.md (sections 10, 11 primarily)
- Test framework: Go testing (`go test`), Playwright MCP for frontend
- Database: SQLite via modernc.org/sqlite (in-memory `:memory:` for Go tests)
- Reference repos: ~/repos/forge (UI patterns and behavior, NOT the architecture)

## Quality Gate

```bash
cd /Users/randy/repos/agent-overflow && go build ./... && go vet ./... && go test ./... -count=1 && cd frontend && npm run build && npm run check
```

Both Go and frontend must pass. Coverage must be increasing with every iteration on new packages.

For frontend items, also verify with Playwright MCP by navigating to the dev server and checking component rendering.

## Workflow Per Iteration

1. Read progress-advanced.md -- check Known Issues and Completed Work Items
2. If known issues exist, fix ALL known issues (highest severity first)
3. Pick next work item from the list below
4. Read spec sections + forge references listed in the work item
5. Implement (Go + Svelte as needed)
6. Run quality gate
7. Test with Playwright MCP if UI changes
8. Commit with descriptive message
9. Update progress-advanced.md
10. Repeat

## Work Items

### Phase 0: Discussion Frontend

#### WI-0.1: Discussion store

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

#### WI-0.2: DiscussionEditor

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

#### WI-0.3: ParticipantCard

**Package**: frontend
**Files**: `frontend/src/lib/components/discussion/ParticipantCard.svelte`
**Deliver**:
- Reusable card showing participant role, description, model selection
- Editable fields: role name (text input), description (text input), system prompt (textarea), provider picker, model picker
- Remove button (disabled if only 2 participants)
- Two-way binding: parent DiscussionEditor passes participant object, card edits in place
**Done when**: participant cards render and edit correctly, `npm run check` passes

#### WI-0.4: ChannelView

**Spec sections**: IMPLEMENTATION-PARITY.md Section 10.5
**Reference**: ~/repos/forge/apps/web/src/components/WorkflowTimeline.parts.tsx (role badges, transcript layout, message styling by role)
**Package**: frontend
**Files**: `frontend/src/lib/components/discussion/ChannelView.svelte`
**Deliver**:
- Timeline of channel messages, scrolling container with auto-scroll on new messages
- Each message shows: role badge (colored by participant, using formatRoleLabel pattern from forge), content rendered via Markdown component, timestamp
- Human input area at bottom: textarea + send button, calls PostChannelMessage binding
- Deliberation state display: turn counter ("Turn 3/20"), current speaker indicator, conclusion proposals section
- Empty state when no messages yet ("Waiting for participants...")
- Message styling: agent messages get background card style, human messages get primary tint, system messages get dashed border (matching forge WorkflowTimelineTranscriptPanel patterns)
**Done when**: channel messages display, human can post messages, deliberation state visible

#### WI-0.5: Discussion thread integration

**Spec sections**: IMPLEMENTATION-PARITY.md Section 10.4
**Target files**: `frontend/src/lib/components/sidebar/Sidebar.svelte` (discussion mode toggle), `frontend/src/lib/components/chat/ChatView.svelte` (channel view routing), `frontend/src/lib/stores/events.ts` (channel event subscription)
**Deliver**:
- When creating a thread with `interaction_mode "discussion"`, show discussion picker (dropdown of available definitions)
- On thread start with discussion mode, call `StartDiscussion` which creates child threads per participant
- Wire `provider:channel` events from backend to ChannelView via event listener
- ChatView routing: when the active thread is a discussion parent, show ChannelView instead of regular MessageTimeline
- Discussion threads are identifiable by `interactionMode === "discussion"` on the thread object
**Done when**: full discussion flow works end-to-end -- create discussion, start thread, see channel messages, post as human

### Phase 1: Discussion Backend Integration

#### WI-1.1: StartDiscussion implementation

**Spec sections**: IMPLEMENTATION-PARITY.md Section 10.3, 10.4
**Reference**: ~/repos/forge/apps/server/src/orchestration/Layers/DiscussionReactor.ts (discussion startup flow)
**Target files**: `app.go` (StartDiscussion binding), discussion coordination logic
**Deliver**:
- Full StartDiscussion flow: look up discussion definition by name, create parent thread (if not already the current thread), create channel for the thread, iterate participants and create a child thread for each (with `parent_thread_id` set, participant's system prompt, participant's provider/model), start a provider session for each child thread, initialize deliberation engine with the channel and maxTurns
- Each child thread gets `interaction_mode: "discussion"`, `parent_thread_id` pointing to the parent
- The deliberation engine is stored in a map keyed by channel ID
**Tests**: StartDiscussion creates channel + child threads, deliberation engine initialized with correct maxTurns
**Done when**: starting a discussion spawns all participant sessions, `go test` passes

#### WI-1.2: Discussion turn management

**Spec sections**: IMPLEMENTATION-PARITY.md Section 10.3
**Reference**: ~/repos/forge/apps/server/src/channel/Layers/DeliberationEngine.ts (record post, transitions, nudge/reinjection)
**Target files**: discussion coordination logic, channel service
**Deliver**:
- When a participant agent completes a turn, extract the assistant response and post it to the channel
- Call deliberation engine `RecordPost` to get next speaker and whether to conclude
- Send channel content (or the latest message) to the next speaker's session as user input
- Handle conclusion: when `shouldConclude` is true or all participants have proposed conclusions, close the channel and emit a conclusion event
- Handle max turns reached: force conclusion
**Tests**: turn alternation works correctly, conclusion triggered after max turns, conclusion requires all participants
**Done when**: multi-turn deliberation flows correctly, `go test` passes

#### WI-1.3: Channel event emission

**Target files**: discussion coordination logic, `frontend/src/lib/stores/events.ts`
**Deliver**:
- When a channel message is posted (via ChannelService.PostMessage), emit `provider:channel` event via app.Event.Emit with the ChannelMessage payload
- Frontend event listener subscribes to `provider:channel` and routes messages to the discussion store
- Discussion store appends new messages and triggers reactive updates in ChannelView
- Also emit deliberation state changes (turn count update, conclusion proposals) via `provider:channel` with a distinct type field
**Done when**: channel messages appear in real-time in ChannelView, `go test` and `npm run check` pass

### Phase 2: Design Mode Frontend

#### WI-2.1: DesignPanel

**Spec sections**: IMPLEMENTATION-PARITY.md Section 11.5
**Reference**: ~/repos/forge/apps/server/src/design/DesignModeReactor.ts (artifact rendering, storage patterns)
**Package**: frontend
**Files**: `frontend/src/lib/components/design/DesignPanel.svelte`
**Deliver**:
- Side panel (resizable via drag handle) showing the latest rendered design artifact in a sandboxed iframe (`sandbox="allow-scripts"`)
- Artifact history as thumbnails at the bottom of the panel (scrollable row), click to view older artifacts
- Auto-opens when the first artifact event arrives for a design-mode thread
- Panel header: artifact title, description, close button
- Empty state when no artifacts yet ("Waiting for design output...")
- Panel state: list of artifacts (from `ListDesignArtifacts` binding), active artifact ID, panel open/closed
- Artifact HTML loaded on demand via `GetDesignArtifactHTML` binding
**Done when**: design artifacts render in iframe, history navigable, `npm run check` passes

#### WI-2.2: DesignOptionPicker

**Spec sections**: IMPLEMENTATION-PARITY.md Section 11.5
**Reference**: ~/repos/forge/apps/server/src/design/DesignModeReactor.ts (option flow: present_options, pending choice, resolve)
**Package**: frontend
**Files**: `frontend/src/lib/components/design/DesignOptionPicker.svelte`
**Deliver**:
- When design options are presented (via event), shows cards for each option
- Each card: title, description, artifact preview thumbnail (small iframe or static preview)
- Click a card to choose it -- calls `ChooseDesignOption` binding with threadID, requestID, optionID
- Renders as an interactive request in the composer area (like approval prompts -- above the composer, blocking further input until resolved)
- After choice is made, card disappears and the conversation continues
**Done when**: design options selectable, choice sent to backend, picker dismisses after selection

#### WI-2.3: Design mode thread integration

**Spec sections**: IMPLEMENTATION-PARITY.md Section 11.3, 11.5
**Target files**: `frontend/src/lib/components/chat/ChatView.svelte`, `frontend/src/lib/components/sidebar/Sidebar.svelte` (design mode toggle), `frontend/src/lib/stores/events.ts`
**Deliver**:
- When creating a thread with `interaction_mode "design"`, backend injects design system prompt before first turn (already handled by design reactor from Loop 1)
- ChatView layout: when thread is design-mode, show DesignPanel alongside the regular MessageTimeline (side-by-side layout, panel on the right)
- Wire artifact events from backend (`provider:design-artifact` and `provider:design-options`) to DesignPanel state and DesignOptionPicker
- Wire option choice events back to backend via `ChooseDesignOption` binding
- DesignPanel auto-opens on first artifact, user can close/reopen
**Done when**: full design mode flow works end-to-end -- start design thread, artifacts render, options pickable

### Phase 3: Integration & Polish

#### WI-3.1: Discussion e2e test

**Tests**: start a discussion with 2 participants, verify channel messages appear in ChannelView, verify turn alternation (participant A speaks, then B, then A...), verify conclusion flow (max turns or unanimous proposal)
**Deliver**:
- If providers are available: full e2e test via Playwright MCP
- Regardless: Go integration test that starts a discussion, mocks provider responses, verifies channel message sequence and deliberation state transitions
**Done when**: discussion works end-to-end, tests pass

#### WI-3.2: Design mode e2e test

**Tests**: start a design-mode thread, verify artifact rendering in DesignPanel, verify option presentation and selection flow
**Deliver**:
- If providers are available: full e2e test via Playwright MCP
- Regardless: Go integration test that triggers design artifact storage and option flow, verifies artifact list and option resolution
**Done when**: design mode works end-to-end, tests pass

#### WI-3.3: Interaction mode UI

**Target files**: thread creation UI (Sidebar or dialog), `frontend/src/lib/components/sidebar/Sidebar.svelte`
**Deliver**:
- Clean interaction mode selector in thread creation flow
- Options: Default, Plan, Design, Discussion
- Each mode shows relevant sub-options:
  - Default: nothing extra
  - Plan: nothing extra
  - Design: nothing extra (design system prompt injected automatically)
  - Discussion: discussion definition picker (dropdown of available definitions)
- Selected mode is stored on the thread's `interactionMode` field
- Mode is immutable after thread creation (set once, before provider start)
**Done when**: interaction modes selectable, correctly configure threads, `npm run check` passes

---

## Review Phase

When all work items are complete, enter the Review Phase. This is NOT optional. You NEVER write "Loop Complete" or "Loop Done" in the progress file. The human decides when the loop is done.

### Review Iteration Workflow

1. Read progress-advanced.md -- check "Known Issues" and "Review Log"
2. If known issues exist, fix ALL known issues (highest severity first)
3. If no known issues, perform a FULL SWEEP of one category:
   a. Scan thoroughly -- search every relevant file, grep for patterns, compare against spec
   b. Collect ALL findings for this category before fixing anything
   c. Fix everything you found
   d. Write/fix tests for all changes
4. Run quality gate
5. Commit all fixes with descriptive message
6. Update progress-advanced.md

### Review Categories (cycle through)

1. **Spec Compliance** -- Compare all types, interfaces, methods against IMPLEMENTATION-PARITY.md sections 10-11
2. **Error Handling** -- Find every discarded error, every `_ = err`. Only acceptable for: error unwind cleanup, documented stdlib idioms, spec-designated non-critical (cite section)
3. **Test Coverage** -- Functions below 80%, missing edge case tests
4. **Code Consistency** -- Same patterns across all packages, same naming conventions Go + Svelte
5. **Dead Code** -- Unused exports, unreferenced types, implemented-but-unwired components
6. **Integration Wiring** -- Every component started/registered/connected. Every event emitted has a listener. Every binding called from frontend exists in Go.
7. **Visual Polish** -- Consistent styling, responsive layouts, accessible markup, no broken states

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

- **Deliberation is coordination, not orchestration** (ARCHITECTURE.md principle 2). The deliberation engine tracks turns and determines next speaker. It does NOT orchestrate provider behavior -- it coordinates between provider processes.
- **Design mode uses MCP tools for Codex, system prompt injection for Claude.** The MCP server is registered in `thread/start` params. Design system prompt is injected before the first turn. Both paths converge at the same Go design reactor.
- **Design mode must be set at thread creation time** (before provider start). The `interaction_mode` field on the thread is immutable after creation. MCP tools and system prompts must be registered before `thread/start`.
- **Channel messages are a NEW push event channel** (`provider:channel`), separate from `provider:event`. The event listener in `events.ts` must subscribe to this new channel and route messages to the discussion store.
- **Discussion participants are child threads** -- each gets their own provider session with the participant's system prompt and model. The parent thread's ChatView shows the ChannelView, not a regular MessageTimeline.
- **Frontend memory is bounded by one thread.** Thread switch = full state replacement. Discussion state (channel messages, deliberation state) is part of that replacement.
- **Heavy payloads are always on-demand.** Design artifact HTML is loaded via binding when the artifact is viewed, not eagerly.
- **Thread state is per-pane, NOT a singleton.** Components receive `pane: ThreadPane` as a prop. Discussion and design state should follow this pattern.
- **Reference forge for patterns, not architecture.** Forge uses event sourcing and Effect -- agent-overflow uses Go triage pipes and SQLite. Copy the visual/behavioral approach, not the structural approach.
- **Read progress-advanced.md.** Every iteration. No exceptions.
- **Known Issues in progress-advanced.md are your #1 priority.**
- **If you build it, wire it.** No dead code.
- **Project != workspace** (ARCHITECTURE.md principle 9). Don't confuse project_path with workspace_path.
- **NEVER write "Loop Complete" in the progress file.**
