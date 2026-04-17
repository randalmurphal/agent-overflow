# PROMPT: agent-overflow backend (Loop 1)

## Housekeeping

- Ignore `ralph-*.log`, `coverage.out`, `*.test`, `build/`
- Pre-existing uncommitted changes are not the agent's problem

## Prime Directive

This loop builds ALL backend Go code for feature parity with Forge. No frontend changes.

Scope: protocol completeness, session lifecycle, settings (JSON file), cost calculation, database migrations, structured logging, git operations (full including worktrees), discussion backend, design mode backend, provider detection, code reorganization.

### Authority Hierarchy

1. ARCHITECTURE.md -- behavioral authority
2. IMPLEMENTATION.md + IMPLEMENTATION-PARITY.md -- implementation authority
3. This PROMPT file -- work items and rules

### Mission

Build every backend capability needed for full Forge parity:
- Protocol completeness for both Claude CLI and Codex app-server (new event types, structured approvals, diff accumulation fix)
- Session lifecycle improvements (auto-resume, reconnect, health monitoring, thread title generation)
- Settings service backed by a JSON file with sparse serialization
- Cost calculation from token usage with per-model pricing
- Database migration system with versioned schema changes
- Git operations (status, diff, branches, commit, push, pull, worktrees, GitHub CLI)
- Discussion system (registry, channels, deliberation engine)
- Design mode backend (artifact storage, system prompts, reactor, Codex MCP server)
- Structured NDJSON logging with rotation
- Provider binary detection and version checking
- Model registry per provider
- All new Wails bindings wired and callable from frontend

## Rules of Engagement

### Non-Negotiable

1. Read progress-backend.md first every iteration
2. MUST read IMPLEMENTATION-PARITY.md sections before implementing each work item
3. MUST read referenced forge files when the spec says "Reference: ~/repos/forge/..."
4. MUST run quality gate after every work item
5. No God files -- max ~400 lines per file
6. No God functions -- max ~60 lines per function
7. One concern per file
8. Proper nested directories for 3+ files with same concern
9. All existing tests must continue to pass
10. New code must have tests (80% coverage floor)
11. Do NOT modify frontend files (anything under `frontend/`)
12. Do NOT create README.md or documentation files
13. When renaming files, update all imports and test files
14. Test in /tmp if you need to validate raw protocol behavior with real CLI output
15. Read ~/repos/forge source code when you need to understand how a feature works in practice
16. Research Codex app-server docs and Claude CLI docs when implementing protocol changes

### Prohibited

- Modifying any file under `frontend/`
- Adding TODO/FIXME comments
- Skipping tests
- Guessing at protocol formats -- read forge or test with real CLI
- Creating abstractions that aren't needed by the current work items
- Blame-shifting on quality gate failures
- No event sourcing, no orchestration engine -- Go is a triage pipe
- No in-memory read models or state duplication -- SQLite is the history cache, provider is the runtime truth
- No fmt.Println for debugging -- use log.Printf
- No custom error types -- use standard error wrapping
- No global mutable state -- dependency injection only
- No dead code -- if you build it, wire it
- No deferrals -- do it now or cite why it's literally impossible

## Environment

- Language: Go 1.25
- Module: agent-overflow
- Working directory: ~/repos/agent-overflow
- Database: SQLite via modernc.org/sqlite (`:memory:` for Go tests)
- Specs: ARCHITECTURE.md, IMPLEMENTATION.md, IMPLEMENTATION-PARITY.md
- Reference repos:
  - ~/repos/forge -- Node.js predecessor (read for behavior, not architecture)
  - ~/repos/llmkit -- Go LLM library (read for pricing, protocol references)

## Quality Gate

```sh
go build ./... && go vet ./... && go test -coverprofile=coverage.out ./... -count=1
```

Must pass before every commit. 80% coverage floor on `internal/` packages.

## Workflow Per Iteration

