# Agent-Overflow Feature Parity Specification

Supplements `IMPLEMENTATION.md`. Covers all features needed for full functional parity with Forge.
References Forge code at `~/repos/forge` for detailed behavior — agents MUST read referenced files.

---

## 1. Code Organization

### 1.1 Guidelines

- Max ~400 lines per Go file, ~300 lines per Svelte component
- One concern per file — if a file handles two distinct domains, split it
- Nested directories when 3+ files share a concern
- No God functions — break functions > 60 lines into named helpers
- Package-level doc comments on every package

### 1.2 Backend Directory Structure

```
internal/
├── provider/
│   ├── types.go              # shared event/item types (exists)
│   ├── process.go            # subprocess management (exists)
│   ├── cost.go               # NEW: token-to-cost calculation
│   ├── models.go             # NEW: known models per provider, capabilities
│   ├── detect.go             # NEW: binary detection + version checking
│   ├── claude/
│   │   ├── session.go        # rename claude.go
│   │   └── protocol.go       # exists
│   └── codex/
│       ├── session.go        # rename codex.go
│       ├── protocol.go       # exists
│       └── mcp.go            # NEW: MCP server for design mode tools
├── store/
│   ├── store.go              # DB init (exists)
│   ├── migrate.go            # NEW: versioned migration system
│   ├── threads.go            # exists
│   ├── items.go              # exists
│   ├── payloads.go           # exists
│   ├── settings.go           # NEW: settings CRUD
│   ├── channels.go           # NEW: channel + message persistence
│   ├── discussions.go        # NEW: discussion definition persistence
│   └── designs.go            # NEW: design artifact persistence
├── triage/
│   ├── router.go             # rename triage.go
│   └── meta.go               # exists
├── settings/
│   ├── settings.go           # NEW: settings service, defaults, validation
│   └── provider.go           # NEW: provider binary detection + status
├── discussion/
│   ├── registry.go           # NEW: discussion definition CRUD
│   ├── channel.go            # NEW: channel message service
│   └── deliberation.go       # NEW: ping-pong turn management
├── design/
│   ├── reactor.go            # NEW: design mode lifecycle
│   ├── artifacts.go          # NEW: HTML artifact storage
│   └── prompts.go            # NEW: design system prompt loader
├── git/
│   ├── core.go               # NEW: git command execution wrapper
│   ├── status.go             # NEW: status, diff, branch queries
│   ├── actions.go            # NEW: commit, push, stacked actions
│   └── github.go             # NEW: gh CLI integration
└── logging/
    └── logger.go             # NEW: structured NDJSON file logger
```

### 1.3 Frontend Directory Structure

```
frontend/src/lib/
├── stores/
│   ├── events.ts             # exists
│   ├── thread.svelte.ts      # exists
│   ├── threads.svelte.ts     # exists
│   ├── panes.svelte.ts       # exists
│   ├── settings.svelte.ts    # NEW
│   ├── discussions.svelte.ts # NEW
│   ├── toast.svelte.ts       # NEW
│   └── bindings.ts           # exists (expand)
├── components/
│   ├── chat/
│   │   ├── ChatView.svelte
│   │   ├── MessageTimeline.svelte
│   │   ├── UserMessage.svelte
│   │   ├── AssistantMessage.svelte
│   │   ├── StreamingMessage.svelte       # NEW: markdown-aware streaming
│   │   ├── WorkEntry.svelte
│   │   ├── DiffPreview.svelte
│   │   ├── CommandOutput.svelte
│   │   ├── ThinkingBlock.svelte          # NEW: expandable
│   │   ├── ChangedFilesTree.svelte       # NEW
│   │   ├── ContextWindowMeter.svelte     # NEW
│   │   ├── RateLimitsMeter.svelte        # NEW
│   │   └── ProviderStatusBanner.svelte   # NEW
│   ├── composer/
│   │   ├── Composer.svelte
│   │   ├── ComposerControls.svelte
│   │   ├── ModelPicker.svelte            # NEW
│   │   ├── ProviderPicker.svelte         # NEW
│   │   └── ApprovalPrompt.svelte
│   ├── sidebar/
│   │   ├── Sidebar.svelte
│   │   ├── ThreadList.svelte
│   │   ├── ThreadRow.svelte
│   │   └── WorkspacePicker.svelte        # NEW
│   ├── settings/
│   │   ├── SettingsView.svelte           # NEW: settings container
│   │   ├── GeneralSettings.svelte        # NEW
│   │   ├── ProviderSettings.svelte       # NEW
│   │   └── ArchivedThreads.svelte        # NEW
│   ├── discussion/
│   │   ├── DiscussionEditor.svelte       # NEW
│   │   ├── ChannelView.svelte            # NEW
│   │   └── ParticipantCard.svelte        # NEW
│   ├── design/
│   │   ├── DesignPanel.svelte            # NEW
│   │   └── DesignOptionPicker.svelte     # NEW
│   ├── git/
│   │   ├── BranchToolbar.svelte          # NEW
│   │   ├── GitActionsControl.svelte      # NEW
│   │   └── CommitDialog.svelte           # NEW
│   └── shared/
│       ├── Markdown.svelte               # move from components/
│       ├── CodeBlock.svelte              # NEW: shiki syntax highlighting
│       ├── CopyButton.svelte             # NEW
│       ├── ConfirmDialog.svelte          # NEW
│       ├── Toast.svelte                  # NEW
│       ├── StatusBar.svelte              # move from components/
│       └── BackgroundTray.svelte         # move from components/
├── types/
│   ├── events.ts             # exists (expand)
│   ├── models.ts             # exists (expand)
│   ├── settings.ts           # NEW
│   ├── discussion.ts         # NEW
│   ├── design.ts             # NEW
│   └── git.ts                # NEW
└── utils/
    ├── format.ts             # exists
    ├── diff.ts               # exists
    ├── clipboard.ts          # NEW
    ├── scroll.ts             # NEW
    └── shiki.ts              # NEW: shiki highlighter singleton
```

---

## 2. Database Schema Additions

### 2.1 Migration System

```sql
CREATE TABLE IF NOT EXISTS migration_versions (
    version  INTEGER PRIMARY KEY,
    name     TEXT    NOT NULL,
    applied  INTEGER NOT NULL DEFAULT (strftime('%s','now') * 1000)
);
```

Migration runner in `store/migrate.go`:
- Array of `Migration{Version int, Name string, SQL string}`
- On startup: create `migration_versions` if not exists, query max version, run unapplied migrations in order inside a transaction
- Existing DDL from `store.go` becomes migration v1

### 2.2 Settings (JSON File, NOT SQLite)

Settings are stored as a JSON file at `<appDataDir>/agent-overflow/settings.json`.
NOT in SQLite. This lets users edit settings when the app isn't running (e.g., fixing
a broken binary path). The file stores only non-default values (sparse).

```go
// Settings file path: <UserConfigDir>/agent-overflow/settings.json
// On read: parse JSON, merge with defaults
// On write: strip values matching defaults, write atomically (temp file + rename)
```

See Section 7 for the full settings schema and service.