1. Read progress-backend.md for known issues -- fix ALL (highest severity first)
2. Pick next incomplete work item
3. Read the relevant IMPLEMENTATION-PARITY.md sections
4. Read referenced forge files if spec says to
5. Implement the feature
6. Write tests (80% coverage minimum for new code)
7. Run quality gate
8. Commit with descriptive message
9. Update progress-backend.md
10. Repeat

## Work Items

### Phase 0: Code Reorganization & Migration System

#### WI-0.1: Database migration system

Create a versioned migration system. Move existing DDL into migration v1. Add all new tables as migration v2.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 2.1-2.7, IMPLEMENTATION.md Section 3
**Target files**: `internal/store/migrate.go`, `internal/store/store.go`
**Deliver**:
- `Migration` struct with Version, Name, SQL fields
- `migration_versions` table for tracking applied migrations
- Migration runner: create tracking table if not exists, query max version, run unapplied migrations in a transaction
- Migration v1: existing DDL from store.go (threads, items, payloads tables with indexes)
- Migration v2: all new tables from IMPLEMENTATION-PARITY.md (channels, channel_messages, discussion_definitions, design_artifacts, thread column additions)
- Update `store.New` to use migration runner instead of inline DDL
**Tests**:
- Migration runs on fresh DB (all tables created)
- Migration runs on existing DB (idempotent -- v1 skipped if already applied)
- Version tracking works (migration_versions populated correctly)
- New tables from v2 created correctly
- Thread table has new columns after v2
**Done when**: `go test ./internal/store/...` passes with migration system

#### WI-0.2: Backend file reorganization

Rename files to match IMPLEMENTATION-PARITY.md directory structure. Update all imports.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 1.2
**Target files**: all renamed files + their test files
**Deliver**:
- Rename `internal/provider/claude/claude.go` to `internal/provider/claude/session.go`
- Rename `internal/provider/codex/codex.go` to `internal/provider/codex/session.go`
- Rename `internal/triage/triage.go` to `internal/triage/router.go`
- Create new empty package directories with doc.go files: `internal/settings/`, `internal/discussion/`, `internal/design/`, `internal/git/`, `internal/logging/`
- Update all imports and test files
**Tests**: all existing tests pass after rename
**Done when**: `go build ./... && go test ./...` passes

---

### Phase 1: Protocol Completeness

#### WI-1.1: Claude protocol additions

Handle previously-skipped system subtypes: tool_progress, compact_boundary, api_retry.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 5.1
**Target files**: `internal/provider/claude/protocol.go`
**Reference**: Read forge's Claude adapter for how these events are structured
**Deliver**:
- `tool_progress` subtype: emit `EventToolProgress` with progress data (current, total, message) in Meta. Include `itemId` if present.
- `compact_boundary` subtype: emit `EventCompactBoundary`. Extract context window data if present.
- `api_retry` subtype: emit `EventSessionStatus` with status "retrying" and retry metadata.
**Tests**:
- ParseLine test for tool_progress with progress data
- ParseLine test for compact_boundary with context window data
- ParseLine test for api_retry with retry metadata
- Verify correct EventKind and Meta for each
**Done when**: all new Claude events parsed and routed correctly

#### WI-1.2: Codex protocol additions

Handle previously-skipped notifications: reasoning deltas, thread/name/updated, rate limits, model rerouted, thread compacted.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 5.2
**Target files**: `internal/provider/codex/protocol.go`
**Reference**: `~/repos/forge/apps/server/src/codexAppServerManager.ts`
**Deliver**:
- `item/reasoning/textDelta` and `item/reasoning/summaryTextDelta`: emit `EventThinking` with delta text in Content
- `thread/name/updated`: emit `EventThreadRenamed` with new title in Meta
- `account/rateLimits/updated`: emit `EventRateLimits` with parsed rate limit entries
- `model/rerouted`: emit `EventModelRerouted` with new model name
- `thread/compacted`: emit `EventCompactBoundary`
**Tests**: ClassifyNotification test for each newly handled method, verify correct EventKind and Content/Meta
**Done when**: all new Codex notifications classified and routed

#### WI-1.3: Structured approval types

Extend ApprovalRequest and ApprovalResponse with new fields for structured user-input and permission approvals.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 3.1
**Reference**: `~/repos/forge/packages/contracts/src/interactiveRequest.ts`
**Target files**: `internal/provider/types.go`
**Deliver**:
- Add to ApprovalRequest: Kind, Questions, Permissions fields (keep ALL existing fields)
- Add to ApprovalResponse: Answers, Permissions, Scope fields (keep ALL existing fields)
- New types: UserInputQuestionOption, UserInputQuestion, PermissionProfile, NetworkPermissions, FileSystemPermissions
- New event kinds: EventToolProgress, EventCompactBoundary, EventRateLimits, EventModelRerouted, EventThreadRenamed
- New types: ContextWindow, RateLimitEntry, RateLimitsSnapshot
**Tests**: JSON serialization round-trip for all new types, verify existing type tests still pass
**Done when**: types compile, existing tests pass, new type tests pass

#### WI-1.4: Codex structured user-input and permission handling

Parse `item/tool/requestUserInput` params into Questions. Parse `item/permissions/requestApproval` into PermissionProfile.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 5.2
**Target files**: `internal/provider/codex/session.go` (handleServerRequest)
**Reference**: `~/repos/forge/apps/server/src/codexAppServerManager.ts` (search for requestUserInput, requestApproval)
**Deliver**:
- Parse `requestUserInput` params to extract questions array, build ApprovalRequest with Kind "user-input" and Questions populated
- Parse `requestApproval` params to extract reason and permissions profile, build ApprovalRequest with Kind "permission" and Permissions populated
**Tests**: handleServerRequest tests with user-input and permission request fixtures
**Done when**: structured approval requests emitted with correct Kind/Questions/Permissions

#### WI-1.5: Diff accumulation fix

Fix duplicate payloads from Codex `turn/diff/updated` which carries cumulative diffs.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 5.2 (diff accumulation fix)
**Target files**: `internal/provider/types.go`, `internal/provider/codex/protocol.go`, `internal/store/payloads.go`, `internal/triage/router.go`
**Deliver**:
- Add `Replace bool` field to ProviderEvent
- Implement `UpsertTurnPayload` in store -- `INSERT OR REPLACE` keyed on (thread_id, turn_index, kind='diff')
- In codex protocol, set Replace=true for `turn/diff/updated`
- In triage router, use UpsertTurnPayload when Replace is true
**Tests**:
- Verify UpsertTurnPayload replaces existing payload for same turn+kind
- Verify non-replace creates new payload (existing behavior preserved)
**Done when**: turn/diff/updated produces single payload per turn, not duplicates

#### WI-1.6: Triage router updates

Add routing for all new event kinds. Add reasoning accumulator buffer for Codex thinking deltas.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 5.3
**Target files**: `internal/triage/router.go`
**Deliver**:
- Route `EventToolProgress` -- emit inline
- Route `EventCompactBoundary` -- emit inline
- Route `EventRateLimits` -- emit inline
- Route `EventModelRerouted` -- emit inline + update thread model in store
- Route `EventThreadRenamed` -- emit inline + update thread title in store
- Add `reasoningAccumulators map[string]string` to Router (like textAccumulators)
- Accumulate `EventThinking` from Codex reasoning deltas into reasoning buffer
- On `EventTurnComplete`, persist accumulated reasoning as ItemThinking payload, then clear buffer
**Tests**: Handle test for each new event kind, reasoning buffer accumulation and persistence on turn complete
**Done when**: all new events routed correctly, reasoning buffer works

---

### Phase 2: Session Lifecycle & Settings

#### WI-2.1: Provider binary detection