### 2.3 Channels Table

```sql
CREATE TABLE IF NOT EXISTS channels (
    id          TEXT    PRIMARY KEY,
    thread_id   TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    type        TEXT    NOT NULL DEFAULT 'deliberation',
    status      TEXT    NOT NULL DEFAULT 'open',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_channels_thread ON channels(thread_id);
```

### 2.4 Channel Messages Table

```sql
CREATE TABLE IF NOT EXISTS channel_messages (
    id          TEXT    PRIMARY KEY,
    channel_id  TEXT    NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    sequence    INTEGER NOT NULL,
    from_type   TEXT    NOT NULL,
    from_id     TEXT    NOT NULL,
    from_role   TEXT,
    content     TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    UNIQUE(channel_id, sequence)
);
-- UNIQUE constraint creates an implicit index; no separate CREATE INDEX needed.
```

### 2.5 Discussion Definitions Table

```sql
CREATE TABLE IF NOT EXISTS discussion_definitions (
    id          TEXT    PRIMARY KEY,
    name        TEXT    NOT NULL,
    description TEXT    NOT NULL DEFAULT '',
    scope       TEXT    NOT NULL DEFAULT 'global',
    project_id  TEXT,
    definition  TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    UNIQUE(name, scope, project_id)
);
```

`definition` column stores the full JSON `DiscussionDefinition` (participants, settings).

### 2.6 Design Artifacts Table

```sql
CREATE TABLE IF NOT EXISTS design_artifacts (
    id          TEXT    PRIMARY KEY,
    thread_id   TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    title       TEXT    NOT NULL,
    description TEXT    NOT NULL DEFAULT '',
    kind        TEXT    NOT NULL DEFAULT 'render',
    html_path   TEXT    NOT NULL,
    created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_design_artifacts_thread ON design_artifacts(thread_id);
```

### 2.7 Thread Table Additions

Add columns to existing `threads` table via migration:

```sql
ALTER TABLE threads ADD COLUMN interaction_mode TEXT NOT NULL DEFAULT 'default';
ALTER TABLE threads ADD COLUMN branch TEXT;
ALTER TABLE threads ADD COLUMN worktree_path TEXT;
ALTER TABLE threads ADD COLUMN project_path TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN discussion_id TEXT;
ALTER TABLE threads ADD COLUMN parent_thread_id TEXT REFERENCES threads(id) ON DELETE SET NULL;
ALTER TABLE threads ADD COLUMN pending_fork_session_ref TEXT;
ALTER TABLE threads ADD COLUMN forked_from_thread_id TEXT REFERENCES threads(id) ON DELETE SET NULL;
```

`interaction_mode`: `"default"` | `"plan"` | `"design"` | `"discussion"`

**Project vs Workspace distinction:**
- `project_path`: the git repo root / project directory. Immutable per thread.
- `workspace_path` (existing): where the provider operates. In local mode this
  equals `project_path`. In worktree mode this is the worktree directory.
- `worktree_path`: if the thread uses a worktree, this is the worktree path
  (may differ from `workspace_path` when worktree has subdirs).
- `branch`: the git branch this thread operates on.

Update `store.Thread` struct to include all new fields:
```go
type Thread struct {
    ID              string `json:"id"`
    Title           string `json:"title"`
    Provider        string `json:"provider"`
    Model           string `json:"model"`
    SessionRef      string `json:"sessionRef"`
    PendingForkRef  string `json:"pendingForkRef,omitempty"`
    ProjectPath     string `json:"projectPath"`
    WorkspacePath   string `json:"workspacePath"`
    WorktreePath    string `json:"worktreePath,omitempty"`
    Branch          string `json:"branch,omitempty"`
    InteractionMode string `json:"interactionMode"`
    DiscussionID    string `json:"discussionId,omitempty"`
    ParentThreadID  string `json:"parentThreadId,omitempty"`
    ForkedFromThreadID string `json:"forkedFromThreadId,omitempty"`
    CreatedAt       int64  `json:"createdAt"`
    UpdatedAt       int64  `json:"updatedAt"`
    Archived        bool   `json:"archived"`
}
```

Update ALL thread CRUD operations (CreateThread, GetThread, ListThreads, UpdateThread)
to read/write the new columns.

---

## 3. New Go Types

### 3.1 Provider Types Additions (`provider/types.go`)

```go
// Add to existing EventKind constants
const (
    EventToolProgress    EventKind = "tool_progress"
    EventCompactBoundary EventKind = "compact_boundary"
    EventRateLimits      EventKind = "rate_limits"
    EventModelRerouted   EventKind = "model_rerouted"
    EventThreadRenamed   EventKind = "thread_renamed"
    EventProposedPlan    EventKind = "proposed_plan"
    // NOTE: Codex reasoning deltas reuse EventThinking (existing).
    // No separate EventReasoning — both providers produce ItemThinking items.
)

type ContextWindow struct {
    UsedTokens      int     `json:"usedTokens"`
    MaxTokens       int     `json:"maxTokens,omitempty"`
    UsedPercentage  float64 `json:"usedPercentage,omitempty"`
    TotalProcessed  int     `json:"totalProcessed,omitempty"`
}

type RateLimitEntry struct {
    LimitID     string  `json:"limitId"`
    LimitName   string  `json:"limitName"`
    UsedPercent float64 `json:"usedPercent"`
    WindowMins  int     `json:"windowMins"`
    ResetsAt    int64   `json:"resetsAt"`
}

type RateLimitsSnapshot struct {
    Provider  string           `json:"provider"`
    Limits    []RateLimitEntry `json:"limits"`
    UpdatedAt int64            `json:"updatedAt"`
}

// --- Structured approval types (NEW, supplements existing ApprovalRequest) ---

type UserInputQuestionOption struct {
    Label       string `json:"label"`
    Description string `json:"description"`
}

type UserInputQuestion struct {
    ID          string                    `json:"id"`
    Header      string                    `json:"header"`
    Question    string                    `json:"question"`
    Options     []UserInputQuestionOption  `json:"options,omitempty"`
    MultiSelect bool                      `json:"multiSelect,omitempty"`
}

type PermissionProfile struct {
    Network    *NetworkPermissions    `json:"network,omitempty"`
    FileSystem *FileSystemPermissions `json:"fileSystem,omitempty"`
}

type NetworkPermissions struct {
    Enabled *bool `json:"enabled,omitempty"`
}

type FileSystemPermissions struct {
    Read  []string `json:"read,omitempty"`
    Write []string `json:"write,omitempty"`
}

// ADD these fields to the existing ApprovalRequest struct (keep ALL existing fields):
//   Kind        string              `json:"kind,omitempty"`        // "command"|"file-read"|"file-change"|"user-input"|"permission"
//   Questions   []UserInputQuestion `json:"questions,omitempty"`   // populated for user-input kind
//   Permissions *PermissionProfile  `json:"permissions,omitempty"` // populated for permission kind
//
// The existing fields (RequestID, ThreadID, TurnID, ToolName, Description, Input, Title) remain unchanged.

// ADD these fields to the existing ApprovalResponse struct (keep ALL existing fields):
//   Answers     map[string]string  `json:"answers,omitempty"`     // for user-input responses
//   Permissions *PermissionProfile `json:"permissions,omitempty"` // for granted permissions
//   Scope       string             `json:"scope,omitempty"`       // "turn"|"session" for permissions
//
// The existing fields (RequestID, Decision) remain unchanged.

// NEW Wails binding for rich approval responses (replaces string-only signature):
// func (a *App) RespondToApproval(threadID string, response ApprovalResponse) error
// This replaces the old (threadID, requestID, decision string) signature.
// The ApprovalResponse struct is JSON-serializable and Wails can bind it.
```

### 3.2 Cost Types (`provider/cost.go`)

```go
type ModelPricing struct {
    InputPerMToken  float64
    OutputPerMToken float64
    CacheReadPerMToken float64
}

// Known pricing table (updated periodically)
var KnownPricing = map[string]ModelPricing{...}

func CalculateCost(model string, usage TokenUsage) float64
```

Reference: `~/repos/llmkit/cost.go` for pricing table structure.

### 3.3 Model Registry (`provider/models.go`)

```go
type ModelInfo struct {
    Slug         string   `json:"slug"`
    Name         string   `json:"name"`
    Provider     string   `json:"provider"`
    Capabilities []string `json:"capabilities,omitempty"` // "thinking", "fast_mode"
}

var ClaudeModels = []ModelInfo{
    {Slug: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6", Provider: "claude"},
    {Slug: "claude-opus-4-6", Name: "Claude Opus 4.6", Provider: "claude"},
    {Slug: "claude-haiku-4-5", Name: "Claude Haiku 4.5", Provider: "claude"},
}

var CodexModels = []ModelInfo{
    {Slug: "gpt-5.4", Name: "GPT-5.4", Provider: "codex"},
    {Slug: "gpt-5.4-mini", Name: "GPT-5.4 Mini", Provider: "codex"},
    {Slug: "o3", Name: "o3", Provider: "codex"},
    {Slug: "o4-mini", Name: "o4-mini", Provider: "codex"},
}

func ModelsForProvider(provider string) []ModelInfo
```

### 3.4 Provider Detection (`provider/detect.go`)

```go
type ProviderStatus struct {
    Provider    string `json:"provider"`
    Installed   bool   `json:"installed"`
    Version     string `json:"version,omitempty"`
    BinaryPath  string `json:"binaryPath"`
    Status      string `json:"status"`    // "ready"|"not_found"|"error"
    Message     string `json:"message,omitempty"`
}

func DetectProvider(provider string, binaryPath string) ProviderStatus
func DetectClaudeVersion(binaryPath string) (string, error)
func DetectCodexVersion(binaryPath string) (string, error)
```

### 3.5 Settings Types (`settings/settings.go`)

```go
type Settings struct {
    Theme              string `json:"theme"`              // "system"|"light"|"dark"
    TimestampFormat    string `json:"timestampFormat"`    // "locale"|"12-hour"|"24-hour"
    DefaultProvider    string `json:"defaultProvider"`
    DefaultModelClaude string `json:"defaultModelClaude"`
    DefaultModelCodex  string `json:"defaultModelCodex"`
    RecentWorkspaces   []string `json:"recentWorkspaces"`
    DiffWordWrap       bool   `json:"diffWordWrap"`
    StreamingEnabled   bool   `json:"streamingEnabled"`
    ConfirmArchive     bool   `json:"confirmArchive"`
    ConfirmDelete      bool   `json:"confirmDelete"`
    ClaudeBinaryPath   string `json:"claudeBinaryPath"`
    CodexBinaryPath    string `json:"codexBinaryPath"`
    ClaudeEnabled      bool   `json:"claudeEnabled"`
    CodexEnabled       bool   `json:"codexEnabled"`
}

var DefaultSettings = Settings{
    Theme:              "system",
    TimestampFormat:    "locale",
    DefaultProvider:    "claude",
    DefaultModelClaude: "claude-sonnet-4-6",
    DefaultModelCodex:  "gpt-5.4",
    DiffWordWrap:       false,
    StreamingEnabled:   true,
    ConfirmArchive:     true,
    ConfirmDelete:      true,
    ClaudeBinaryPath:   "claude",
    CodexBinaryPath:    "codex",
    ClaudeEnabled:      true,
    CodexEnabled:       true,
}

func (s *SettingsService) Get() Settings
func (s *SettingsService) Update(patch map[string]any) (Settings, error)
func (s *SettingsService) AddRecentWorkspace(path string)
```

### 3.6 Discussion Types (`discussion/`)

```go
type DiscussionParticipant struct {
    Role        string `json:"role"`
    Description string `json:"description"`
    System      string `json:"system"`
    Provider    string `json:"provider,omitempty"`
    Model       string `json:"model,omitempty"`
}

type DiscussionSettings struct {
    MaxTurns int `json:"maxTurns"`
}

type DiscussionDefinition struct {
    ID           string                  `json:"id"`
    Name         string                  `json:"name"`
    Description  string                  `json:"description"`
    Scope        string                  `json:"scope"`    // "global"|"project"
    ProjectID    string                  `json:"projectId,omitempty"`
    Participants []DiscussionParticipant  `json:"participants"`
    Settings     DiscussionSettings       `json:"settings"`
    CreatedAt    int64                   `json:"createdAt"`
    UpdatedAt    int64                   `json:"updatedAt"`
}

type Channel struct {
    ID        string `json:"id"`
    ThreadID  string `json:"threadId"`
    Type      string `json:"type"`
    Status    string `json:"status"` // "open"|"concluded"|"closed"
    CreatedAt int64  `json:"createdAt"`
    UpdatedAt int64  `json:"updatedAt"`
}

type ChannelMessage struct {
    ID        string `json:"id"`
    ChannelID string `json:"channelId"`
    Sequence  int    `json:"sequence"`
    FromType  string `json:"fromType"` // "human"|"agent"
    FromID    string `json:"fromId"`
    FromRole  string `json:"fromRole,omitempty"`
    Content   string `json:"content"`
    CreatedAt int64  `json:"createdAt"`
}

type DeliberationState struct {
    ChannelID           string            `json:"channelId"`
    CurrentSpeaker      string            `json:"currentSpeaker"` // thread ID
    TurnCount           int               `json:"turnCount"`
    MaxTurns            int               `json:"maxTurns"`
    ConclusionProposals map[string]string `json:"conclusionProposals"` // threadID → summary
    Concluded           bool              `json:"concluded"`
}
```

### 3.7 Design Types (`design/`)

```go
type DesignArtifact struct {
    ID          string `json:"id"`
    ThreadID    string `json:"threadId"`
    Title       string `json:"title"`
    Description string `json:"description"`
    Kind        string `json:"kind"` // "render"|"option"
    HTMLPath    string `json:"htmlPath"`
    CreatedAt   int64  `json:"createdAt"`
}

type DesignOption struct {
    ID          string `json:"id"`
    Title       string `json:"title"`
    Description string `json:"description"`
    ArtifactID  string `json:"artifactId"`
}

type DesignOptionsRequest struct {
    RequestID string         `json:"requestId"`
    ThreadID  string         `json:"threadId"`
    Prompt    string         `json:"prompt"`
    Options   []DesignOption `json:"options"`
}
```