Detect installed provider binaries and their versions.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 6.3, Section 3.4
**Target files**: `internal/provider/detect.go`, `app.go`
**Deliver**:
- `DetectProvider(name, binaryPath string) ProviderStatus`
- `DetectClaudeVersion(binaryPath string) (string, error)` -- runs `claude --version`
- `DetectCodexVersion(binaryPath string) (string, error)` -- runs `codex --version`
- `GetProviderStatuses` binding in app.go
**Tests**: detect with mock binary paths, version string parsing, not-found handling
**Done when**: GetProviderStatuses returns correct status for both providers

#### WI-2.2: Settings service (JSON file)

File-based settings with sparse serialization and atomic writes.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 7, Section 3.5
**Target files**: `internal/settings/settings.go`, `internal/settings/provider.go`
**Reference**: `~/repos/forge/apps/server/src/serverSettings.ts`
**Deliver**:
- `Service` struct with path, mutex, cached settings
- `NewService(configDir string)` constructor
- `Get() Settings` -- read from cache, reload from file if stale, merge with defaults
- `Update(patch map[string]any) (Settings, error)` -- merge, validate, sparse-serialize, atomic write (temp + rename)
- `AddRecentWorkspace(path string)` -- push to front, cap at 10, dedup
- `DefaultSettings` variable with all default values from spec
- `Path() string` accessor
**Tests**:
- Get returns defaults on missing file
- Update persists and sparse-serializes (only non-default values written)
- Concurrent read/write safety
- AddRecentWorkspace deduplicates and caps at 10
- Malformed JSON file returns defaults with no error
**Done when**: settings service reads/writes JSON file correctly

#### WI-2.3: Model registry

Known models per provider with capabilities.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 3.3
**Reference**: `~/repos/forge/packages/contracts/src/model.ts` for current model slugs
**Target files**: `internal/provider/models.go`
**Deliver**:
- `ModelInfo` struct with Slug, Name, Provider, Capabilities
- `ClaudeModels` and `CodexModels` slices with known models
- `ModelsForProvider(provider string) []ModelInfo`
**Tests**: ModelsForProvider returns correct lists for claude, codex, and unknown provider
**Done when**: model registry works for both providers

#### WI-2.4: Cost calculation

Token-to-cost calculation with per-model pricing table.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 8, Section 3.2
**Reference**: `~/repos/llmkit/cost.go` for pricing values
**Target files**: `internal/provider/cost.go`, `internal/triage/router.go`
**Deliver**:
- `ModelPricing` struct with InputPerMToken, OutputPerMToken, CacheReadPerMToken
- `KnownPricing` map keyed by model slug
- `CalculateCost(model string, usage TokenUsage) float64`
- Integrate into triage: after EventTokenUsage, calculate cost and attach to event before emitting
**Tests**: cost calculation for known models, zero cost for unknown models, cache read pricing
**Done when**: TokenUsage events include calculated cost

#### WI-2.5: Session lifecycle improvements

Auto-resume on thread switch, reconnect, health monitoring.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 6.1, Section 6.2
**Target files**: `app.go`, `internal/provider/claude/session.go`, `internal/provider/codex/session.go`
**Deliver**:
- `SwitchThread(threadID string) (store.Thread, error)` binding -- auto-resumes if thread has session_ref but no active session
- `ReconnectSession(threadID string) error` binding -- stops old session, starts fresh (resumes via session_ref)
- Health monitoring in readLoop: emit EventSessionStatus with status "error" on unexpected subprocess exit
**Tests**:
- SwitchThread with existing session_ref triggers auto-resume
- ReconnectSession stops old and starts new
- Unexpected process exit emits error status
**Done when**: thread switch auto-resumes, reconnect works, provider crash emits error status

#### WI-2.6: Thread title auto-generation

Automatic thread titles from Codex events and Claude heuristics.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 6.4
**Target files**: `internal/provider/codex/protocol.go`, `internal/triage/router.go`, `app.go`
**Deliver**:
- Codex: handle `thread/name/updated` -- update thread title in store + emit EventThreadRenamed
- Claude: after first turn completes, if thread title is still "New Thread", generate title from first user message (truncate to 50 chars, first sentence or line)
**Tests**: Codex thread rename updates store, Claude title generation from first message
**Done when**: threads get meaningful titles automatically

#### WI-2.7: Updated Wails bindings

Wire all new services and bindings into app.go.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 7.3, Section 6.1, Section 6.2
**Target files**: `app.go`
**Deliver**:
- Update `RespondToApproval` to accept `ApprovalResponse` struct (replaces string-only signature)
- Add `GetSettings`, `UpdateSettings` bindings wired to settings service
- Add `GetProviderStatuses` binding
- Add `GetModelsForProvider` binding
- Add `SwitchThread`, `ReconnectSession` bindings
- Wire settings service into `app.startup`
**Tests**: build passes, bindings generate correctly
**Done when**: all new bindings compile and are callable from frontend

#### WI-2.8: Thread struct expansion

Add new fields to store.Thread and update all CRUD operations.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 2.7
**Target files**: `internal/store/store.go`, `internal/store/threads.go`
**Deliver**:
- Add to Thread struct: InteractionMode, Branch, WorktreePath, ProjectPath, DiscussionID, ParentThreadID
- Update CreateThread to write new columns
- Update GetThread to read new columns
- Update ListThreads to include new columns
- Update UpdateThread to handle new columns
**Tests**: CreateThread with new fields, GetThread returns all fields, ListThreads works
**Done when**: thread CRUD works with all new columns

---

### Phase 3: Git Operations

#### WI-3.1: Git core

Git command execution wrapper with timeout and output limits.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 9.1, Section 9.2, Section 3.8
**Reference**: `~/repos/forge/apps/server/src/git/Layers/GitCore.ts`
**Target files**: `internal/git/core.go`, `internal/git/status.go`
**Deliver**:
- `Core` struct with timeout (30s default)
- `Execute(cwd string, args ...string) (stdout, stderr string, err error)` -- timeout, 1MB output limit
- `Status(cwd string) (GitStatus, error)` -- parse `git status --porcelain=v2 --branch`
- `WorkingTreeDiff(cwd string) (string, error)` -- `git diff HEAD` + `git diff --cached`
- `ListBranches(cwd string) ([]GitBranch, error)` -- parse `git branch -a` output
**Tests**: Status parsing from porcelain output, branch list parsing, timeout handling
**Done when**: git status/diff/branches work correctly

#### WI-3.2: Git actions

Commit, push, pull, checkout, create branch.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 9.3
**Target files**: `internal/git/actions.go`
**Deliver**:
- `Commit(cwd, subject, body string) (commitSha string, err error)` -- git add -A && git commit
- `Push(cwd string) error` -- with --set-upstream if no upstream
- `Pull(cwd string) error` -- `git pull --ff-only`
- `Checkout(cwd, branch string) error`
- `CreateBranch(cwd, name string) error`
**Tests**: commit creates commit in temp git repo, push/pull error handling, checkout branch
**Done when**: basic git operations work

#### WI-3.3: Git worktree operations

Create, remove, and list git worktrees.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 9 (worktree section)
**Reference**: `~/repos/forge/apps/server/src/git/Layers/GitCore.ts` (createWorktree, removeWorktree)
**Target files**: `internal/git/core.go` (add worktree methods)
**Deliver**:
- `CreateWorktree(cwd, path, branch string) error` -- `git worktree add <path> -b <branch>`
- `RemoveWorktree(cwd, path string) error` -- `git worktree remove <path>`
- `ListWorktrees(cwd string) ([]Worktree, error)` -- parse `git worktree list --porcelain`
- `Worktree` struct with Path, Branch, HEAD fields
**Tests**: create/remove worktree in temp repo, list worktrees parsing
**Done when**: worktree management works

#### WI-3.4: GitHub CLI integration