### 3.8 Git Types (`git/`)

```go
type GitStatus struct {
    IsRepo              bool   `json:"isRepo"`
    Branch              string `json:"branch"`
    IsDefaultBranch     bool   `json:"isDefaultBranch"`
    HasChanges          bool   `json:"hasChanges"`
    Insertions          int    `json:"insertions"`
    Deletions           int    `json:"deletions"`
    FileCount           int    `json:"fileCount"`
    HasUpstream         bool   `json:"hasUpstream"`
    AheadCount          int    `json:"aheadCount"`
    BehindCount         int    `json:"behindCount"`
    HasOriginRemote     bool   `json:"hasOriginRemote"`
    OpenPRURL           string `json:"openPrUrl,omitempty"`
    OpenPRNumber        int    `json:"openPrNumber,omitempty"`
}

type GitBranch struct {
    Name         string `json:"name"`
    IsRemote     bool   `json:"isRemote"`
    IsCurrent    bool   `json:"isCurrent"`
    IsDefault    bool   `json:"isDefault"`
    WorktreePath string `json:"worktreePath,omitempty"`
}

type GitActionResult struct {
    Action  string `json:"action"`
    Branch  string `json:"branch,omitempty"`
    Commit  string `json:"commitSha,omitempty"`
    PRURL   string `json:"prUrl,omitempty"`
    Message string `json:"message,omitempty"`
    Error   string `json:"error,omitempty"`
}

type GitActionProgress struct {
    Phase   string `json:"phase"`   // "branch"|"commit"|"push"|"pr"
    Message string `json:"message"`
}

func (g *GitCore) Status(cwd string) (GitStatus, error)
func (g *GitCore) WorkingTreeDiff(cwd string) (string, error)
func (g *GitCore) ListBranches(cwd string) ([]GitBranch, error)
func (g *GitCore) Commit(cwd, subject, body string) (string, error) // returns commitSha
func (g *GitCore) Push(cwd string) error
func (g *GitCore) Pull(cwd string) error
func (g *GitCore) Checkout(cwd, branch string) error
func (g *GitCore) CreateBranch(cwd, name string) error
```

Reference: `~/repos/forge/apps/server/src/git/Layers/GitCore.ts` for full behavior.

---

## 4. New Frontend Types

### 4.1 Settings (`types/settings.ts`)

```typescript
export interface Settings {
    theme: 'system' | 'light' | 'dark';
    timestampFormat: 'locale' | '12-hour' | '24-hour';
    defaultProvider: 'claude' | 'codex';
    defaultModelClaude: string;
    defaultModelCodex: string;
    recentWorkspaces: string[];
    diffWordWrap: boolean;
    streamingEnabled: boolean;
    confirmArchive: boolean;
    confirmDelete: boolean;
    claudeBinaryPath: string;
    codexBinaryPath: string;
    claudeEnabled: boolean;
    codexEnabled: boolean;
}

export interface ProviderStatus {
    provider: string;
    installed: boolean;
    version: string;
    binaryPath: string;
    status: 'ready' | 'not_found' | 'error';
    message: string;
}

export interface ModelInfo {
    slug: string;
    name: string;
    provider: string;
    capabilities: string[];
}
```

### 4.2 Discussion (`types/discussion.ts`)

```typescript
export interface DiscussionParticipant {
    role: string;
    description: string;
    system: string;
    provider?: string;
    model?: string;
}

export interface DiscussionDefinition {
    id: string;
    name: string;
    description: string;
    scope: 'global' | 'project';
    projectId?: string;
    participants: DiscussionParticipant[];
    settings: { maxTurns: number };
    createdAt: number;
    updatedAt: number;
}

export interface Channel {
    id: string;
    threadId: string;
    type: string;
    status: 'open' | 'concluded' | 'closed';
    createdAt: number;
    updatedAt: number;
}

export interface ChannelMessage {
    id: string;
    channelId: string;
    sequence: number;
    fromType: 'human' | 'agent';
    fromId: string;
    fromRole?: string;
    content: string;
    createdAt: number;
}
```

### 4.3 Design (`types/design.ts`)

```typescript
export interface DesignArtifact {
    id: string;
    threadId: string;
    title: string;
    description: string;
    kind: 'render' | 'option';
    htmlPath: string;
    createdAt: number;
}

export interface DesignOption {
    id: string;
    title: string;
    description: string;
    artifactId: string;
}

export interface DesignOptionsRequest {
    requestId: string;
    threadId: string;
    prompt: string;
    options: DesignOption[];
}
```

### 4.4 Git (`types/git.ts`)

```typescript
export interface GitStatus {
    isRepo: boolean;
    branch: string;
    isDefaultBranch: boolean;
    hasChanges: boolean;
    insertions: number;
    deletions: number;
    fileCount: number;
    hasUpstream: boolean;
    aheadCount: number;
    behindCount: number;
    hasOriginRemote: boolean;
    openPrUrl?: string;
    openPrNumber?: number;
}

export interface GitBranch {
    name: string;
    isRemote: boolean;
    isCurrent: boolean;
    isDefault: boolean;
    worktreePath?: string;
}

export interface GitActionResult {
    action: string;
    branch?: string;
    commitSha?: string;
    prUrl?: string;
    message?: string;
    error?: string;
}
```

### 4.5 Events Additions (`types/events.ts`)

Add to the `EventKind` union:
```typescript
export type EventKind =
    // existing 15 inline + 3 heavy (18 total, including 'thinking') ...
    | 'tool_progress'
    | 'compact_boundary'
    | 'rate_limits'
    | 'model_rerouted'
    | 'thread_renamed';
    // Codex reasoning deltas arrive as 'thinking' (same as Claude thinking blocks)
```

The `ProviderEvent` interface stays lean. Event-specific data is carried in the
existing `meta` field (JSON) and parsed on the frontend based on `kind`:

```typescript
// Frontend parses meta based on event kind:
// kind === 'tool_progress'    → meta as { current: number; total: number; message: string }
// kind === 'compact_boundary' → meta as ContextWindow
// kind === 'rate_limits'      → meta as { limits: RateLimitEntry[] }
// kind === 'model_rerouted'   → meta as { newModel: string }
// kind === 'thread_renamed'   → meta as { newTitle: string }
// kind === 'thinking'          → for Codex reasoning deltas, content field carries
//                                 delta text (accumulated server-side like text)
// kind === 'approval_request' → meta as ApprovalRequest (already works this way)
```

No new fields on `ProviderEvent`. The Go side populates `Meta json.RawMessage`
with the appropriate JSON for each event kind.

---

## 5. Protocol Completeness

### 5.1 Claude Additions

**File**: `internal/provider/claude/protocol.go`

Currently skipped system subtypes to NOW HANDLE:

| Subtype | Behavior |
|---------|----------|
| `tool_progress` | Emit `EventToolProgress` with progress data from the message. Include `itemId` if present. |
| `compact_boundary` | Emit `EventCompactBoundary`. Extract context window data if present in the message body. |
| `api_retry` | Emit `EventSessionStatus` with status `"retrying"` and retry metadata. |
| `assistant` `tool_use` named `ExitPlanMode` | Emit `EventProposedPlan` with the captured markdown and do not render it as a normal tool call. |

Reference: Read `claude --output-format stream-json --verbose` output samples. The `system` message with subtype `tool_progress` carries `{ content: { progress: {current, total, message} } }`.

**Session-scoped approval** (`claude/session.go`):
The wire protocol for `allow_session` sends the same `behavior: "allow"` as regular
`allow` — this is correct and already works. The distinction is frontend-only: when
the user clicks "Allow for Session", the frontend should track which tool names have
been session-approved and auto-resolve future approval requests for the same tool
without prompting the user.

Implementation: Add a `sessionApprovedTools map[string]bool` to the frontend pane
state. On `allow_session` response, add the tool name. On new approval requests,
check if the tool is already session-approved and auto-resolve.

### 5.2 Codex Additions

**File**: `internal/provider/codex/protocol.go`

Currently skipped notifications to NOW HANDLE:

| Method | Behavior |
|--------|----------|
| `item/reasoning/textDelta` | Emit `EventThinking` with delta text in `Content`. These are Codex's equivalent of Claude's thinking blocks. Accumulate in a per-thread reasoning buffer (like text accumulation). On turn complete, persist as an `ItemThinking` item. |
| `item/reasoning/summaryTextDelta` | Same as `textDelta` — accumulate into the same reasoning buffer. |
| `item/completed` where `item.type == "plan"` | Emit `EventProposedPlan` carrying the final markdown and persist it as a dedicated `proposed_plan` payload/item so the timeline renders a plan card instead of a plain assistant message. |
| `thread/name/updated` | Emit `EventThreadRenamed` with new title extracted from params. Call back to update thread title in store. |
| `account/rateLimits/updated` | Emit `EventRateLimits` with parsed rate limit entries. |
| `model/rerouted` | Emit `EventModelRerouted` with new model name. |
| `thread/compacted` | Emit `EventCompactBoundary`. |

**Structured user input** (`codex/session.go`):
`item/tool/requestUserInput` currently treated as binary approve/deny.
Fix: Parse the `params` to extract questions array. Build `ApprovalRequest` with `Kind: "user-input"` and `Questions` populated. Frontend renders a multi-question form.

**Structured permission requests** (`codex/session.go`):
`item/permissions/requestApproval` currently treated as binary.
Fix: Parse `params` to extract `reason` and `permissions` profile (network + filesystem). Build `ApprovalRequest` with `Kind: "permission"` and `Permissions` populated. Frontend renders scope selection (turn/session) + permission details.

**Diff accumulation fix** (`codex/protocol.go` + `triage/router.go`):
`turn/diff/updated` currently emits a new `EventDiff` each time. But the notification
carries the FULL cumulative diff, so each emission creates a duplicate payload.

Fix: Use an UPSERT pattern in the triage layer. Add an `EventDiffReplace` kind (or
a `Replace bool` field on `ProviderEvent`) that tells `persistHeavy` to replace the
existing payload for this turn's diff rather than creating a new one.

Implementation:
1. In `codex/protocol.go`: set a `Replace: true` flag on the `ProviderEvent` for
   `turn/diff/updated` notifications.
2. In `triage/router.go`: when `Replace` is true, use `store.UpsertPayload` (new
   method) that does `INSERT OR REPLACE` keyed on `(thread_id, turn_index, kind='diff')`.
3. This preserves "persist immediately" (no crash data loss) while avoiding duplication.

Add to store:
```go
func (s *Store) UpsertTurnPayload(threadID string, turnIndex int, kind string, payload Payload) error
```

### 5.3 Triage Additions

**File**: `internal/triage/router.go`

Add routing for new event kinds:
- `EventToolProgress` → emit inline (frontend shows progress on active tool call)
- `EventCompactBoundary` → emit inline (frontend updates context window state)
- `EventThinking` (from Codex reasoning deltas) → accumulate in a per-thread
  reasoning buffer (new field on Router, like `textAccumulators`). On turn complete,
  persist accumulated reasoning as an `ItemThinking` payload. This unifies Codex
  reasoning deltas with Claude's complete thinking blocks — both produce the same
  item kind. Claude thinking still goes through `persistHeavy` (complete blocks).
- `EventProposedPlan` → persist as a heavy `proposed_plan` payload with preview
  metadata so the frontend can render a lightweight expandable plan card with
  copy/download/save actions.
- `EventRateLimits` → emit inline (frontend updates rate limits display)
- `EventModelRerouted` → emit inline + update thread model in store
- `EventThreadRenamed` → emit inline + update thread title in store

---

## 6. Session Lifecycle

### 6.1 Auto-Resume on Thread Switch

**File**: `app.go`

When `switchThread` is called from the frontend (via a new binding `SwitchThread`), if the thread has a `session_ref` or a `pending_fork_session_ref` but no active session in the `sessions` map, automatically call `StartSession`. The session will use the stored `session_ref` for resume, or perform a Claude `--fork-session` resume when `pending_fork_session_ref` is populated.

```go
func (a *App) SwitchThread(threadID string) (store.Thread, error) {
    t, err := a.store.GetThread(threadID)
    if err != nil { return t, err }

    a.mu.Lock()
    _, hasSession := a.sessions[threadID]
    a.mu.Unlock()

    if !hasSession && t.SessionRef != "" {
        // Auto-resume in background — don't block the switch
        go func() {
            if err := a.StartSession(threadID); err != nil {
                log.Printf("auto-resume failed for %s: %v", threadID, err)
                // Emit error event to frontend
            }
        }()
    }
    return t, nil
}
```

### 6.2 Session Health Monitoring

In `readLoop` for both providers: when the subprocess exits unexpectedly (non-zero exit, signal), emit a `session_status` event with status `"error"` and the exit reason. The frontend shows a "Session disconnected" banner with a "Reconnect" button.

Add `Reconnect` binding:
```go
func (a *App) ReconnectSession(threadID string) error {
    a.StopSession(threadID) // clean up old session
    return a.StartSession(threadID) // start fresh (will resume via session_ref)
}
```

Add `UpdateThreadModel` binding:
```go
func (a *App) UpdateThreadModel(threadID string, model string) (store.Thread, error)
```

Behavior:
- Trim and validate the requested model slug.
- If the thread has no active session, update the stored thread model only.
- If the thread has an active session, restart it with the stored resume reference so the conversation continues on the new model.
- If the restart fails, roll the stored model back to the previous value and return the restart error.

### 6.3 Provider Binary Detection

**File**: `provider/detect.go`

On app startup (`app.startup`), detect both provider binaries:
```go
func DetectProvider(name string, binaryPath string) ProviderStatus {
    path, err := exec.LookPath(binaryPath)
    if err != nil {
        return ProviderStatus{Provider: name, Status: "not_found", Message: "..."}
    }
    version, err := getVersion(path) // run `claude --version` or `codex --version`
    return ProviderStatus{Provider: name, Installed: true, Version: version, BinaryPath: path, Status: "ready"}
}
```