PR operations via `gh` CLI.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 9.4
**Reference**: `~/repos/forge/apps/server/src/git/Layers/GitHubCli.ts`
**Target files**: `internal/git/github.go`
**Deliver**:
- `CreatePR(cwd, title, body string) (url string, err error)` -- `gh pr create`
- `ListOpenPRs(cwd, head string) ([]GitPR, error)` -- `gh pr list --json url,number,title,state`
- Graceful handling when `gh` is not installed
**Tests**: parse PR list JSON output, handle gh not installed
**Done when**: PR operations work when gh is available

#### WI-3.5: Git Wails bindings

Wire all git operations to app.go bindings.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 9.5
**Target files**: `app.go`
**Deliver**:
- `GetGitStatus(threadID string) (GitStatus, error)`
- `GetWorkingTreeDiff(threadID string) (string, error)`
- `GitListBranches(threadID string) ([]GitBranch, error)`
- `GitCommit(threadID, subject, body string) (GitActionResult, error)`
- `GitPush(threadID string) (GitActionResult, error)`
- `GitPull(threadID string) (GitActionResult, error)`
- `GitCheckout(threadID, branch string) error`
- `GitCreateBranch(threadID, name string) error`
- `GitCreatePR(threadID, title, body string) (GitActionResult, error)`
- `GitCreateWorktree(threadID, branch string) (string, error)` -- returns worktree path
- `GitRemoveWorktree(threadID string) error`
- `GitListWorktrees(threadID string) ([]Worktree, error)`
- Resolve cwd from thread's ProjectPath (repo-level ops) or WorkspacePath (working-dir ops)
**Tests**: build passes
**Done when**: all git bindings wired and callable

---

### Phase 4: Discussion Backend

#### WI-4.1: Discussion store operations

SQLite persistence for discussion definitions and channels/messages.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 10.1, Section 10.2
**Target files**: `internal/store/discussions.go`, `internal/store/channels.go`
**Deliver**:
- Discussion definition CRUD: CreateDiscussionDef, GetDiscussionDef, ListDiscussionDefs, UpdateDiscussionDef, DeleteDiscussionDef
- Channel CRUD: CreateChannel, GetChannel, UpdateChannelStatus
- Channel message operations: InsertChannelMessage, ListChannelMessages
**Tests**: full CRUD cycle for discussions, channel message insertion and retrieval with ordering
**Done when**: persistence layer works for discussions and channels

#### WI-4.2: Discussion registry

Business logic layer for discussion definitions with validation.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 10.1
**Reference**: `~/repos/forge/apps/server/src/discussion/Services/DiscussionRegistry.ts`
**Target files**: `internal/discussion/registry.go`
**Deliver**:
- `Registry` struct with store dependency
- `List(scope string)`, `Get(name, scope string)`, `Create(def)`, `Update(prevName, prevScope, def)`, `Delete(name, scope)`
- Validation: name non-empty, at least 2 participants, each participant has role + system prompt
**Tests**: CRUD operations, validation errors for invalid definitions (missing name, <2 participants, missing role)
**Done when**: discussion definitions can be managed with validation

#### WI-4.3: Channel service

Channel message service with ordering and lifecycle.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 10.2
**Reference**: `~/repos/forge/apps/server/src/channel/Services/ChannelService.ts`
**Target files**: `internal/discussion/channel.go`
**Deliver**:
- `ChannelService` struct with store dependency
- `Create(threadID, channelType string) (Channel, error)`
- `PostMessage(input PostMessageInput) (ChannelMessage, error)` -- assigns next sequence number
- `GetMessages(channelID string, afterSeq, limit int) ([]ChannelMessage, error)`
- `Close(channelID string) error`
**Tests**: message ordering by sequence, sequence number auto-increment, close behavior
**Done when**: channel messaging works correctly

#### WI-4.4: Deliberation engine