Expose via binding: `GetProviderStatuses() []ProviderStatus`

### 6.4 Thread Title Auto-Generation

Two approaches depending on provider:

**Codex**: Handle `thread/name/updated` notification → update thread title in store + emit event.

**Claude**: No auto-title event. Generate a title on the first user turn with the Claude CLI:
- When the first user message is sent and the thread title is still the default `"New Thread"`, run a one-shot Claude structured-output prompt to produce a concise title.
- Sanitize the returned title to a single-line sidebar-safe label (trim quotes/whitespace, collapse spaces, cap at 50 characters).
- Apply the rename only if the title is still the default when the result comes back, so a user rename is never overwritten.
- Emit the same `thread_renamed` provider event after the store update so the active pane and thread list refresh immediately.

### 6.5 Codex Process Model — Per-Thread (Matching Forge)

Forge spawns one `codex app-server` process per active thread. The Codex app-server
protocol does NOT reliably support multiplexing multiple threads on a single process
(turn interleaving, error isolation, and cleanup are problematic).

Keep the current per-thread spawn model. No `pool.go` needed. Remove the `pool.go`
entry from the directory structure in Section 1.2.

The ARCHITECTURE.md has been updated to reflect this (per-thread, not singleton).

---

## 7. Settings System

### 7.1 File-Based Settings (`settings/settings.go`)

Settings are a JSON file, NOT SQLite. Path: `<UserConfigDir>/agent-overflow/settings.json`.

```go
type Service struct {
    path     string    // settings.json path
    mu       sync.RWMutex
    cached   *Settings // in-memory cache, invalidated on file change
}

func NewService(configDir string) (*Service, error)
func (s *Service) Get() Settings              // read from cache (reload from file if stale)
func (s *Service) Update(patch map[string]any) (Settings, error) // merge, validate, write
func (s *Service) AddRecentWorkspace(path string)  // push to front, cap at 10, dedup
func (s *Service) Path() string               // returns settings file path (for "open in editor")
```

**Read behavior:**
1. Try to read and parse `settings.json`
2. If file doesn't exist or is empty → return `DefaultSettings`
3. If JSON is malformed → log warning, return `DefaultSettings`
4. Merge parsed values over `DefaultSettings` (sparse file only has overrides)

**Write behavior:**
1. Load current settings
2. Apply patch (validate each field type)
3. Strip values matching `DefaultSettings` (sparse serialization)
4. Write atomically: temp file → rename over settings.json
5. Update in-memory cache

Reference: `~/repos/forge/apps/server/src/serverSettings.ts` for atomic write
pattern and sparse serialization.

### 7.3 Wails Bindings (`app.go`)

Replace the stub:
```go
func (a *App) GetSettings() (Settings, error)
func (a *App) UpdateSettings(patch map[string]any) (Settings, error)
func (a *App) GetProviderStatuses() ([]ProviderStatus, error)
func (a *App) GetModelsForProvider(provider string) ([]ModelInfo, error)
```

---

## 8. Cost Calculation

**File**: `provider/cost.go`

Maintain a pricing table for known models. Calculate cost from token usage:

```go
func CalculateCost(model string, usage TokenUsage) float64 {
    pricing, ok := KnownPricing[normalizeModel(model)]
    if !ok { return 0 }
    input := float64(usage.InputTokens) / 1_000_000 * pricing.InputPerMToken
    output := float64(usage.OutputTokens) / 1_000_000 * pricing.OutputPerMToken
    cache := float64(usage.CacheReadInputTokens) / 1_000_000 * pricing.CacheReadPerMToken
    return input + output + cache
}
```

Reference `~/repos/llmkit/cost.go` for the exact pricing values.

Integrate: after `EventTokenUsage` is received in triage, calculate cost and attach to the event before emitting.

---

## 9. Git Operations

### 9.1 Git Core (`git/core.go`)

Wraps `exec.Command("git", ...)` with:
- Timeout (30s default)
- Output size limits (1MB default)
- Non-zero exit handling

```go
type Core struct {
    timeout time.Duration
}

func NewCore() *Core
func (c *Core) Execute(cwd string, args ...string) (stdout, stderr string, err error)
```

### 9.2 Status and Diff (`git/status.go`)

```go
func (c *Core) Status(cwd string) (GitStatus, error)
// Runs: git status --porcelain=v2 --branch
// Parses: branch name, ahead/behind counts, changed file count
// Also checks: git remote get-url origin (for hasOriginRemote)
// Also checks: gh pr list --head <branch> --json url,number (for open PR)

func (c *Core) WorkingTreeDiff(cwd string) (string, error)
// Runs: git diff HEAD + git diff --cached

func (c *Core) ListBranches(cwd string) ([]GitBranch, error)
// Runs: git branch -a --format='%(refname:short)|%(HEAD)|%(worktreepath)'
```

### 9.3 Actions (`git/actions.go`)

```go
func (c *Core) Commit(cwd, subject, body string) (commitSha string, err error)
// Runs: git add -A && git commit -m "subject\n\nbody"

func (c *Core) Push(cwd string) error
// Runs: git push (with --set-upstream origin <branch> if no upstream)

func (c *Core) Pull(cwd string) error
// Runs: git pull --ff-only

func (c *Core) Checkout(cwd, branch string) error
func (c *Core) CreateBranch(cwd, name string) error

// Worktree operations
func (c *Core) CreateWorktree(cwd, path, branch string) error
// Runs: git worktree add <path> -b <branch>

func (c *Core) RemoveWorktree(cwd, path string) error
// Runs: git worktree remove <path>

func (c *Core) ListWorktrees(cwd string) ([]Worktree, error)
// Runs: git worktree list --porcelain

type Worktree struct {
    Path   string `json:"path"`
    Branch string `json:"branch"`
    HEAD   string `json:"head"`
}
```

Reference: `~/repos/forge/apps/server/src/git/Layers/GitCore.ts` for full behavior
including createWorktree, removeWorktree implementations.

### 9.4 GitHub CLI (`git/github.go`)

```go
func (c *Core) CreatePR(cwd, title, body string) (url string, err error)
// Runs: gh pr create --title ... --body ...
// Returns PR URL

func (c *Core) ListOpenPRs(cwd, head string) ([]GitPR, error)
// Runs: gh pr list --head <head> --json url,number,title,state
```

### 9.5 Wails Bindings

```go
func (a *App) GetGitStatus(threadID string) (GitStatus, error)
func (a *App) GetWorkingTreeDiff(threadID string) (string, error)
func (a *App) GitListBranches(threadID string) ([]GitBranch, error)
func (a *App) GitCommit(threadID, subject, body string) (GitActionResult, error)
func (a *App) GitPush(threadID string) (GitActionResult, error)
func (a *App) GitPull(threadID string) (GitActionResult, error)
func (a *App) GitCheckout(threadID, branch string) error
func (a *App) GitCreateBranch(threadID, name string) error
func (a *App) GitCreatePR(threadID, title, body string) (GitActionResult, error)
func (a *App) GitCreateWorktree(threadID, branch string) (string, error) // returns worktree path; empty branch creates a temporary forge/<8hex> branch
func (a *App) GitRemoveWorktree(threadID string) error
func (a *App) GitListWorktrees(threadID string) ([]Worktree, error)
```

Git operations resolve `cwd` from the thread's `ProjectPath` (for repo-level ops
like branch listing, worktree creation) or `WorkspacePath` (for working-dir ops
like status, diff, commit). The distinction matters for worktree threads where
the workspace is NOT the project root.

Worktree branch naming matches forge's visible lifecycle:
- If the user leaves the worktree branch blank, create the worktree on a temporary `forge/<8-hex>` branch.
- On the first user turn, if the worktree branch is still temporary, derive a descriptive branch name from the message, normalize it to `forge/<fragment>`, rename the branch, and persist the updated thread metadata before sending the turn to the provider.

---

## 10. Discussion System

### 10.1 Discussion Registry (`discussion/registry.go`)

```go
type Registry struct {
    store *store.Store
}

func NewRegistry(st *store.Store) *Registry
func (r *Registry) List(scope string) ([]DiscussionDefinition, error)
func (r *Registry) Get(name, scope string) (DiscussionDefinition, error)
func (r *Registry) Create(def DiscussionDefinition) error
func (r *Registry) Update(previousName, previousScope string, def DiscussionDefinition) error
func (r *Registry) Delete(name, scope string) error
```

Validation: name non-empty, at least 2 participants, each participant has role + system prompt.

### 10.2 Channel Service (`discussion/channel.go`)

```go
type ChannelService struct {
    store *store.Store
}

func NewChannelService(st *store.Store) *ChannelService
func (cs *ChannelService) Create(threadID, channelType string) (Channel, error)
func (cs *ChannelService) PostMessage(input PostMessageInput) (ChannelMessage, error)
func (cs *ChannelService) GetMessages(channelID string, afterSeq int, limit int) ([]ChannelMessage, error)
func (cs *ChannelService) Close(channelID string) error
```

### 10.3 Deliberation Engine (`discussion/deliberation.go`)

Simplified ping-pong deliberation:

```go
type Deliberation struct {
    state DeliberationState
    mu    sync.Mutex
}

func NewDeliberation(channelID string, maxTurns int) *Deliberation
func (d *Deliberation) RecordPost(participantThreadID string) (nextSpeaker string, shouldConclude bool)
func (d *Deliberation) ProposeConclusionFrom(threadID, summary string) (allAgreed bool)
func (d *Deliberation) State() DeliberationState
```

Turn logic:
1. Track `currentSpeaker` — alternates between participants in order
2. Increment `turnCount` on each post
3. When `turnCount >= maxTurns`, set `shouldConclude = true`
4. Conclusion requires all participants to have proposed (unanimous)

### 10.4 Wails Bindings

```go
func (a *App) ListDiscussions(scope string) ([]DiscussionDefinition, error)
func (a *App) GetDiscussion(name, scope string) (DiscussionDefinition, error)
func (a *App) CreateDiscussion(def DiscussionDefinition) error
func (a *App) UpdateDiscussion(prevName, prevScope string, def DiscussionDefinition) error
func (a *App) DeleteDiscussion(name, scope string) error
func (a *App) StartDiscussion(threadID, discussionName string) error
func (a *App) GetChannelMessages(channelID string, afterSeq, limit int) ([]ChannelMessage, error)
func (a *App) PostChannelMessage(channelID, content string) error
```

### 10.5 Frontend

**DiscussionEditor.svelte**: Form for creating/editing discussion definitions. Fields: name, description, scope, participants list (add/remove), per-participant: role, description, system prompt, provider/model picker. Settings: maxTurns. Validate before save.

**ChannelView.svelte**: Displays channel messages in a timeline. Each message shows role badge + content. Human can post messages via an input at the bottom. Shows deliberation state (turn count, conclusion proposals).

**ParticipantCard.svelte**: Reusable card for a discussion participant. Shows role, description, model selection.

---

## 11. Design Mode

### 11.1 Artifact Storage (`design/artifacts.go`)

```go
type ArtifactStore struct {
    baseDir string // e.g., <appDataDir>/design-artifacts/
}

func NewArtifactStore(baseDir string) *ArtifactStore
func (as *ArtifactStore) Store(threadID string, html, title, description string, kind string) (DesignArtifact, error)
func (as *ArtifactStore) Get(threadID, artifactID string) (string, error) // returns HTML content
func (as *ArtifactStore) List(threadID string, kind string) ([]DesignArtifact, error)
```

Storage layout:
```
<baseDir>/<threadID>/<artifactID>.html
```

Metadata stored in SQLite `design_artifacts` table.

### 11.2 Design System Prompt (`design/prompts.go`)

Bundled default system prompt instructing the agent to use `render_design` and `present_options` tools. Can be overridden via a user config file.

```go
func LoadDesignSystemPrompt(configDir string) string
```

Reference: `~/repos/forge/apps/server/src/design/designSystemPrompt.ts`

### 11.3 Design Reactor (`design/reactor.go`)

When a thread's `interaction_mode` is `"design"`:
1. Inject design system prompt before the first turn
2. When the provider renders HTML (via tool call), store the artifact and emit event
3. When the provider presents options, store each option as an artifact, create an interactive request
4. When the user chooses an option, resolve the request and inform the provider

For Codex: design tools (`render_design`, `present_options`) are registered as MCP
tools via the app-server protocol. The MCP server is a lightweight HTTP endpoint
that the Codex process connects to. Register the MCP server config in `thread/start`
params under `mcpServers`. Design mode must be set at thread creation time (before
`thread/start`) so the MCP tools are available from the first turn.

For Claude: design tools are injected via the system prompt, instructing Claude to
use structured output for `render_design` and `present_options` actions. The Go
layer parses tool_use blocks matching these names and routes them through the
design reactor.

Reference: `~/repos/forge/apps/server/src/design/DesignModeReactor.ts` for the
full lifecycle.

### 11.4 Wails Bindings

```go
func (a *App) ListDesignArtifacts(threadID string) ([]DesignArtifact, error)
func (a *App) GetDesignArtifactHTML(threadID, artifactID string) (string, error)
func (a *App) ChooseDesignOption(threadID, requestID, optionID string) error
```

### 11.5 Frontend

**DesignPanel.svelte**: Side panel showing the latest rendered design artifact. Renders HTML in a sandboxed iframe. Shows artifact history as thumbnails. Auto-opens when first artifact arrives for a design-mode thread.

**DesignOptionPicker.svelte**: When design options are presented, shows cards for each option with title, description, and preview thumbnail. User clicks to choose.

---

## 12. UI Improvements

### 12.1 Shared Components

**CodeBlock.svelte**: Replaces plain `<pre><code>` in Markdown. Uses shiki for syntax highlighting. Language detection from code fence. Copy button in top-right corner. Line numbers optional.