Ping-pong turn management with conclusion tracking.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 10.3
**Reference**: `~/repos/forge/apps/server/src/channel/Services/DeliberationEngine.ts`
**Target files**: `internal/discussion/deliberation.go`
**Deliver**:
- `Deliberation` struct with state and mutex
- `NewDeliberation(channelID string, maxTurns int)`
- `RecordPost(participantThreadID string) (nextSpeaker string, shouldConclude bool)` -- alternates speakers, increments turn count
- `ProposeConclusionFrom(threadID, summary string) (allAgreed bool)` -- tracks conclusion proposals, unanimous required
- `State() DeliberationState`
**Tests**: turn alternation between participants, max turns triggers shouldConclude, unanimous conclusion proposals required
**Done when**: deliberation correctly manages turns and conclusions

#### WI-4.5: Discussion Wails bindings

Wire all discussion operations to app.go.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 10.4
**Target files**: `app.go`
**Deliver**:
- `ListDiscussions(scope string) ([]DiscussionDefinition, error)`
- `GetDiscussion(name, scope string) (DiscussionDefinition, error)`
- `CreateDiscussion(def DiscussionDefinition) error`
- `UpdateDiscussion(prevName, prevScope string, def DiscussionDefinition) error`
- `DeleteDiscussion(name, scope string) error`
- `StartDiscussion(threadID, discussionName string) error`
- `GetChannelMessages(channelID string, afterSeq, limit int) ([]ChannelMessage, error)`
- `PostChannelMessage(channelID, content string) error`
**Tests**: build passes
**Done when**: all discussion bindings wired

---

### Phase 5: Design Mode Backend

#### WI-5.1: Design artifact storage

SQLite metadata + filesystem HTML storage for design artifacts.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 11.1
**Reference**: `~/repos/forge/apps/server/src/design/artifactStorage.ts`
**Target files**: `internal/design/artifacts.go`, `internal/store/designs.go`
**Deliver**:
- `ArtifactStore` struct with baseDir
- `Store(threadID, html, title, description, kind string) (DesignArtifact, error)` -- write HTML to filesystem, metadata to SQLite
- `Get(threadID, artifactID string) (string, error)` -- returns HTML content from filesystem
- `List(threadID, kind string) ([]DesignArtifact, error)` -- query SQLite
- Storage layout: `<baseDir>/<threadID>/<artifactID>.html`
- Store-level operations: InsertDesignArtifact, GetDesignArtifact, ListDesignArtifacts
**Tests**: store artifact, retrieve by ID, list by thread, list by kind, filesystem cleanup
**Done when**: artifact storage works

#### WI-5.2: Design system prompt

Bundled default prompt with config override support.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 11.2
**Reference**: `~/repos/forge/apps/server/src/design/designSystemPrompt.ts`
**Target files**: `internal/design/prompts.go`
**Deliver**:
- `LoadDesignSystemPrompt(configDir string) string` -- load bundled default, check for override file
- Default prompt instructing agent to use `render_design` and `present_options` tools
**Tests**: default prompt loaded, override from file replaces default
**Done when**: design prompt available

#### WI-5.3: Design reactor

Design mode lifecycle management.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 11.3
**Reference**: `~/repos/forge/apps/server/src/design/DesignModeReactor.ts`
**Target files**: `internal/design/reactor.go`
**Deliver**:
- `Reactor` struct with artifact store dependency
- Handle design interaction mode lifecycle
- When provider renders HTML (via tool call), store artifact and emit event
- When provider presents options, store each option as artifact, create interactive request
- When user chooses option, resolve request and inform provider
**Tests**: artifact store on render event, option flow creates interactive request
**Done when**: design mode lifecycle works

#### WI-5.4: Codex MCP server for design tools

Lightweight HTTP MCP server providing render_design and present_options tools to Codex.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 11.3 (Codex MCP section)
**Target files**: `internal/provider/codex/mcp.go`
**Deliver**:
- HTTP MCP server that Codex connects to
- Register `render_design` and `present_options` as MCP tools
- Handle tool call requests, delegate to design reactor
- Server starts before Codex session and registers in `thread/start` mcpServers param
**Tests**: MCP tool call handling, response format validation
**Done when**: Codex can call design tools via MCP