**CopyButton.svelte**: Clipboard copy with success feedback (checkmark for 2s). Props: `text: string`, `label?: string`.

**ConfirmDialog.svelte**: Modal dialog with title, description, confirm/cancel buttons. Destructive variant with red confirm button. Props: `open`, `title`, `description`, `confirmLabel`, `destructive`, `onConfirm`, `onCancel`.

**Toast.svelte** + `toast.svelte.ts` store: Toast notification system. Types: success, error, warning, info. Auto-dismiss after 5s. Stacks from bottom-right.

### 12.2 Chat View Improvements

**StreamingMessage.svelte**: Markdown-aware streaming renderer. Renders completed blocks (paragraphs, code blocks, lists) as full markdown. Renders the in-progress tail as plain text with cursor. This prevents partial markdown from rendering incorrectly mid-stream.

**ThinkingBlock.svelte**: Expandable thinking/reasoning display. Shows preview (first 200 chars) collapsed. Click to expand and load full content from payload via `GetPayloadData`. Animated collapse/expand.

**ChangedFilesTree.svelte**: File tree showing all files changed in a turn. Grouped by directory. Each file shows +/- counts with green/red badges. Click to expand that file's diff preview.

**ContextWindowMeter.svelte**: Circular SVG ring meter. Shows usage percentage in center. Hover popover with detailed token counts.

**RateLimitsMeter.svelte**: Shows rate limit usage. Dual-window display (5h + 7d). Warning color when > 80%.

**ProviderStatusBanner.svelte**: Alert banner shown below header when provider has issues. Shows reconnect button when disconnected.

### 12.3 Composer Improvements

**ModelPicker.svelte**: Dropdown showing models for the current thread provider. Selected model highlighted. Choosing a model updates the active thread immediately and restarts the provider session so the new model takes effect mid-conversation. Custom model slug input at bottom.

**ProviderPicker.svelte**: Toggle or dropdown for switching between Claude and Codex. Shows provider status dot. Disabled providers are grayed out with tooltip.

**WorkspacePicker.svelte**: Uses `Dialogs.OpenDirectory` from `@wailsio/runtime` for native folder picker. Shows recent workspaces as dropdown. Text input for manual path entry.

**ApprovalPrompt.svelte enhancements**:
- Show full tool input (command text, file path, file content preview)
- "Allow for Session" button in addition to Allow/Deny
- For user-input requests: render questions with text inputs
- For permission requests: show permission details + scope selector (turn/session)

### 12.4 Sidebar Improvements

**Thread rename**: Double-click thread title in ThreadRow to rename inline. Or right-click context menu with "Rename" option.

**Thread delete**: Context menu "Delete" option. Respects `confirmDelete` setting.

**Provider status indicators**: Small dots next to provider name showing ready/error/not-found.

### 12.5 Settings View

**SettingsView.svelte**: Full-page view replacing ChatView when settings are open. Navigation: General, Providers, Archived.

**GeneralSettings.svelte**: Theme picker, timestamp format, diff word wrap, streaming toggle, archive/delete confirmation toggles.

**ProviderSettings.svelte**: Per-provider: enable/disable, binary path, status display, model list with known + custom models.

**ArchivedThreads.svelte**: List archived threads with unarchive/delete actions.

### 12.6 Git UI

**BranchToolbar.svelte**: Shows current branch name in chat header. Click to open branch picker dropdown. Shows branch list with search filter. "Create new branch" option. "Checkout PR" option with PR number input.

**GitActionsControl.svelte**: Context-aware action button (Commit / Push / Create PR). Logic from `~/repos/forge/apps/web/src/components/GitActionsControl.logic.ts`.

**CommitDialog.svelte**: Dialog with commit message input (subject + body). Auto-generated subject suggestion from diff summary. Confirm/cancel.

---

## 13. Operations

### 13.1 Structured Logging (`logging/logger.go`)

```go
type Logger struct {
    file     *os.File
    mu       sync.Mutex
    maxBytes int64
}

func NewLogger(path string, maxBytes int64) (*Logger, error)
func (l *Logger) Log(entry LogEntry) error
func (l *Logger) Close() error

type LogEntry struct {
    Timestamp string `json:"ts"`
    Level     string `json:"level"`
    Component string `json:"component"`
    Message   string `json:"msg"`
    Data      any    `json:"data,omitempty"`
}
```

NDJSON format. Rotate when file exceeds `maxBytes` (default 10MB). Keep 3 rotated files.

### 13.2 Provider Event Logging

Log all raw provider stdin/stdout to a separate NDJSON file:
```
<appDataDir>/logs/provider-events-<date>.ndjson
```

Each entry: `{ ts, threadId, direction: "in"|"out", provider, data }`.

Enable via `AGENT_OVERFLOW_DEBUG=provider` env var (follows FORGE_DEBUG pattern).

### 13.3 Provider Version Detection

On startup and when settings change:
```go
func DetectClaudeVersion(binaryPath string) (string, error) {
    // Run: claude --version
    // Parse: extract version string
}

func DetectCodexVersion(binaryPath string) (string, error) {
    // Run: codex --version
    // Parse: extract version string
}
```

Surface version in settings panel. Warn if version is below minimum supported.

---

## 14. Loop Split

### Loop 1 — Backend & Protocol (~18 work items)

All backend work. No frontend changes.

Includes:
- Protocol completeness (all Claude + Codex event additions)
- Session lifecycle (auto-resume, health, reconnect, binary detection)
- Cost calculation
- Settings persistence layer (store + service)
- Database migration system
- Structured logging
- Provider event logging
- Thread title auto-generation
- Git operations (full backend including worktree management)
- Discussion backend (registry, channel, deliberation)
- Design mode backend (artifacts, prompts, reactor)
- Backend code reorganization (renames, new dirs)
- Provider version detection
- Model registry

### Loop 2 — Core UI (~18 work items)

All frontend work for core features.

Includes:
- Frontend code reorganization (move to subdirs)
- Shared components (CodeBlock/shiki, CopyButton, ConfirmDialog, Toast)
- Streaming markdown rendering
- Chat improvements (ThinkingBlock, ChangedFilesTree, ContextWindowMeter, RateLimitsMeter, ProviderStatusBanner)
- Composer improvements (ModelPicker, ProviderPicker, WorkspacePicker, enhanced ApprovalPrompt)
- Sidebar improvements (rename, delete, confirmations, provider status)
- Settings panel (full settings UI)
- Theme system (system/light/dark)
- Git UI (BranchToolbar, GitActionsControl, CommitDialog)
- Worktree UI (local vs worktree mode toggle, worktree creation flow)
- Event router updates for new event kinds
- Bindings expansion

### Loop 3 — Discussions & Design (~12 work items)

Full-stack implementation of advanced interaction modes.

Includes:
- Discussion frontend (editor, channel view, participant cards)
- Discussion integration (start discussion → spawn child threads → channel messaging)
- Design frontend (design panel, option picker)
- Design integration (interaction mode → system prompt injection → artifact rendering → option flow)
- End-to-end testing of both features
- Final polish and integration verification