#### WI-5.5: Design Wails bindings

Wire design operations to app.go.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 11.4
**Target files**: `app.go`
**Deliver**:
- `ListDesignArtifacts(threadID string) ([]DesignArtifact, error)`
- `GetDesignArtifactHTML(threadID, artifactID string) (string, error)`
- `ChooseDesignOption(threadID, requestID, optionID string) error`
**Tests**: build passes
**Done when**: all design bindings wired

---

### Phase 6: Operations

#### WI-6.1: Structured logging

NDJSON file logger with rotation.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 13.1
**Target files**: `internal/logging/logger.go`
**Deliver**:
- `Logger` struct with file, mutex, maxBytes
- `NewLogger(path string, maxBytes int64) (*Logger, error)`
- `Log(entry LogEntry) error` -- write NDJSON line
- `Close() error`
- `LogEntry` struct: Timestamp, Level, Component, Message, Data
- Rotation when file exceeds maxBytes (default 10MB), keep 3 rotated files
**Tests**: log entries written as NDJSON, rotation triggers at max bytes, old files cleaned up
**Done when**: structured logger works

#### WI-6.2: Provider event logging

Log raw provider stdin/stdout to NDJSON file when debug enabled.

**Spec refs**: IMPLEMENTATION-PARITY.md Section 13.2
**Target files**: `internal/provider/process.go`, `internal/logging/logger.go`
**Deliver**:
- Separate NDJSON log file: `<appDataDir>/logs/provider-events-<date>.ndjson`
- Each entry: `{ ts, threadId, direction: "in"|"out", provider, data }`
- Enable via `AGENT_OVERFLOW_DEBUG=provider` env var
- Wire into Process.ReadLine and Process.WriteLine
**Tests**: event logging captured when debug enabled, not captured when disabled
**Done when**: provider events logged to file

---

## Review Phase

After all work items are complete, enter an indefinite review/fix cycle:

1. Check progress-backend.md for known issues -- fix ALL (highest severity first)
2. If no known issues, do a FULL SWEEP of one category -- scan every relevant file, collect all findings, then fix everything:
   - **Spec compliance**: every type/method matches IMPLEMENTATION-PARITY.md
   - **Error handling**: no swallowed errors, no empty catch
   - **Test coverage**: 80% floor, missing edge cases
   - **Code consistency**: same patterns across packages
   - **Dead code**: unused exports, unreferenced types
   - **Integration wiring**: every component started/registered/connected
   - **Security**: injection, path traversal, unsafe operations
3. Run quality gate, commit all fixes together
4. Update progress-backend.md
5. Repeat forever

You NEVER write "Loop Complete" or "Loop Done" in the progress file. The human decides when the loop is done.

Known Issues are #1 priority. Before any category sweep, fix all Known Issues first.

No rubber stamps -- cannot mark findings as "INTENTIONAL" without citing specific spec section.
"Noted but not fixed" IS a defect. If you find it, fix it.
Dead code is a defect. If built but not wired, wire it or remove it.

## Reminders

- The existing IMPLEMENTATION.md is still authoritative for the foundation code
- IMPLEMENTATION-PARITY.md supplements it for new features
- Always read the spec section BEFORE implementing
- Always read referenced forge files when the spec says to
- Test protocol changes with real CLI output in /tmp if unsure
- No frontend changes in this loop -- Loop 2 handles all frontend work
- Project path is not workspace path. Project is git root, workspace is where provider operates.
- Codex reasoning deltas reuse EventThinking, not a separate event kind
- Settings are a JSON file, NOT SQLite
- Read progress-backend.md every iteration. No exceptions.
- Known Issues in progress-backend.md are your #1 priority.
- If you build it, wire it. No dead code.
- NEVER write "Loop Complete" in the progress file.
