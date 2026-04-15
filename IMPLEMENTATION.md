# Implementation Spec

Authority hierarchy: `ARCHITECTURE.md` (behavioral) > `IMPLEMENTATION.md` (implementation) > PROMPT files (work items).

---

## 1. Module/Directory Layout

```
agent-overflow/
├── main.go                              # Wails entry, process options
├── app.go                               # Wails-bound struct, all frontend bindings
├── internal/
│   ├── provider/
│   │   ├── types.go                     # Shared types: ProviderEvent, EventKind, ItemKind, etc.
│   │   ├── process.go                   # Shared subprocess management: spawn, pipes, kill
│   │   ├── claude/
│   │   │   ├── claude.go                # Claude CLI session: spawn, handshake, send, close
│   │   │   ├── protocol.go             # NDJSON parsing, message types, control protocol
│   │   │   └── claude_test.go
│   │   └── codex/
│   │       ├── codex.go                 # Codex app-server session: spawn, handshake, send, close
│   │       ├── protocol.go             # JSON-RPC framing, request correlation, notifications
│   │       └── codex_test.go
│   ├── triage/
│   │   ├── triage.go                    # Event classification, meta extraction, routing
│   │   ├── triage_test.go
│   │   └── meta.go                      # Preview/summary extraction for heavy payloads
│   └── store/
│       ├── store.go                     # SQLite setup, migrations, Store struct
│       ├── threads.go                   # Thread CRUD
│       ├── items.go                     # Item persistence (append-only)
│       ├── payloads.go                  # Payload storage and retrieval
│       └── store_test.go
├── frontend/
│   ├── src/
│   │   ├── main.ts                      # Svelte mount
│   │   ├── app.css                      # Tailwind 4 theme
│   │   ├── App.svelte                   # Root: sidebar + main content
│   │   ├── lib/
│   │   │   ├── stores/
│   │   │   │   ├── thread.svelte.ts     # ThreadPane factory (per-pane state, tiling-ready)
│   │   │   │   ├── panes.svelte.ts      # Active panes registry (v1: single "main" pane)
│   │   │   │   ├── threads.svelte.ts    # Thread list for sidebar
│   │   │   │   ├── events.ts            # Wails Events.On listener, fans out to panes
│   │   │   │   └── bindings.ts          # Typed wrappers around Wails Go bindings
│   │   │   ├── components/
│   │   │   │   ├── Sidebar.svelte
│   │   │   │   ├── ThreadList.svelte
│   │   │   │   ├── ThreadRow.svelte
│   │   │   │   ├── ChatView.svelte          # Main chat container
│   │   │   │   ├── MessageTimeline.svelte   # Append-only item list
│   │   │   │   ├── UserMessage.svelte
│   │   │   │   ├── AssistantMessage.svelte
│   │   │   │   ├── WorkEntry.svelte         # Tool call / file change / command
│   │   │   │   ├── WorkGroup.svelte         # Grouped tool calls
│   │   │   │   ├── DiffPreview.svelte       # Inline diff preview card
│   │   │   │   ├── CommandOutput.svelte     # Expandable command output
│   │   │   │   ├── ApprovalPrompt.svelte    # Permission/approval UI
│   │   │   │   ├── BackgroundTray.svelte    # Background tasks tray
│   │   │   │   ├── Composer.svelte          # Message input
│   │   │   │   ├── ComposerControls.svelte  # Model picker, mode toggles
│   │   │   │   ├── StatusBar.svelte         # Session status, token usage
│   │   │   │   └── Markdown.svelte          # Markdown renderer with code highlight
│   │   │   ├── types/
│   │   │   │   ├── events.ts                # TS types matching Go ProviderEvent
│   │   │   │   └── models.ts                # Thread, Item, Payload types matching Go
│   │   │   └── utils/
│   │   │       ├── format.ts                # Time formatting, number formatting
│   │   │       └── diff.ts                  # Diff parsing utilities
│   │   └── vite-env.d.ts
│   ├── index.html
│   ├── package.json
│   ├── vite.config.ts
│   ├── tsconfig.json
│   └── svelte.config.js
├── ARCHITECTURE.md
├── CLAUDE.md
├── IMPLEMENTATION.md
├── go.mod
├── go.sum
└── wails.json
```

---

## 2. Dependencies

### Go Dependencies

| Package | Version | Purpose |
|---------|---------|---------|
| `github.com/wailsapp/wails/v3` | `v3.0.0` | Desktop app framework (already in go.mod) |
| `github.com/google/uuid` | `v1.6.0` | ID generation (already indirect in go.mod) |
| `modernc.org/sqlite` | latest | Pure-Go SQLite driver — no CGO, no C toolchain |

Use `modernc.org/sqlite` — not `mattn/go-sqlite3`. The pure-Go driver eliminates the CGO dependency, simplifying cross-compilation. Register the driver as `"sqlite"` via the `modernc.org/sqlite` package's `init()` (it auto-registers as driver name `"sqlite"`).

### Frontend Dependencies

Add to `frontend/package.json` beyond what's already there:

| Package | Version | Purpose |
|---------|---------|---------|
| `svelte-markdown` | latest | Markdown rendering in message blocks |
| `shiki` | latest | Code syntax highlighting within markdown |

No UI component library. The ~15 components are small enough to build directly with Tailwind.

---

## 3. Database Schema (SQLite DDL)

All migrations run in `internal/store/store.go` inside `New()`. Single migration block for v1 — no migration versioning table needed yet.

```sql
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS threads (
    id             TEXT PRIMARY KEY,
    title          TEXT NOT NULL DEFAULT 'New Thread',
    provider       TEXT NOT NULL CHECK(provider IN ('claude', 'codex')),
    session_ref    TEXT,
    workspace_path TEXT NOT NULL,
    model          TEXT NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    archived       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_threads_updated ON threads(updated_at DESC);

CREATE TABLE IF NOT EXISTS payloads (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL,
    meta       TEXT NOT NULL DEFAULT '{}',
    data       BLOB NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS items (
    id          TEXT PRIMARY KEY,
    thread_id   TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    turn_index  INTEGER NOT NULL,
    item_index  INTEGER NOT NULL,
    kind        TEXT NOT NULL,
    role        TEXT NOT NULL DEFAULT 'assistant',
    summary     TEXT NOT NULL DEFAULT '',
    payload_id  TEXT REFERENCES payloads(id),
    created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_items_thread ON items(thread_id, turn_index, item_index);
```

Notes:
- `payloads` table must be created before `items` because `items.payload_id` has a foreign key to `payloads.id`.
- Timestamps are unix milliseconds (`time.Now().UnixMilli()`).
- `session_ref` stores the provider's own session/thread ID for resume (Claude session ID or Codex thread ID).
- `meta` in payloads is a JSON string containing preview info (see Section 11 for schemas).

---

## 4. Shared Types (Go)

File: `internal/provider/types.go`

```go
package provider

import (
	"encoding/json"
	"time"
)

// ProviderKind identifies the provider backend.
type ProviderKind string

const (
	Claude ProviderKind = "claude"
	Codex  ProviderKind = "codex"
)

// EventKind classifies provider events for triage routing.
type EventKind string

const (
	// Inline events — forwarded directly to frontend via app.Event.Emit.
	EventInit             EventKind = "init"
	EventTextDelta        EventKind = "text_delta"
	EventToolStart        EventKind = "tool_start"
	EventToolComplete     EventKind = "tool_complete"
	EventTurnStart        EventKind = "turn_start"
	EventTurnComplete     EventKind = "turn_complete"
	EventApprovalRequest  EventKind = "approval_request"
	EventApprovalResolved EventKind = "approval_resolved"
	EventSessionStatus    EventKind = "session_status"
	EventTokenUsage       EventKind = "token_usage"
	EventError            EventKind = "error"
	EventBackgroundStart  EventKind = "background_start"
	EventBackgroundDelta  EventKind = "background_delta"
	EventBackgroundComplete EventKind = "background_complete"

	// Heavy events — persisted to SQLite, meta emitted to frontend.
	EventDiff           EventKind = "diff"
	EventCommandOutput  EventKind = "command_output"
	EventThinking       EventKind = "thinking"
)

// ItemKind for persisted items in the database.
type ItemKind string

const (
	ItemText              ItemKind = "text"
	ItemToolCall          ItemKind = "tool_call"
	ItemToolResult        ItemKind = "tool_result"
	ItemThinking          ItemKind = "thinking"
	ItemDiff              ItemKind = "diff"
	ItemCommandExecution  ItemKind = "command_execution"
	ItemBackgroundStarted ItemKind = "background_started"
	ItemBackgroundDone    ItemKind = "background_done"
)

// ProviderEvent is the normalized event emitted by both provider protocols.
// The triage layer classifies these and routes them.
type ProviderEvent struct {
	Kind      EventKind       `json:"kind"`
	ThreadID  string          `json:"threadId"`
	TurnID    string          `json:"turnId,omitempty"`
	ItemID    string          `json:"itemId,omitempty"`
	ItemType  string          `json:"itemType,omitempty"`
	Content   string          `json:"content,omitempty"`
	Role      string          `json:"role,omitempty"`
	Meta      json.RawMessage `json:"meta,omitempty"`
	Timestamp time.Time       `json:"timestamp"`
	Raw       json.RawMessage `json:"-"`
}

// ApprovalRequest is sent when a provider needs user permission.
type ApprovalRequest struct {
	RequestID   string          `json:"requestId"`
	ThreadID    string          `json:"threadId"`
	TurnID      string          `json:"turnId"`
	ToolName    string          `json:"toolName"`
	Description string          `json:"description"`
	Input       json.RawMessage `json:"input"`
	Title       string          `json:"title"`
}

// ApprovalResponse is sent back to the provider.
type ApprovalResponse struct {
	RequestID string `json:"requestId"`
	Decision  string `json:"decision"` // "allow", "deny", "allow_session"
}

// SessionInfo contains metadata from the provider init/handshake.
type SessionInfo struct {
	SessionID string   `json:"sessionId"`
	Model     string   `json:"model"`
	CWD       string   `json:"cwd"`
	Tools     []string `json:"tools,omitempty"`
	Version   string   `json:"version,omitempty"`
}

// TokenUsage tracks token consumption for display.
type TokenUsage struct {
	InputTokens              int     `json:"inputTokens"`
	OutputTokens             int     `json:"outputTokens"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens,omitempty"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens,omitempty"`
	TotalCostUSD             float64 `json:"totalCostUsd,omitempty"`
}
```

---

## 5. Store Interface

### File: `internal/store/store.go`

```go
package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// Store wraps SQLite and provides all persistence operations.
type Store struct {
	db *sql.DB
}

// New opens (or creates) the SQLite database at the given path and runs migrations.
// Pass ":memory:" for tests.
func New(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}

	if err := runMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: run migrations: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// runMigrations executes the DDL from Section 3 as a single transaction.
func runMigrations(db *sql.DB) error {
	// PRAGMAs must execute outside the transaction.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return err
	}
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS threads (
			id             TEXT PRIMARY KEY,
			title          TEXT NOT NULL DEFAULT 'New Thread',
			provider       TEXT NOT NULL CHECK(provider IN ('claude', 'codex')),
			session_ref    TEXT,
			workspace_path TEXT NOT NULL,
			model          TEXT NOT NULL DEFAULT '',
			created_at     INTEGER NOT NULL,
			updated_at     INTEGER NOT NULL,
			archived       INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_threads_updated ON threads(updated_at DESC)`,
		`CREATE TABLE IF NOT EXISTS payloads (
			id         TEXT PRIMARY KEY,
			kind       TEXT NOT NULL,
			meta       TEXT NOT NULL DEFAULT '{}',
			data       BLOB NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS items (
			id          TEXT PRIMARY KEY,
			thread_id   TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
			turn_index  INTEGER NOT NULL,
			item_index  INTEGER NOT NULL,
			kind        TEXT NOT NULL,
			role        TEXT NOT NULL DEFAULT 'assistant',
			summary     TEXT NOT NULL DEFAULT '',
			payload_id  TEXT REFERENCES payloads(id),
			created_at  INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_items_thread ON items(thread_id, turn_index, item_index)`,
	}

	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("migration failed on %q: %w", stmt[:40], err)
		}
	}

	return tx.Commit()
}
```

### Model types: `internal/store/store.go` (bottom of same file)

```go
// Thread represents a conversation thread.
type Thread struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Provider      string `json:"provider"`
	SessionRef    string `json:"sessionRef,omitempty"`
	WorkspacePath string `json:"workspacePath"`
	Model         string `json:"model"`
	CreatedAt     int64  `json:"createdAt"`
	UpdatedAt     int64  `json:"updatedAt"`
	Archived      bool   `json:"archived"`
}

// Item represents a persisted timeline entry.
type Item struct {
	ID        string `json:"id"`
	ThreadID  string `json:"threadId"`
	TurnIndex int    `json:"turnIndex"`
	ItemIndex int    `json:"itemIndex"`
	Kind      string `json:"kind"`
	Role      string `json:"role"`
	Summary   string `json:"summary"`
	PayloadID string `json:"payloadId,omitempty"`
	CreatedAt int64  `json:"createdAt"`
}

// Payload represents heavy content stored for on-demand loading.
type Payload struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Meta      string `json:"meta"`
	Data      []byte `json:"data,omitempty"`
	CreatedAt int64  `json:"createdAt"`
}

// PayloadMeta is the meta-only view (no data blob).
type PayloadMeta struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Meta      string `json:"meta"`
	CreatedAt int64  `json:"createdAt"`
}
```

### File: `internal/store/threads.go`

```go
package store

import "fmt"

func (s *Store) CreateThread(t Thread) error {
	_, err := s.db.Exec(
		`INSERT INTO threads (id, title, provider, session_ref, workspace_path, model, created_at, updated_at, archived)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Title, t.Provider, nilIfEmpty(t.SessionRef), t.WorkspacePath, t.Model, t.CreatedAt, t.UpdatedAt, boolToInt(t.Archived),
	)
	if err != nil {
		return fmt.Errorf("store: create thread: %w", err)
	}
	return nil
}

func (s *Store) GetThread(id string) (Thread, error) {
	row := s.db.QueryRow(
		`SELECT id, title, provider, COALESCE(session_ref, ''), workspace_path, model, created_at, updated_at, archived
		 FROM threads WHERE id = ?`, id,
	)
	var t Thread
	var archived int
	err := row.Scan(&t.ID, &t.Title, &t.Provider, &t.SessionRef, &t.WorkspacePath, &t.Model, &t.CreatedAt, &t.UpdatedAt, &archived)
	if err != nil {
		return Thread{}, fmt.Errorf("store: get thread %s: %w", id, err)
	}
	t.Archived = archived != 0
	return t, nil
}

func (s *Store) ListThreads() ([]Thread, error) {
	rows, err := s.db.Query(
		`SELECT id, title, provider, COALESCE(session_ref, ''), workspace_path, model, created_at, updated_at, archived
		 FROM threads WHERE archived = 0 ORDER BY updated_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list threads: %w", err)
	}
	defer rows.Close()

	var threads []Thread
	for rows.Next() {
		var t Thread
		var archived int
		if err := rows.Scan(&t.ID, &t.Title, &t.Provider, &t.SessionRef, &t.WorkspacePath, &t.Model, &t.CreatedAt, &t.UpdatedAt, &archived); err != nil {
			return nil, fmt.Errorf("store: scan thread row: %w", err)
		}
		t.Archived = archived != 0
		threads = append(threads, t)
	}
	return threads, rows.Err()
}

func (s *Store) UpdateThread(t Thread) error {
	_, err := s.db.Exec(
		`UPDATE threads SET title=?, provider=?, session_ref=?, workspace_path=?, model=?, updated_at=?, archived=?
		 WHERE id=?`,
		t.Title, t.Provider, nilIfEmpty(t.SessionRef), t.WorkspacePath, t.Model, t.UpdatedAt, boolToInt(t.Archived), t.ID,
	)
	if err != nil {
		return fmt.Errorf("store: update thread %s: %w", t.ID, err)
	}
	return nil
}

func (s *Store) DeleteThread(id string) error {
	_, err := s.db.Exec(`DELETE FROM threads WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete thread %s: %w", id, err)
	}
	return nil
}

func (s *Store) ArchiveThread(id string) error {
	_, err := s.db.Exec(`UPDATE threads SET archived = 1, updated_at = ? WHERE id = ?`,
		nowMillis(), id)
	if err != nil {
		return fmt.Errorf("store: archive thread %s: %w", id, err)
	}
	return nil
}

func (s *Store) UpdateSessionRef(threadID, ref string) error {
	_, err := s.db.Exec(`UPDATE threads SET session_ref = ?, updated_at = ? WHERE id = ?`,
		ref, nowMillis(), threadID)
	if err != nil {
		return fmt.Errorf("store: update session ref for %s: %w", threadID, err)
	}
	return nil
}

// -- helpers --

func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nowMillis() int64 {
	return time.Now().UnixMilli()
}
```

Note: the `time` import is needed for `nowMillis()`. Add it to the imports.

### File: `internal/store/items.go`

```go
package store

import (
	"database/sql"
	"fmt"
)

func (s *Store) InsertItem(item Item) error {
	_, err := s.db.Exec(
		`INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, summary, payload_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID, item.ThreadID, item.TurnIndex, item.ItemIndex, item.Kind, item.Role, item.Summary,
		nilIfEmpty(item.PayloadID), item.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("store: insert item: %w", err)
	}
	// Touch the parent thread's updated_at.
	_, _ = s.db.Exec(`UPDATE threads SET updated_at = ? WHERE id = ?`, item.CreatedAt, item.ThreadID)
	return nil
}

func (s *Store) ListItems(threadID string) ([]Item, error) {
	rows, err := s.db.Query(
		`SELECT id, thread_id, turn_index, item_index, kind, role, summary, COALESCE(payload_id, ''), created_at
		 FROM items WHERE thread_id = ? ORDER BY turn_index, item_index`, threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list items for thread %s: %w", threadID, err)
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.ThreadID, &it.TurnIndex, &it.ItemIndex, &it.Kind, &it.Role, &it.Summary, &it.PayloadID, &it.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan item row: %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

func (s *Store) NextItemIndex(threadID string, turnIndex int) (int, error) {
	var maxIndex sql.NullInt64
	err := s.db.QueryRow(
		`SELECT MAX(item_index) FROM items WHERE thread_id = ? AND turn_index = ?`,
		threadID, turnIndex,
	).Scan(&maxIndex)
	if err != nil {
		return 0, fmt.Errorf("store: next item index: %w", err)
	}
	if !maxIndex.Valid {
		return 0, nil
	}
	return int(maxIndex.Int64) + 1, nil
}

func (s *Store) LastTurnIndex(threadID string) (int, error) {
	var maxIndex sql.NullInt64
	err := s.db.QueryRow(
		`SELECT MAX(turn_index) FROM items WHERE thread_id = ?`, threadID,
	).Scan(&maxIndex)
	if err != nil {
		return 0, fmt.Errorf("store: last turn index: %w", err)
	}
	if !maxIndex.Valid {
		return 0, nil
	}
	return int(maxIndex.Int64), nil
}
```

### File: `internal/store/payloads.go`

```go
package store

import "fmt"

func (s *Store) InsertPayload(p Payload) error {
	_, err := s.db.Exec(
		`INSERT INTO payloads (id, kind, meta, data, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		p.ID, p.Kind, p.Meta, p.Data, p.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("store: insert payload: %w", err)
	}
	return nil
}

func (s *Store) GetPayloadMeta(id string) (PayloadMeta, error) {
	row := s.db.QueryRow(
		`SELECT id, kind, meta, created_at FROM payloads WHERE id = ?`, id,
	)
	var pm PayloadMeta
	err := row.Scan(&pm.ID, &pm.Kind, &pm.Meta, &pm.CreatedAt)
	if err != nil {
		return PayloadMeta{}, fmt.Errorf("store: get payload meta %s: %w", id, err)
	}
	return pm, nil
}

func (s *Store) GetPayloadData(id string) ([]byte, error) {
	var data []byte
	err := s.db.QueryRow(`SELECT data FROM payloads WHERE id = ?`, id).Scan(&data)
	if err != nil {
		return nil, fmt.Errorf("store: get payload data %s: %w", id, err)
	}
	return data, nil
}

func (s *Store) ListPayloadMetas(threadID string) ([]PayloadMeta, error) {
	rows, err := s.db.Query(
		`SELECT p.id, p.kind, p.meta, p.created_at
		 FROM payloads p
		 INNER JOIN items i ON i.payload_id = p.id
		 WHERE i.thread_id = ?
		 ORDER BY i.turn_index, i.item_index`, threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list payload metas for thread %s: %w", threadID, err)
	}
	defer rows.Close()

	var metas []PayloadMeta
	for rows.Next() {
		var pm PayloadMeta
		if err := rows.Scan(&pm.ID, &pm.Kind, &pm.Meta, &pm.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan payload meta row: %w", err)
		}
		metas = append(metas, pm)
	}
	return metas, rows.Err()
}
```

---

## 6. Provider Process Management

File: `internal/provider/process.go`

```go
package provider

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const (
	// MaxLineSize is the max stdout line buffer: 10 MB.
	// Diffs and command outputs can be large.
	MaxLineSize = 10 * 1024 * 1024

	// shutdownGrace is how long to wait after closing stdin before sending SIGTERM.
	shutdownGrace = 3 * time.Second

	// killGrace is how long to wait after SIGTERM before sending SIGKILL.
	killGrace = 2 * time.Second
)

// Process manages a subprocess with stdin/stdout pipes.
type Process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	done   chan struct{}
	err    error
	mu     sync.Mutex
}

// SpawnConfig configures subprocess creation.
type SpawnConfig struct {
	Binary string
	Args   []string
	Dir    string
	Env    map[string]string
}

// Spawn starts a subprocess with stdin/stdout pipes and process group isolation.
// The context controls the lifetime — canceling it triggers Close().
func Spawn(ctx context.Context, cfg SpawnConfig) (*Process, error) {
	cmd := exec.CommandContext(ctx, cfg.Binary, cfg.Args...)
	cmd.Dir = cfg.Dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Build env: inherit current env + overrides.
	env := os.Environ()
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("provider: stdin pipe: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("provider: stdout pipe: %w", err)
	}

	// Forward stderr so provider debug output is visible during development.
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("provider: start %s: %w", cfg.Binary, err)
	}

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxLineSize)

	p := &Process{
		cmd:    cmd,
		stdin:  stdin,
		stdout: scanner,
		done:   make(chan struct{}),
	}

	// Wait goroutine: detect process exit.
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()

	return p, nil
}

// WriteLine writes a line to stdin (appends newline).
func (p *Process) WriteLine(data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	select {
	case <-p.done:
		return fmt.Errorf("provider: process already exited")
	default:
	}

	buf := make([]byte, len(data)+1)
	copy(buf, data)
	buf[len(data)] = '\n'
	_, err := p.stdin.Write(buf)
	if err != nil {
		return fmt.Errorf("provider: write to stdin: %w", err)
	}
	return nil
}

// ReadLine reads the next line from stdout. Returns io.EOF when process exits.
func (p *Process) ReadLine() ([]byte, error) {
	if p.stdout.Scan() {
		// Return a copy — scanner reuses its buffer.
		line := p.stdout.Bytes()
		out := make([]byte, len(line))
		copy(out, line)
		return out, nil
	}
	if err := p.stdout.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

// Done returns a channel that closes when the process exits.
func (p *Process) Done() <-chan struct{} {
	return p.done
}

// Err returns the process exit error. Only valid after Done() is closed.
func (p *Process) Err() error {
	return p.err
}

// Close performs graceful shutdown:
// 1. Close stdin
// 2. Wait shutdownGrace for natural exit
// 3. SIGTERM the process group
// 4. Wait killGrace
// 5. SIGKILL the process group
func (p *Process) Close() error {
	// Close stdin to signal the process to exit.
	p.stdin.Close()

	select {
	case <-p.done:
		return p.err
	case <-time.After(shutdownGrace):
	}

	// SIGTERM the process group.
	p.signalGroup(syscall.SIGTERM)

	select {
	case <-p.done:
		return p.err
	case <-time.After(killGrace):
	}

	// SIGKILL the process group.
	return p.Kill()
}

// Kill immediately kills the process group.
func (p *Process) Kill() error {
	p.signalGroup(syscall.SIGKILL)
	<-p.done
	return p.err
}

// signalGroup sends a signal to the process group.
func (p *Process) signalGroup(sig syscall.Signal) {
	if p.cmd.Process == nil {
		return
	}
	// Negative PID sends to the process group.
	_ = syscall.Kill(-p.cmd.Process.Pid, sig)
}
```

---

## 7. Claude Provider

File: `internal/provider/claude/claude.go`

```go
package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strconv"
	"time"

	"agent-overflow/internal/provider"

	"github.com/google/uuid"
)

// Session manages a Claude Code CLI subprocess.
type Session struct {
	proc      *provider.Process
	threadID  string
	sessionID string
	model     string
	onEvent   func(provider.ProviderEvent)
	cancel    context.CancelFunc
}

// Config for creating a Claude session.
type Config struct {
	Binary         string   // default: "claude"
	Model          string
	WorkDir        string
	Resume         string   // session ID to resume, empty for new
	SystemPrompt   string
	AllowedTools   []string
	PermissionMode string   // "default", "acceptEdits", "bypassPermissions"
	MaxTurns       int
}

// NewSession spawns a Claude CLI process and starts the stdout reader goroutine.
// The init event arrives after the first Send() call, not on spawn.
func NewSession(ctx context.Context, threadID string, cfg Config, onEvent func(provider.ProviderEvent)) (*Session, error) {
	binary := cfg.Binary
	if binary == "" {
		binary = "claude"
	}

	args := []string{
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	}

	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.Resume != "" {
		args = append(args, "--resume", cfg.Resume)
	}
	if cfg.SystemPrompt != "" {
		args = append(args, "--system-prompt", cfg.SystemPrompt)
	}
	if cfg.PermissionMode != "" && cfg.PermissionMode != "default" {
		args = append(args, "--permission-mode", cfg.PermissionMode)
	}
	if cfg.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(cfg.MaxTurns))
	}
	for _, tool := range cfg.AllowedTools {
		args = append(args, "--allowedTools", tool)
	}

	childCtx, cancel := context.WithCancel(ctx)

	proc, err := provider.Spawn(childCtx, provider.SpawnConfig{
		Binary: binary,
		Args:   args,
		Dir:    cfg.WorkDir,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("claude: spawn: %w", err)
	}

	s := &Session{
		proc:     proc,
		threadID: threadID,
		model:    cfg.Model,
		onEvent:  onEvent,
		cancel:   cancel,
	}

	// Start stdout reader goroutine.
	go s.readLoop()

	return s, nil
}

// Send sends a user message. The message is written as a JSON object to stdin.
// Format: {"type":"user","message":{"role":"user","content":"<text>"}}
func (s *Session) Send(ctx context.Context, content string) error {
	msg := map[string]any{
		"type": "user",
		"message": map[string]string{
			"role":    "user",
			"content": content,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("claude: marshal user message: %w", err)
	}
	return s.proc.WriteLine(data)
}

// Interrupt sends a control interrupt to the CLI.
// Format: {"type":"control","control":{"type":"interrupt"}}
func (s *Session) Interrupt(ctx context.Context) error {
	msg := map[string]any{
		"type": "control",
		"control": map[string]string{
			"type": "interrupt",
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("claude: marshal interrupt: %w", err)
	}
	return s.proc.WriteLine(data)
}

// RespondToApproval sends a tool-use approval decision back to the CLI.
// Allow format:  {"type":"control_response","response":{"subtype":"success","request_id":"<id>","response":{"behavior":"allow"}}}
// Deny format:   {"type":"control_response","response":{"subtype":"success","request_id":"<id>","response":{"behavior":"deny","message":"User denied"}}}
func (s *Session) RespondToApproval(ctx context.Context, resp provider.ApprovalResponse) error {
	var behavior map[string]any
	if resp.Decision == "allow" || resp.Decision == "allow_session" {
		behavior = map[string]any{"behavior": "allow"}
	} else {
		behavior = map[string]any{"behavior": "deny", "message": "User denied"}
	}
	msg := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": resp.RequestID,
			"response":   behavior,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("claude: marshal approval response: %w", err)
	}
	return s.proc.WriteLine(data)
}

// SessionID returns the provider's session identifier.
// Only valid after the init event has been received.
func (s *Session) SessionID() string {
	return s.sessionID
}

// Close shuts down the CLI process gracefully.
func (s *Session) Close() error {
	s.cancel()
	return s.proc.Close()
}

// readLoop reads stdout NDJSON lines and dispatches them as ProviderEvents.
func (s *Session) readLoop() {
	defer func() {
		s.onEvent(provider.ProviderEvent{
			Kind:      provider.EventSessionStatus,
			ThreadID:  s.threadID,
			Content:   "disconnected",
			Timestamp: time.Now(),
		})
	}()

	for {
		line, err := s.proc.ReadLine()
		if err != nil {
			if err != io.EOF {
				s.onEvent(provider.ProviderEvent{
					Kind:      provider.EventError,
					ThreadID:  s.threadID,
					Content:   fmt.Sprintf("claude: read error: %v", err),
					Timestamp: time.Now(),
				})
			}
			return
		}

		events, err := ParseLine(s.threadID, line)
		if err != nil {
			log.Printf("claude: parse error: %v (line: %s)", err, string(line[:min(len(line), 200)]))
			continue
		}

		for _, evt := range events {
			// Capture session ID from init event.
			if evt.Kind == provider.EventInit && evt.Meta != nil {
				var info provider.SessionInfo
				if json.Unmarshal(evt.Meta, &info) == nil && info.SessionID != "" {
					s.sessionID = info.SessionID
				}
			}
			s.onEvent(evt)
		}
	}
}

// Note: Go 1.21+ has a built-in min() function. Do not define a custom one.
```

---

## 8. Claude Protocol

File: `internal/provider/claude/protocol.go`

Each stdout line is a JSON object. The `type` field is the discriminator.

### Wire message types

| `type` field | Description |
|---|---|
| `"system"` | System events, discriminated by `subtype` |
| `"assistant"` | Assistant response with content blocks |
| `"user"` | Echoed user messages / tool results |
| `"result"` | Final turn result |
| `"stream_event"` | Token-level partial streaming delta |
| `"control_request"` | Permission/approval request from CLI |

### ParseLine implementation

```go
package claude

import (
	"encoding/json"
	"fmt"
	"time"

	"agent-overflow/internal/provider"
)

// ParseLine parses a single NDJSON line from Claude CLI stdout
// and returns zero or more ProviderEvents.
func ParseLine(threadID string, line []byte) ([]provider.ProviderEvent, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}

	var msgType string
	if err := json.Unmarshal(raw["type"], &msgType); err != nil {
		return nil, fmt.Errorf("missing or invalid type field")
	}

	now := time.Now()

	switch msgType {
	case "system":
		return parseSystem(threadID, raw, now, line)
	case "assistant":
		return parseAssistant(threadID, raw, now, line)
	case "user":
		// Echoed tool results. Skip for now — we track tool results
		// from the tool_complete flow.
		return nil, nil
	case "result":
		return parseResult(threadID, raw, now, line)
	case "stream_event":
		return parseStreamEvent(threadID, raw, now)
	case "control_request":
		return parseControlRequest(threadID, raw, now, line)
	default:
		// Unknown type — skip gracefully.
		return nil, nil
	}
}
```

### System subtype handling

```go
func parseSystem(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
	var subtype string
	if err := json.Unmarshal(raw["subtype"], &subtype); err != nil {
		return nil, nil // no subtype — skip
	}

	switch subtype {
	case "init":
		// Extract session info from the init payload.
		meta, _ := json.Marshal(extractSessionInfo(raw))
		return []provider.ProviderEvent{{
			Kind:      provider.EventInit,
			ThreadID:  threadID,
			Meta:      meta,
			Timestamp: now,
			Raw:       line,
		}}, nil

	case "session_state_changed":
		return []provider.ProviderEvent{{
			Kind:      provider.EventSessionStatus,
			ThreadID:  threadID,
			Content:   "session_state_changed",
			Meta:      raw["data"],
			Timestamp: now,
		}}, nil

	// Explicitly skipped subtypes — no action, no error.
	case "compact_boundary",
		"api_retry",
		"hook_started", "hook_progress", "hook_response",
		"tool_progress",
		"notification",
		"files_persisted",
		"tool_use_summary",
		"memory_recall":
		return nil, nil

	default:
		// Unknown system subtype — skip.
		return nil, nil
	}
}
```

### Assistant message parsing

```go
func parseAssistant(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
	// The assistant message has a "message" field containing content blocks.
	var msg struct {
		ID      string `json:"id"`
		Content []struct {
			Type    string `json:"type"`
			Text    string `json:"text,omitempty"`
			ID      string `json:"id,omitempty"`
			Name    string `json:"name,omitempty"`
			Input   json.RawMessage `json:"input,omitempty"`
			Thinking string `json:"thinking,omitempty"`
		} `json:"content"`
		Role  string `json:"role"`
		Usage *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage,omitempty"`
	}

	// The message payload is under "message" key for assistant type.
	if rawMsg, ok := raw["message"]; ok {
		if err := json.Unmarshal(rawMsg, &msg); err != nil {
			return nil, fmt.Errorf("parse assistant message: %w", err)
		}
	} else {
		// Might be flat — try parsing raw directly.
		data, _ := json.Marshal(raw)
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, nil
		}
	}

	var events []provider.ProviderEvent

	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			events = append(events, provider.ProviderEvent{
				Kind:      provider.EventTextDelta,
				ThreadID:  threadID,
				ItemID:    msg.ID,
				Content:   block.Text,
				Role:      "assistant",
				Timestamp: now,
			})

		case "tool_use":
			meta, _ := json.Marshal(map[string]any{
				"toolName": block.Name,
				"input":    block.Input,
			})
			events = append(events, provider.ProviderEvent{
				Kind:      provider.EventToolStart,
				ThreadID:  threadID,
				ItemID:    block.ID,
				ItemType:  block.Name,
				Meta:      meta,
				Timestamp: now,
			})

		case "thinking":
			events = append(events, provider.ProviderEvent{
				Kind:      provider.EventThinking,
				ThreadID:  threadID,
				ItemID:    msg.ID,
				Content:   block.Thinking,
				Timestamp: now,
			})
		}
	}

	// Emit token usage if present.
	if msg.Usage != nil {
		usageMeta, _ := json.Marshal(provider.TokenUsage{
			InputTokens:              msg.Usage.InputTokens,
			OutputTokens:             msg.Usage.OutputTokens,
			CacheReadInputTokens:     msg.Usage.CacheReadInputTokens,
			CacheCreationInputTokens: msg.Usage.CacheCreationInputTokens,
		})
		events = append(events, provider.ProviderEvent{
			Kind:      provider.EventTokenUsage,
			ThreadID:  threadID,
			Meta:      usageMeta,
			Timestamp: now,
		})
	}

	return events, nil
}
```

### Result parsing

```go
func parseResult(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
	var result struct {
		IsError bool   `json:"is_error"`
		Error   string `json:"error,omitempty"`
		// Result can have various shapes — we extract what we need.
		SessionID string `json:"session_id,omitempty"`
	}

	data, _ := json.Marshal(raw)
	_ = json.Unmarshal(data, &result)

	if result.IsError {
		return []provider.ProviderEvent{{
			Kind:      provider.EventError,
			ThreadID:  threadID,
			Content:   result.Error,
			Timestamp: now,
			Raw:       line,
		}}, nil
	}

	return []provider.ProviderEvent{{
		Kind:      provider.EventTurnComplete,
		ThreadID:  threadID,
		Timestamp: now,
		Raw:       line,
	}}, nil
}
```

### Stream event parsing

```go
func parseStreamEvent(threadID string, raw map[string]json.RawMessage, now time.Time) ([]provider.ProviderEvent, error) {
	// stream_event carries partial token deltas.
	var evt struct {
		Event string `json:"event"`
		Data  struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta,omitempty"`
		} `json:"data,omitempty"`
	}

	data, _ := json.Marshal(raw)
	_ = json.Unmarshal(data, &evt)

	if evt.Data.Delta.Text != "" {
		return []provider.ProviderEvent{{
			Kind:      provider.EventTextDelta,
			ThreadID:  threadID,
			Content:   evt.Data.Delta.Text,
			Role:      "assistant",
			Timestamp: now,
		}}, nil
	}

	return nil, nil
}
```

### Control request (approval) parsing

```go
// parseControlRequest handles the wire format:
// {"type":"control_request","request_id":"req_1_abc","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}
func parseControlRequest(threadID string, raw map[string]json.RawMessage, now time.Time, line []byte) ([]provider.ProviderEvent, error) {
	var msg struct {
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype  string          `json:"subtype"`
			ToolName string          `json:"tool_name"`
			Input    json.RawMessage `json:"input"`
		} `json:"request"`
	}

	data, _ := json.Marshal(raw)
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, nil
	}

	if msg.Request.Subtype != "can_use_tool" {
		return nil, nil
	}

	approvalMeta, _ := json.Marshal(provider.ApprovalRequest{
		RequestID:   msg.RequestID,
		ThreadID:    threadID,
		ToolName:    msg.Request.ToolName,
		Description: fmt.Sprintf("Allow %s?", msg.Request.ToolName),
		Input:       msg.Request.Input,
		Title:       msg.Request.ToolName,
	})

	return []provider.ProviderEvent{{
		Kind:      provider.EventApprovalRequest,
		ThreadID:  threadID,
		ItemID:    msg.RequestID,
		Meta:      approvalMeta,
		Timestamp: now,
		Raw:       line,
	}}, nil
}
```

### Session info extraction helper

```go
// extractSessionInfo reads fields from the init message top level.
// Wire format: {"type":"system","subtype":"init","session_id":"...","model":"...","cwd":"...","tools":[...],"claude_code_version":"..."}
func extractSessionInfo(raw map[string]json.RawMessage) provider.SessionInfo {
	var info provider.SessionInfo

	// All fields are at the message top level (same level as "type" and "subtype").
	if v, ok := raw["session_id"]; ok {
		json.Unmarshal(v, &info.SessionID)
	}
	if v, ok := raw["model"]; ok {
		json.Unmarshal(v, &info.Model)
	}
	if v, ok := raw["cwd"]; ok {
		json.Unmarshal(v, &info.CWD)
	}
	if v, ok := raw["tools"]; ok {
		json.Unmarshal(v, &info.Tools)
	}
	if v, ok := raw["claude_code_version"]; ok {
		json.Unmarshal(v, &info.Version)
	}

	return info
}
```

> **Testing note:** The wire format shapes above are based on SDK source code analysis. The PROMPT should instruct the agent to validate the actual wire format by testing against a real Claude CLI / Codex app-server process in `/tmp` before committing protocol code. Run `echo '{"type":"user","message":{"role":"user","content":"hello"}}' | claude --input-format stream-json --output-format stream-json --verbose 2>/dev/null | head -5` to capture real output samples.

---

## 9. Codex Provider

File: `internal/provider/codex/codex.go`

```go
package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/provider"

	"github.com/google/uuid"
)

// Session manages a Codex app-server subprocess.
type Session struct {
	proc          *provider.Process
	threadID      string // our internal thread ID
	codexThreadID string // the Codex app-server's thread ID from thread/start
	nextID        atomic.Int64
	mu            sync.Mutex
	pending       map[int64]chan json.RawMessage
	onEvent       func(provider.ProviderEvent)
	cancel        context.CancelFunc
}

// Config for creating a Codex session.
type Config struct {
	Binary         string // default: "codex"
	Model          string
	WorkDir        string
	ApprovalPolicy string // "never", "on-failure", "on-request", "untrusted"
	Sandbox        string // "read-only", "workspace-write", "danger-full-access"
	ResumeThreadID string // thread ID to resume, empty for new
	SystemPrompt   string
}

// NewSession spawns codex app-server, performs the initialize handshake,
// and starts (or resumes) a thread. Returns after handshake completes.
func NewSession(ctx context.Context, threadID string, cfg Config, onEvent func(provider.ProviderEvent)) (*Session, error) {
	binary := cfg.Binary
	if binary == "" {
		binary = "codex"
	}

	childCtx, cancel := context.WithCancel(ctx)

	proc, err := provider.Spawn(childCtx, provider.SpawnConfig{
		Binary: binary,
		Args:   []string{"app-server"},
		Dir:    cfg.WorkDir,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("codex: spawn: %w", err)
	}

	s := &Session{
		proc:    proc,
		threadID: threadID,
		pending: make(map[int64]chan json.RawMessage),
		onEvent: onEvent,
		cancel:  cancel,
	}

	// Start stdout reader goroutine before sending any requests.
	go s.readLoop()

	// Initialize handshake.
	_, err = s.sendRequest(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "agent_overflow",
			"title":   "Agent Overflow",
			"version": "0.1.0",
		},
		"capabilities": map[string]any{
			"experimentalApi": true,
		},
	})
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("codex: initialize handshake failed: %w", err)
	}

	// Send initialized notification (no id, no response expected).
	s.writeNotification("initialized", nil)

	// Start or resume thread.
	threadParams := buildThreadParams(cfg)
	var method string
	if cfg.ResumeThreadID != "" {
		method = "thread/resume"
		threadParams["threadId"] = cfg.ResumeThreadID
	} else {
		method = "thread/start"
	}

	resp, err := s.sendRequest(ctx, method, threadParams)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("codex: %s failed: %w", method, err)
	}

	// Extract the Codex thread ID from response.
	s.threadID = threadID // our internal ID
	s.codexThreadID = readStringFromResponse(resp, "thread", "id")
	if s.codexThreadID != "" {
		// Emit init event with the provider thread ID.
		meta, _ := json.Marshal(provider.SessionInfo{
			SessionID: s.codexThreadID,
			Model:     cfg.Model,
			CWD:       cfg.WorkDir,
		})
		s.onEvent(provider.ProviderEvent{
			Kind:      provider.EventInit,
			ThreadID:  threadID,
			Meta:      meta,
			Timestamp: time.Now(),
		})
	}

	return s, nil
}

// Send sends a user turn via turn/start.
func (s *Session) Send(ctx context.Context, content string) error {
	params := map[string]any{
		"threadId": s.codexThreadID,
		"input": []map[string]any{{
			"type":          "text",
			"text":          content,
			"text_elements": []any{},
		}},
	}

	resp, err := s.sendRequest(ctx, "turn/start", params)
	if err != nil {
		return fmt.Errorf("codex: turn/start: %w", err)
	}

	// Extract turn ID from response.
	turnID := readStringFromResponse(resp, "turn", "id")
	if turnID != "" {
		s.onEvent(provider.ProviderEvent{
			Kind:      provider.EventTurnStart,
			ThreadID:  s.threadID,
			TurnID:    turnID,
			Timestamp: time.Now(),
		})
	}

	return nil
}

// Interrupt sends turn/interrupt for the given turn.
func (s *Session) Interrupt(ctx context.Context, turnID string) error {
	_, err := s.sendRequest(ctx, "turn/interrupt", map[string]any{
		"turnId": turnID,
	})
	return err
}

// RespondToApproval responds to a server approval request by sending a JSON-RPC response.
func (s *Session) RespondToApproval(ctx context.Context, jsonRpcID int64, decision string) error {
	var result any
	switch decision {
	case "allow":
		result = map[string]any{"decision": "accept"}
	case "allow_session":
		result = map[string]any{"decision": "acceptForSession"}
	default:
		result = map[string]any{"decision": "decline"}
	}

	return s.writeResponse(jsonRpcID, result)
}

// ThreadID returns the Codex thread identifier.
func (s *Session) ThreadID() string {
	return s.threadID
}

// Close shuts down the app-server process.
func (s *Session) Close() error {
	s.cancel()
	return s.proc.Close()
}

// -- Internal methods --

// sendRequest sends a JSON-RPC request and waits for a response.
func (s *Session) sendRequest(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := s.nextID.Add(1)

	ch := make(chan json.RawMessage, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		msg["params"] = params
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("codex: marshal request: %w", err)
	}

	if err := s.proc.WriteLine(data); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-ch:
		// Check for error in response.
		var rpcResp struct {
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error,omitempty"`
			Result json.RawMessage `json:"result,omitempty"`
		}
		if json.Unmarshal(resp, &rpcResp) == nil && rpcResp.Error != nil {
			return nil, fmt.Errorf("codex: %s: %s (code %d)", method, rpcResp.Error.Message, rpcResp.Error.Code)
		}
		return resp, nil
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("codex: %s: timeout", method)
	}
}

// writeNotification sends a JSON-RPC notification (no id, no response expected).
func (s *Session) writeNotification(method string, params any) error {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		msg["params"] = params
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("codex: marshal notification: %w", err)
	}
	return s.proc.WriteLine(data)
}

// writeResponse sends a JSON-RPC response (to server requests like approvals).
func (s *Session) writeResponse(id int64, result any) error {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("codex: marshal response: %w", err)
	}
	return s.proc.WriteLine(data)
}

// readLoop reads stdout and dispatches JSON-RPC messages.
func (s *Session) readLoop() {
	defer func() {
		// Unblock all pending requests.
		s.mu.Lock()
		for id, ch := range s.pending {
			close(ch)
			delete(s.pending, id)
		}
		s.mu.Unlock()

		s.onEvent(provider.ProviderEvent{
			Kind:      provider.EventSessionStatus,
			ThreadID:  s.threadID,
			Content:   "disconnected",
			Timestamp: time.Now(),
		})
	}()

	for {
		line, err := s.proc.ReadLine()
		if err != nil {
			if err != io.EOF {
				s.onEvent(provider.ProviderEvent{
					Kind:      provider.EventError,
					ThreadID:  s.threadID,
					Content:   fmt.Sprintf("codex: read error: %v", err),
					Timestamp: time.Now(),
				})
			}
			return
		}

		s.dispatchLine(line)
	}
}

// dispatchLine classifies a JSON-RPC line and routes it.
func (s *Session) dispatchLine(line []byte) {
	var msg struct {
		ID     *json.Number    `json:"id,omitempty"`
		Method string          `json:"method,omitempty"`
		Result json.RawMessage `json:"result,omitempty"`
		Error  json.RawMessage `json:"error,omitempty"`
		Params json.RawMessage `json:"params,omitempty"`
	}

	if err := json.Unmarshal(line, &msg); err != nil {
		log.Printf("codex: invalid JSON line: %v", err)
		return
	}

	// Response: has id, no method.
	if msg.ID != nil && msg.Method == "" {
		id, err := msg.ID.Int64()
		if err != nil {
			return
		}
		s.mu.Lock()
		ch, ok := s.pending[id]
		s.mu.Unlock()
		if ok {
			ch <- line
		}
		return
	}

	// Server request: has both id and method (approval flow).
	if msg.ID != nil && msg.Method != "" {
		s.handleServerRequest(msg.Method, msg.ID, msg.Params, line)
		return
	}

	// Notification: has method, no id.
	if msg.Method != "" {
		events := ClassifyNotification(s.threadID, msg.Method, msg.Params)
		for _, evt := range events {
			s.onEvent(evt)
		}
		return
	}
}

// handleServerRequest processes server-initiated requests (approvals).
func (s *Session) handleServerRequest(method string, id *json.Number, params json.RawMessage, line []byte) {
	rpcID, _ := id.Int64()

	// All server requests with known approval methods get emitted as approval requests.
	switch method {
	case "item/commandExecution/requestApproval",
		"item/fileChange/requestApproval",
		"item/fileRead/requestApproval",
		"item/tool/requestUserInput",
		"item/permissions/requestApproval":

		meta := buildApprovalMeta(s.threadID, method, rpcID, params)
		s.onEvent(provider.ProviderEvent{
			Kind:      provider.EventApprovalRequest,
			ThreadID:  s.threadID,
			Meta:      meta,
			Timestamp: time.Now(),
			Raw:       line,
		})

	default:
		// Unknown server request — respond with error so we don't block.
		s.writeResponse(rpcID, map[string]any{
			"error": map[string]any{
				"code":    -32601,
				"message": fmt.Sprintf("unsupported server request: %s", method),
			},
		})
	}
}

// -- helpers --

func buildThreadParams(cfg Config) map[string]any {
	params := map[string]any{}

	if cfg.Model != "" {
		params["model"] = cfg.Model
	}

	// Map approval policy + sandbox to Codex runtime mode fields.
	if cfg.Sandbox != "" {
		switch cfg.Sandbox {
		case "danger-full-access":
			params["approvalPolicy"] = "never"
			params["sandboxPolicy"] = "none"
		case "workspace-write":
			params["approvalPolicy"] = cfg.ApprovalPolicy
			params["sandboxPolicy"] = "workspace"
		default:
			params["approvalPolicy"] = cfg.ApprovalPolicy
			params["sandboxPolicy"] = "read-only"
		}
	}

	if cfg.SystemPrompt != "" {
		params["baseInstructions"] = cfg.SystemPrompt
	}

	return params
}

func buildApprovalMeta(threadID, method string, rpcID int64, params json.RawMessage) json.RawMessage {
	var parsed map[string]json.RawMessage
	_ = json.Unmarshal(params, &parsed)

	toolName := method // fallback
	description := method
	var input json.RawMessage
	title := method

	// Try to extract better info from params.
	if cmd, ok := parsed["command"]; ok {
		var cmdStr string
		if json.Unmarshal(cmd, &cmdStr) == nil {
			toolName = "command"
			description = cmdStr
			title = "Run command"
			input = cmd
		}
	}
	if filePath, ok := parsed["filePath"]; ok {
		var fp string
		if json.Unmarshal(filePath, &fp) == nil {
			toolName = "file_change"
			description = fp
			title = "File change"
			input = params
		}
	}

	approval := provider.ApprovalRequest{
		RequestID:   fmt.Sprintf("%d", rpcID),
		ThreadID:    threadID,
		ToolName:    toolName,
		Description: description,
		Input:       input,
		Title:       title,
	}

	data, _ := json.Marshal(approval)
	return data
}

func readStringFromResponse(data json.RawMessage, keys ...string) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}
	for i, key := range keys {
		raw, ok := m[key]
		if !ok {
			return ""
		}
		if i == len(keys)-1 {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				return s
			}
			return ""
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return ""
		}
	}
	return ""
}
```

---

## 10. Codex Protocol

File: `internal/provider/codex/protocol.go`

```go
package codex

import (
	"encoding/json"
	"time"

	"agent-overflow/internal/provider"
)

// ClassifyNotification converts a Codex app-server notification into
// zero or more ProviderEvents. Unrecognized methods are silently skipped.
func ClassifyNotification(threadID, method string, params json.RawMessage) []provider.ProviderEvent {
	now := time.Now()

	switch method {

	// --- Handle and emit ---

	case "turn/started":
		turnID := readNestedString(params, "turn", "id")
		return []provider.ProviderEvent{{
			Kind:      provider.EventTurnStart,
			ThreadID:  threadID,
			TurnID:    turnID,
			Timestamp: now,
		}}

	case "turn/completed":
		turnID := readNestedString(params, "turn", "id")
		status := readNestedString(params, "turn", "status")
		errorMsg := readNestedString(params, "turn", "error", "message")
		if status == "failed" && errorMsg != "" {
			return []provider.ProviderEvent{
				{Kind: provider.EventError, ThreadID: threadID, TurnID: turnID, Content: errorMsg, Timestamp: now},
				{Kind: provider.EventTurnComplete, ThreadID: threadID, TurnID: turnID, Timestamp: now},
			}
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventTurnComplete,
			ThreadID:  threadID,
			TurnID:    turnID,
			Timestamp: now,
		}}

	case "turn/diff/updated":
		return []provider.ProviderEvent{{
			Kind:      provider.EventDiff,
			ThreadID:  threadID,
			Content:   readTopLevelString(params, "diff"),
			Meta:      params,
			Timestamp: now,
		}}

	case "item/started":
		itemID := readNestedString(params, "item", "id")
		itemType := readNestedString(params, "item", "type")
		return []provider.ProviderEvent{{
			Kind:      provider.EventToolStart,
			ThreadID:  threadID,
			ItemID:    itemID,
			ItemType:  itemType,
			Meta:      params,
			Timestamp: now,
		}}

	case "item/completed":
		itemID := readNestedString(params, "item", "id")
		itemType := readNestedString(params, "item", "type")
		return []provider.ProviderEvent{{
			Kind:      provider.EventToolComplete,
			ThreadID:  threadID,
			ItemID:    itemID,
			ItemType:  itemType,
			Meta:      params,
			Timestamp: now,
		}}

	case "item/agentMessage/delta":
		delta := readTopLevelString(params, "delta")
		if delta == "" {
			return nil
		}
		return []provider.ProviderEvent{{
			Kind:      provider.EventTextDelta,
			ThreadID:  threadID,
			Content:   delta,
			Role:      "assistant",
			Timestamp: now,
		}}

	case "item/commandExecution/outputDelta":
		return []provider.ProviderEvent{{
			Kind:      provider.EventCommandOutput,
			ThreadID:  threadID,
			Content:   readTopLevelString(params, "delta"),
			Meta:      params,
			Timestamp: now,
		}}

	case "item/fileChange/outputDelta":
		return []provider.ProviderEvent{{
			Kind:      provider.EventDiff,
			ThreadID:  threadID,
			Content:   readTopLevelString(params, "delta"),
			Meta:      params,
			Timestamp: now,
		}}

	case "thread/tokenUsage/updated":
		return []provider.ProviderEvent{{
			Kind:      provider.EventTokenUsage,
			ThreadID:  threadID,
			Meta:      params,
			Timestamp: now,
		}}

	case "error":
		errorMsg := readNestedString(params, "error", "message")
		return []provider.ProviderEvent{{
			Kind:      provider.EventError,
			ThreadID:  threadID,
			Content:   errorMsg,
			Meta:      params,
			Timestamp: now,
		}}

	case "turn/plan/updated":
		return []provider.ProviderEvent{{
			Kind:      provider.EventSessionStatus,
			ThreadID:  threadID,
			Content:   "plan_updated",
			Meta:      params,
			Timestamp: now,
		}}

	// --- Explicitly skipped notifications ---

	case "thread/started",
		"thread/status/changed",
		"thread/name/updated",
		"thread/archived",
		"thread/unarchived",
		"thread/closed",
		"thread/compacted",
		"item/autoApprovalReview/started",
		"item/autoApprovalReview/completed",
		"item/reasoning/textDelta",
		"item/reasoning/summaryTextDelta",
		"item/reasoning/summaryPartAdded",
		"item/mcpToolCall/progress",
		"serverRequest/resolved",
		"account/updated",
		"account/rateLimits/updated",
		"account/login/completed",
		"model/rerouted",
		"configWarning",
		"deprecationNotice":
		return nil

	default:
		// Catch-all for unrecognized methods. Also covers glob patterns:
		// mcpServer/*, skills/changed, app/list/updated, fs/changed,
		// hook/started, hook/completed, windows/*, thread/realtime/*
		// Log debug if desired; never crash.
		return nil
	}
}

// -- JSON helpers --

// readTopLevelString reads a string from the top level of a JSON object.
func readTopLevelString(data json.RawMessage, key string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// readNestedString reads a string by walking through nested object keys.
// E.g., readNestedString(data, "turn", "id") reads data.turn.id.
func readNestedString(data json.RawMessage, keys ...string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	for i, key := range keys {
		raw, ok := m[key]
		if !ok {
			return ""
		}
		if i == len(keys)-1 {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				return s
			}
			return ""
		}
		if json.Unmarshal(raw, &m) != nil {
			return ""
		}
	}
	return ""
}
```

> **Testing note:** The wire format shapes above are based on SDK source code analysis. The PROMPT should instruct the agent to validate the actual wire format by testing against a real Claude CLI / Codex app-server process in `/tmp` before committing protocol code. Run `echo '{"type":"user","message":{"role":"user","content":"hello"}}' | claude --input-format stream-json --output-format stream-json --verbose 2>/dev/null | head -5` to capture real output samples.

---

## 11. Triage Layer

### File: `internal/triage/triage.go`

```go
package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// Router classifies provider events and routes them.
type Router struct {
	store            *store.Store
	emit             func(eventName string, data any) // wraps app.Event.Emit
	textAccumulators map[string]*strings.Builder       // threadID → accumulated assistant text
}

// NewRouter creates a triage router.
func NewRouter(st *store.Store, emit func(eventName string, data any)) *Router {
	return &Router{
		store:            st,
		emit:             emit,
		textAccumulators: make(map[string]*strings.Builder),
	}
}

// Handle processes a provider event: persists heavy payloads, forwards inline events.
func (r *Router) Handle(evt provider.ProviderEvent) error {
	switch evt.Kind {

	// --- Inline events: forward directly ---

	case provider.EventTextDelta:
		// Accumulate text for persistence and emit to frontend.
		acc, ok := r.textAccumulators[evt.ThreadID]
		if !ok {
			acc = &strings.Builder{}
			r.textAccumulators[evt.ThreadID] = acc
		}
		acc.WriteString(evt.Content)
		r.emit("provider:event", evt)
		return nil

	case provider.EventToolStart,
		provider.EventToolComplete,
		provider.EventTurnStart,
		provider.EventApprovalRequest,
		provider.EventApprovalResolved,
		provider.EventSessionStatus,
		provider.EventTokenUsage,
		provider.EventError,
		provider.EventBackgroundStart:
		r.emit("provider:event", evt)
		return nil

	case provider.EventInit:
		r.emit("provider:event", evt)
		// Update thread session_ref from init metadata.
		if evt.Meta != nil {
			var info provider.SessionInfo
			if json.Unmarshal(evt.Meta, &info) == nil && info.SessionID != "" {
				if err := r.store.UpdateSessionRef(evt.ThreadID, info.SessionID); err != nil {
					log.Printf("triage: update session ref: %v", err)
				}
			}
		}
		return nil

	case provider.EventTurnComplete:
		// If text was accumulated during this turn, persist it as an assistant message item.
		if acc, ok := r.textAccumulators[evt.ThreadID]; ok && acc.Len() > 0 {
			content := acc.String()
			now := time.Now().UnixMilli()
			turnIndex, err := r.store.LastTurnIndex(evt.ThreadID)
			if err != nil {
				turnIndex = 0
			}
			itemIndex, err := r.store.NextItemIndex(evt.ThreadID, turnIndex)
			if err != nil {
				itemIndex = 0
			}
			item := store.Item{
				ID:        uuid.New().String(),
				ThreadID:  evt.ThreadID,
				TurnIndex: turnIndex,
				ItemIndex: itemIndex,
				Kind:      string(provider.ItemText),
				Role:      "assistant",
				Summary:   content,
				CreatedAt: now,
			}
			if err := r.store.InsertItem(item); err != nil {
				log.Printf("triage: persist assistant text: %v", err)
			}
			acc.Reset()
		}
		r.emit("provider:event", evt)
		return nil

	case provider.EventBackgroundDelta:
		// Accumulate in memory — do not emit per-delta.
		// The Session layer accumulates these; triage just drops them.
		return nil

	case provider.EventBackgroundComplete:
		// Persist the accumulated background output as a payload + item.
		return r.persistHeavy(evt, "full_text", string(provider.ItemBackgroundDone))

	// --- Heavy events: extract meta, persist, emit meta ---

	case provider.EventDiff:
		return r.persistHeavy(evt, "diff", string(provider.ItemDiff))

	case provider.EventCommandOutput:
		return r.persistHeavy(evt, "command_output", string(provider.ItemCommandExecution))

	case provider.EventThinking:
		return r.persistHeavy(evt, "thinking", string(provider.ItemThinking))

	default:
		log.Printf("triage: unhandled event kind: %s", evt.Kind)
		r.emit("provider:event", evt)
		return nil
	}
}

// persistHeavy extracts meta, stores payload + item, emits meta to frontend.
func (r *Router) persistHeavy(evt provider.ProviderEvent, payloadKind string, itemKind string) error {
	now := time.Now().UnixMilli()
	payloadID := uuid.New().String()
	itemID := evt.ItemID
	if itemID == "" {
		itemID = uuid.New().String()
	}

	// Extract meta based on payload kind.
	var metaJSON string
	switch payloadKind {
	case "diff":
		dm := ExtractDiffMeta(evt.Content)
		data, _ := json.Marshal(dm)
		metaJSON = string(data)
	case "command_output":
		cm := ExtractCommandOutputMeta(evt.Content, "", 0)
		// Try to get command and exit code from evt.Meta if available.
		if evt.Meta != nil {
			var parsed struct {
				Command  string `json:"command"`
				ExitCode int    `json:"exitCode"`
			}
			if json.Unmarshal(evt.Meta, &parsed) == nil {
				cm = ExtractCommandOutputMeta(evt.Content, parsed.Command, parsed.ExitCode)
			}
		}
		data, _ := json.Marshal(cm)
		metaJSON = string(data)
	case "thinking":
		tm := ExtractThinkingMeta(evt.Content)
		data, _ := json.Marshal(tm)
		metaJSON = string(data)
	default:
		metaJSON = "{}"
	}

	// Persist payload.
	payload := store.Payload{
		ID:        payloadID,
		Kind:      payloadKind,
		Meta:      metaJSON,
		Data:      []byte(evt.Content),
		CreatedAt: now,
	}
	if err := r.store.InsertPayload(payload); err != nil {
		log.Printf("triage: persist payload: %v", err)
		// Don't fail the stream on persistence error.
	}

	// Determine turn/item indices.
	turnIndex, err := r.store.LastTurnIndex(evt.ThreadID)
	if err != nil {
		turnIndex = 0
	}
	itemIndex, err := r.store.NextItemIndex(evt.ThreadID, turnIndex)
	if err != nil {
		itemIndex = 0
	}

	// Build summary from meta for always-loaded display.
	summary := buildSummary(payloadKind, metaJSON)

	// Persist item.
	item := store.Item{
		ID:        itemID,
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		ItemIndex: itemIndex,
		Kind:      itemKind,
		Role:      "assistant",
		Summary:   summary,
		PayloadID: payloadID,
		CreatedAt: now,
	}
	if err := r.store.InsertItem(item); err != nil {
		log.Printf("triage: persist item: %v", err)
	}

	// Emit meta to frontend (not the full content).
	r.emit("provider:meta", store.PayloadMeta{
		ID:        payloadID,
		Kind:      payloadKind,
		Meta:      metaJSON,
		CreatedAt: now,
	})

	return nil
}

// buildSummary creates a short human-readable summary from meta.
func buildSummary(payloadKind, metaJSON string) string {
	switch payloadKind {
	case "diff":
		var dm DiffMeta
		if json.Unmarshal([]byte(metaJSON), &dm) == nil {
			return fmt.Sprintf("%s: +%d/-%d %s", dm.ChangeKind, dm.Insertions, dm.Deletions, dm.FilePath)
		}
	case "command_output":
		var cm CommandOutputMeta
		if json.Unmarshal([]byte(metaJSON), &cm) == nil {
			return fmt.Sprintf("$ %s (exit %d, %d lines)", cm.Command, cm.ExitCode, cm.LineCount)
		}
	case "thinking":
		var tm ThinkingMeta
		if json.Unmarshal([]byte(metaJSON), &tm) == nil {
			return tm.Preview
		}
	}
	return ""
}
```

### File: `internal/triage/meta.go`

```go
package triage

import (
	"strings"
)

// DiffMeta is the JSON structure stored in payloads.meta for diffs.
type DiffMeta struct {
	FilePath   string `json:"filePath"`
	ChangeKind string `json:"changeKind"`
	Insertions int    `json:"insertions"`
	Deletions  int    `json:"deletions"`
	Preview    string `json:"preview"`
}

// CommandOutputMeta is the JSON structure for command output payloads.
type CommandOutputMeta struct {
	Command   string `json:"command"`
	ExitCode  int    `json:"exitCode"`
	LineCount int    `json:"lineCount"`
	Preview   string `json:"preview"`
}

// ThinkingMeta is the JSON structure for thinking block payloads.
type ThinkingMeta struct {
	TokenCount int    `json:"tokenCount"`
	Preview    string `json:"preview"`
}

// ExtractDiffMeta parses a unified diff string and returns structured meta.
func ExtractDiffMeta(patch string) DiffMeta {
	dm := DiffMeta{ChangeKind: "modified"}
	lines := strings.Split(patch, "\n")

	// Extract file path from diff header.
	for _, line := range lines {
		if strings.HasPrefix(line, "+++ b/") {
			dm.FilePath = strings.TrimPrefix(line, "+++ b/")
			break
		}
		if strings.HasPrefix(line, "+++ ") {
			dm.FilePath = strings.TrimPrefix(line, "+++ ")
			break
		}
	}

	// Count insertions/deletions (lines starting with + or - that aren't headers).
	inBody := false
	for _, line := range lines {
		if strings.HasPrefix(line, "@@") {
			inBody = true
			continue
		}
		if !inBody {
			continue
		}
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			dm.Insertions++
		} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			dm.Deletions++
		}
	}

	// Detect change kind.
	if dm.Deletions == 0 && dm.Insertions > 0 {
		// Check if diff header says new file.
		for _, line := range lines {
			if strings.HasPrefix(line, "new file") {
				dm.ChangeKind = "added"
				break
			}
		}
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "deleted file") {
			dm.ChangeKind = "deleted"
			break
		}
		if strings.HasPrefix(line, "rename from") {
			dm.ChangeKind = "renamed"
			break
		}
	}

	// Preview: first ~20 lines.
	previewLines := lines
	if len(previewLines) > 20 {
		previewLines = previewLines[:20]
	}
	dm.Preview = strings.Join(previewLines, "\n")

	return dm
}

// ExtractCommandOutputMeta extracts preview from command output.
func ExtractCommandOutputMeta(output string, command string, exitCode int) CommandOutputMeta {
	lines := strings.Split(output, "\n")
	cm := CommandOutputMeta{
		Command:   command,
		ExitCode:  exitCode,
		LineCount: len(lines),
	}

	// Preview: last ~10 lines.
	start := len(lines) - 10
	if start < 0 {
		start = 0
	}
	cm.Preview = strings.Join(lines[start:], "\n")

	return cm
}

// ExtractThinkingMeta extracts preview from a thinking block.
func ExtractThinkingMeta(content string) ThinkingMeta {
	tm := ThinkingMeta{
		// Rough token estimate: chars / 4.
		TokenCount: len(content) / 4,
	}

	// Preview: first ~200 chars.
	if len(content) > 200 {
		tm.Preview = content[:200] + "..."
	} else {
		tm.Preview = content
	}

	return tm
}
```

---

## 12. Wails Bindings (app.go)

All public methods on `App` are auto-exposed to the frontend via Wails bindings.

```go
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"log"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"

	"github.com/google/uuid"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// App is the primary Wails-bound service.
type App struct {
	app     *application.App
	store   *store.Store
	triage  *triage.Router
	mu      sync.Mutex
	sessions map[string]session // threadID → active session
}

// session wraps a provider session regardless of type.
type session struct {
	provider string
	// Exactly one of these is non-nil.
	claude *claude.Session
	codex  *codex.Session
}

func NewApp() *App {
	return &App{
		sessions: make(map[string]session),
	}
}

// ServiceStartup is called by Wails v3 when the service is initialized.
func (a *App) ServiceStartup(ctx context.Context, options application.ServiceOptions) error {
	a.app = application.Get()

	// Open SQLite database in the app data directory.
	dataDir, err := os.UserConfigDir()
	if err != nil {
		dataDir = os.TempDir()
	}
	dbDir := filepath.Join(dataDir, "agent-overflow")
	os.MkdirAll(dbDir, 0755)
	dbPath := filepath.Join(dbDir, "agent-overflow.db")

	st, err := store.New(dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}

	a.store = st
	a.triage = triage.NewRouter(st, func(eventName string, data any) {
		a.app.Event.Emit(eventName, data)
	})
	return nil
}

// ServiceShutdown is called by Wails v3 on app close.
func (a *App) ServiceShutdown() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.sessions {
		if s.claude != nil {
			s.claude.Close()
		}
		if s.codex != nil {
			s.codex.Close()
		}
	}
	if a.store != nil {
		a.store.Close()
	}
}

// --- Thread operations ---

func (a *App) CreateThread(providerName string, workspacePath string, model string) (store.Thread, error) {
	now := time.Now().UnixMilli()
	t := store.Thread{
		ID:            uuid.New().String(),
		Title:         "New Thread",
		Provider:      providerName,
		WorkspacePath: workspacePath,
		Model:         model,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := a.store.CreateThread(t); err != nil {
		return store.Thread{}, err
	}
	return t, nil
}

func (a *App) ListThreads() ([]store.Thread, error) {
	return a.store.ListThreads()
}

func (a *App) GetThread(id string) (store.Thread, error) {
	return a.store.GetThread(id)
}

func (a *App) DeleteThread(id string) error {
	a.StopSession(id) // stop provider if running
	return a.store.DeleteThread(id)
}

func (a *App) ArchiveThread(id string) error {
	return a.store.ArchiveThread(id)
}

func (a *App) RenameThread(id string, title string) error {
	t, err := a.store.GetThread(id)
	if err != nil {
		return err
	}
	t.Title = title
	t.UpdatedAt = time.Now().UnixMilli()
	return a.store.UpdateThread(t)
}

// --- Item operations ---

func (a *App) ListItems(threadID string) ([]store.Item, error) {
	return a.store.ListItems(threadID)
}

// --- Payload operations ---

func (a *App) GetPayloadData(payloadID string) (string, error) {
	data, err := a.store.GetPayloadData(payloadID)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (a *App) ListPayloadMetas(threadID string) ([]store.PayloadMeta, error) {
	return a.store.ListPayloadMetas(threadID)
}

// --- Session operations ---

func (a *App) StartSession(threadID string) error {
	t, err := a.store.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}

	onEvent := func(evt provider.ProviderEvent) {
		a.triage.Handle(evt)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	// Stop existing session if any.
	if existing, ok := a.sessions[threadID]; ok {
		if existing.claude != nil {
			existing.claude.Close()
		}
		if existing.codex != nil {
			existing.codex.Close()
		}
		delete(a.sessions, threadID)
	}

	switch t.Provider {
	case "claude":
		cfg := claude.Config{
			Model:   t.Model,
			WorkDir: t.WorkspacePath,
			Resume:  t.SessionRef,
		}
		sess, err := claude.NewSession(a.ctx, threadID, cfg, onEvent)
		if err != nil {
			return err
		}
		a.sessions[threadID] = session{provider: "claude", claude: sess}

	case "codex":
		cfg := codex.Config{
			Model:          t.Model,
			WorkDir:        t.WorkspacePath,
			ResumeThreadID: t.SessionRef,
		}
		sess, err := codex.NewSession(a.ctx, threadID, cfg, onEvent)
		if err != nil {
			return err
		}
		a.sessions[threadID] = session{provider: "codex", codex: sess}

	default:
		return fmt.Errorf("unknown provider: %s", t.Provider)
	}

	return nil
}

func (a *App) SendMessage(threadID string, content string) error {
	a.mu.Lock()
	sess, ok := a.sessions[threadID]
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("no active session for thread %s", threadID)
	}

	// Persist user message before sending to provider.
	turnIndex, _ := a.store.LastTurnIndex(threadID)
	turnIndex++ // new turn starts with user message
	itemIndex := 0
	now := time.Now().UnixMilli()
	userItem := store.Item{
		ID:        uuid.New().String(),
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		ItemIndex: itemIndex,
		Kind:      "text",
		Role:      "user",
		Summary:   content,
		CreatedAt: now,
	}
	if err := a.store.InsertItem(userItem); err != nil {
		log.Printf("failed to persist user message: %v", err)
	}

	switch {
	case sess.claude != nil:
		return sess.claude.Send(a.ctx, content)
	case sess.codex != nil:
		return sess.codex.Send(a.ctx, content)
	default:
		return fmt.Errorf("session has no provider")
	}
}

func (a *App) InterruptTurn(threadID string) error {
	a.mu.Lock()
	sess, ok := a.sessions[threadID]
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("no active session for thread %s", threadID)
	}

	switch {
	case sess.claude != nil:
		return sess.claude.Interrupt(a.ctx)
	case sess.codex != nil:
		// Codex interrupt needs a turn ID — omitted for simplicity in v1.
		return sess.codex.Interrupt(a.ctx, "")
	default:
		return fmt.Errorf("session has no provider")
	}
}

func (a *App) StopSession(threadID string) error {
	a.mu.Lock()
	sess, ok := a.sessions[threadID]
	if ok {
		delete(a.sessions, threadID)
	}
	a.mu.Unlock()

	if !ok {
		return nil // no-op
	}

	if sess.claude != nil {
		return sess.claude.Close()
	}
	if sess.codex != nil {
		return sess.codex.Close()
	}
	return nil
}

func (a *App) RespondToApproval(threadID string, requestID string, decision string) error {
	a.mu.Lock()
	sess, ok := a.sessions[threadID]
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("no active session for thread %s", threadID)
	}

	switch {
	case sess.claude != nil:
		return sess.claude.RespondToApproval(a.ctx, provider.ApprovalResponse{
			RequestID: requestID,
			Decision:  decision,
		})
	case sess.codex != nil:
		// Parse requestID as int64 for JSON-RPC response.
		var rpcID int64
		fmt.Sscanf(requestID, "%d", &rpcID)
		return sess.codex.RespondToApproval(a.ctx, rpcID, decision)
	default:
		return fmt.Errorf("session has no provider")
	}
}

// --- Settings (stub) ---

func (a *App) GetSettings() (map[string]any, error) {
	return map[string]any{}, nil
}
```

In `main.go`, register the App as a Wails v3 service:

```go
// In the application.New options, add:
Services: []application.Service{
	application.NewService(app),
},
```

### Wails event names

| Event name | Payload | Direction |
|---|---|---|
| `"provider:event"` | `provider.ProviderEvent` (JSON) | Go → Frontend |
| `"provider:meta"` | `store.PayloadMeta` (JSON) | Go → Frontend |
| `"provider:error"` | `provider.ProviderEvent` with `kind="error"` | Go → Frontend |

---

## 13. Frontend TypeScript Types

### File: `frontend/src/lib/types/events.ts`

```typescript
export type EventKind =
  | 'init'
  | 'text_delta'
  | 'tool_start'
  | 'tool_complete'
  | 'turn_start'
  | 'turn_complete'
  | 'approval_request'
  | 'approval_resolved'
  | 'session_status'
  | 'token_usage'
  | 'error'
  | 'background_start'
  | 'background_delta'
  | 'background_complete'
  | 'diff'
  | 'command_output'
  | 'thinking';

export interface ProviderEvent {
  kind: EventKind;
  threadId: string;
  turnId?: string;
  itemId?: string;
  itemType?: string;
  content?: string;
  role?: string;
  meta?: unknown;
  timestamp: string;
}

export interface ApprovalRequest {
  requestId: string;
  threadId: string;
  turnId: string;
  toolName: string;
  description: string;
  input: unknown;
  title: string;
}

export interface TokenUsage {
  inputTokens: number;
  outputTokens: number;
  cacheReadInputTokens?: number;
  cacheCreationInputTokens?: number;
  totalCostUsd?: number;
}
```

### File: `frontend/src/lib/types/models.ts`

```typescript
export interface Thread {
  id: string;
  title: string;
  provider: 'claude' | 'codex';
  sessionRef?: string;
  workspacePath: string;
  model: string;
  createdAt: number;
  updatedAt: number;
  archived: boolean;
}

export interface Item {
  id: string;
  threadId: string;
  turnIndex: number;
  itemIndex: number;
  kind: string;
  role: string;
  summary: string;
  payloadId?: string;
  createdAt: number;
}

export interface PayloadMeta {
  id: string;
  kind: string;
  meta: string; // JSON string — parse based on kind
  createdAt: number;
}

export interface DiffMeta {
  filePath: string;
  changeKind: 'added' | 'modified' | 'deleted' | 'renamed';
  insertions: number;
  deletions: number;
  preview: string;
}

export interface CommandOutputMeta {
  command: string;
  exitCode: number;
  lineCount: number;
  preview: string;
}

export interface ThinkingMeta {
  tokenCount: number;
  preview: string;
}
```

---

## 14. Frontend State Management

### Design: Per-Pane Thread State (tiling-ready)

Thread state is a **factory function**, not a module singleton. Each pane (currently one, future: multiple via tiling/splitting) gets its own `ThreadPane` instance with independent reactive state. This means components receive a `pane` prop instead of importing global getters, and the event router fans out to all active panes.

This enables future tiling (e.g., dockview-core) as a purely additive change — create a new pane per split, hand it to a ChatView. No refactoring of components or stores.

### File: `frontend/src/lib/stores/thread.svelte.ts`

Uses Svelte 5 runes. Per-pane instance, not module singleton.

```typescript
import type { Item, PayloadMeta, Thread } from '../types/models';
import type { ApprovalRequest, TokenUsage } from '../types/events';
import { ListItems, ListPayloadMetas } from '../../bindings/agent-overflow';

/**
 * Creates a self-contained thread pane state instance.
 * Each pane tracks its own thread, items, streaming state, approvals, etc.
 * Components receive a ThreadPane as a prop.
 */
export function createThreadPane() {
  let thread: Thread | null = $state(null);
  let items: Item[] = $state([]);
  let payloadMetas: Map<string, PayloadMeta> = $state(new Map());
  let streamingContent: string = $state('');
  let activeToolCalls: Map<string, unknown> = $state(new Map());
  let pendingApprovals: ApprovalRequest[] = $state([]);
  let backgroundTasks: Map<string, unknown> = $state(new Map());
  let sessionStatus: string = $state('disconnected');
  let tokenUsage: TokenUsage | null = $state(null);

  return {
    // --- Getters (reactive reads) ---
    get thread() { return thread; },
    get threadId() { return thread?.id ?? null; },
    get items() { return items; },
    get payloadMetas() { return payloadMetas; },
    get streamingContent() { return streamingContent; },
    get activeToolCalls() { return activeToolCalls; },
    get pendingApprovals() { return pendingApprovals; },
    get backgroundTasks() { return backgroundTasks; },
    get sessionStatus() { return sessionStatus; },
    get tokenUsage() { return tokenUsage; },

    // --- Thread switching ---

    async switchThread(newThread: Thread): Promise<void> {
      // Clear streaming state.
      streamingContent = '';
      activeToolCalls = new Map();
      pendingApprovals = [];
      backgroundTasks = new Map();
      tokenUsage = null;
      sessionStatus = 'disconnected';

      thread = newThread;

      // Load persisted items from SQLite.
      try {
        items = await ListItems(newThread.id);
      } catch (err) {
        console.error('Failed to load items:', err);
        items = [];
      }

      // Load payload metas for all items in this thread.
      try {
        const metas = await ListPayloadMetas(newThread.id);
        payloadMetas = new Map((metas ?? []).map((m: PayloadMeta) => [m.id, m]));
      } catch (err) {
        console.error('Failed to load payload metas:', err);
        payloadMetas = new Map();
      }
    },

    clear(): void {
      thread = null;
      items = [];
      payloadMetas = new Map();
      streamingContent = '';
      activeToolCalls = new Map();
      pendingApprovals = [];
      backgroundTasks = new Map();
      sessionStatus = 'disconnected';
      tokenUsage = null;
    },

    // --- Mutations (called by event router) ---

    appendTextDelta(delta: string): void {
      streamingContent += delta;
    },

    freezeStreamingContent(item: Item): void {
      items = [...items, item];
      streamingContent = '';
    },

    addToolCall(id: string, data: unknown): void {
      activeToolCalls = new Map(activeToolCalls).set(id, data);
    },

    completeToolCall(id: string, item: Item): void {
      const next = new Map(activeToolCalls);
      next.delete(id);
      activeToolCalls = next;
      items = [...items, item];
    },

    addApproval(approval: ApprovalRequest): void {
      pendingApprovals = [...pendingApprovals, approval];
    },

    removeApproval(requestId: string): void {
      pendingApprovals = pendingApprovals.filter((a) => a.requestId !== requestId);
    },

    addBackgroundTask(id: string, data: unknown): void {
      backgroundTasks = new Map(backgroundTasks).set(id, data);
    },

    completeBackgroundTask(id: string): void {
      const next = new Map(backgroundTasks);
      next.delete(id);
      backgroundTasks = next;
    },

    setSessionStatus(status: string): void {
      sessionStatus = status;
    },

    setTokenUsage(usage: TokenUsage): void {
      tokenUsage = usage;
    },

    addPayloadMeta(meta: PayloadMeta): void {
      payloadMetas = new Map(payloadMetas).set(meta.id, meta);
    },

    appendItem(item: Item): void {
      items = [...items, item];
    },
  };
}

export type ThreadPane = ReturnType<typeof createThreadPane>;
```

### File: `frontend/src/lib/stores/panes.svelte.ts`

Manages active panes. For v1, there's always exactly one pane. The structure supports future tiling.

```typescript
import { createThreadPane, type ThreadPane } from './thread.svelte';

// Active panes, keyed by pane ID. v1 has exactly one pane ("main").
let panes: Map<string, ThreadPane> = $state(new Map());

export function getPane(id: string): ThreadPane | undefined {
  return panes.get(id);
}

export function getMainPane(): ThreadPane {
  let main = panes.get('main');
  if (!main) {
    main = createThreadPane();
    panes = new Map(panes).set('main', main);
  }
  return main;
}

export function getAllPanes(): Map<string, ThreadPane> {
  return panes;
}

// Future: createPane, removePane, etc. for tiling support.
```

### File: `frontend/src/lib/stores/threads.svelte.ts`

```typescript
import type { Thread } from '../types/models';
import { ListThreads } from '../../bindings/agent-overflow';

let threads: Thread[] = $state([]);

export function getThreads(): Thread[] {
  return threads;
}

export async function refreshThreads(): Promise<void> {
  try {
    threads = await ListThreads();
  } catch (err) {
    console.error('Failed to load threads:', err);
  }
}

export function prependThread(thread: Thread): void {
  threads = [thread, ...threads];
}

export function removeThread(id: string): void {
  threads = threads.filter((t) => t.id !== id);
}

export function updateThreadInList(updated: Thread): void {
  threads = threads.map((t) => (t.id === updated.id ? updated : t));
}
```

### File: `frontend/src/lib/stores/events.ts`

The event router fans out to all active panes showing the relevant thread. This supports future tiling where multiple panes may show different (or the same) thread.

```typescript
import { Events } from '@wailsio/runtime';
import type { ProviderEvent, ApprovalRequest, TokenUsage } from '../types/events';
import type { PayloadMeta } from '../types/models';
import type { ThreadPane } from './thread.svelte';
import { getAllPanes } from './panes.svelte';

/**
 * Route a provider event to the correct pane mutation.
 * Called once per pane that matches the event's threadId.
 */
function routeEventToPane(pane: ThreadPane, evt: ProviderEvent): void {
  switch (evt.kind) {
    case 'text_delta':
      pane.appendTextDelta(evt.content ?? '');
      break;

    case 'tool_start':
      pane.addToolCall(evt.itemId ?? '', evt.meta);
      break;

    case 'tool_complete':
      pane.completeToolCall(evt.itemId ?? '', {
        id: evt.itemId ?? '',
        threadId: evt.threadId,
        turnIndex: 0,
        itemIndex: 0,
        kind: evt.itemType ?? 'tool_result',
        role: 'assistant',
        summary: evt.content ?? '',
        createdAt: Date.now(),
      });
      break;

    case 'turn_start':
      pane.setSessionStatus('running');
      break;

    case 'turn_complete':
      pane.setSessionStatus('ready');
      break;

    case 'approval_request':
      if (evt.meta) {
        pane.addApproval(evt.meta as ApprovalRequest);
      }
      break;

    case 'approval_resolved':
      if (evt.itemId) {
        pane.removeApproval(evt.itemId);
      }
      break;

    case 'session_status':
      pane.setSessionStatus(evt.content ?? 'unknown');
      break;

    case 'token_usage':
      if (evt.meta) {
        pane.setTokenUsage(evt.meta as TokenUsage);
      }
      break;

    case 'error':
      console.error('Provider error:', evt.content);
      pane.setSessionStatus('error');
      break;

    case 'init':
      pane.setSessionStatus('connected');
      break;

    case 'background_start':
      pane.addBackgroundTask(evt.itemId ?? '', evt.meta);
      break;

    case 'background_complete':
      pane.completeBackgroundTask(evt.itemId ?? '');
      break;
  }
}

export function setupEventListeners(): () => void {
  const cancelEvent = Events.On('provider:event', (ev) => {
    const evt = ev.data as ProviderEvent;
    // Fan out to all panes showing this thread.
    for (const pane of getAllPanes().values()) {
      if (pane.threadId === evt.threadId) {
        routeEventToPane(pane, evt);
      }
    }
  });

  const cancelMeta = Events.On('provider:meta', (ev) => {
    const meta = ev.data as PayloadMeta;
    for (const pane of getAllPanes().values()) {
      if (meta.threadId && pane.threadId !== meta.threadId) continue;
      pane.addPayloadMeta(meta);
    }
  });

  const cancelError = Events.On('provider:error', (ev) => {
    const evt = ev.data as ProviderEvent;
    console.error('Provider error event:', evt.content);
  });

  return () => { cancelEvent(); cancelMeta(); cancelError(); };
}
```

### File: `frontend/src/lib/stores/bindings.ts`

Typed wrappers around the auto-generated Wails Go bindings.

```typescript
// Re-export Wails-generated bindings with explicit types for convenience.
// The actual generated bindings live in bindings/agent-overflow/.
export {
  CreateThread,
  ListThreads,
  GetThread,
  DeleteThread,
  ArchiveThread,
  RenameThread,
  ListItems,
  GetPayloadData,
  ListPayloadMetas,
  StartSession,
  SendMessage,
  InterruptTurn,
  StopSession,
  RespondToApproval,
  GetSettings,
} from '../../bindings/agent-overflow';
```

---

## 15. Error Handling

All errors use Go's standard `error` interface. No custom error types for v1.

### Provider process errors

When the provider process crashes or exits unexpectedly:
1. The `readLoop` goroutine detects `io.EOF` on stdout.
2. It emits `EventSessionStatus` with content `"disconnected"`.
3. If the exit was abnormal, it also emits `EventError` with the error message.
4. The `session` entry in `App.sessions` remains until explicitly stopped or the thread is re-started.
5. Frontend shows the error and offers "Restart session" which calls `StartSession` again.

### SQLite errors

Wrapped with context: `fmt.Errorf("store: <operation>: %w", err)`. The triage layer logs persistence failures but does not crash — streaming continues. This means if SQLite is broken, the user still sees real-time output but history won't persist.

### Wails binding errors

Returned as Go `error` which Wails serializes as a string to the frontend. The frontend shows these in a toast or inline error via the catch block on the binding call.

### Startup errors

If the database fails to open, `App.ServiceStartup` returns an error. Wails v3 surfaces this as a startup failure, crashing the app intentionally — the database is required.

---

## 16. Testing Strategy

### Store tests (`internal/store/store_test.go`)

Use in-memory SQLite (`:memory:`) for all tests. Covers:
- `CreateThread` / `GetThread` / `ListThreads` — basic CRUD
- `DeleteThread` — verify CASCADE deletes items
- `ArchiveThread` — verify filtered from `ListThreads`
- `UpdateSessionRef` — verify ref update + updated_at bump
- `InsertItem` / `ListItems` — ordering by turn_index, item_index
- `NextItemIndex` / `LastTurnIndex` — edge cases: empty thread, multiple turns
- `InsertPayload` / `GetPayloadMeta` / `GetPayloadData` — round-trip verification
- `ListPayloadMetas` — verify JOIN with items, correct thread scoping

### Protocol tests

**Claude** (`internal/provider/claude/claude_test.go`):
- Test `ParseLine` with fixture JSON strings for each message type:
  - `system/init` → `EventInit` with SessionInfo
  - `assistant` with text + tool_use + thinking blocks
  - `result` (success and error variants)
  - `control_request` with `can_use_tool`
  - `stream_event` with text delta
  - Unknown type → no error, empty events

**Codex** (`internal/provider/codex/codex_test.go`):
- Test `ClassifyNotification` with fixture params for each handled method:
  - `turn/started` → `EventTurnStart` with turn ID
  - `turn/completed` (success and failed) → `EventTurnComplete` / `EventError`
  - `item/agentMessage/delta` → `EventTextDelta`
  - `item/started` / `item/completed` → `EventToolStart` / `EventToolComplete`
  - `thread/tokenUsage/updated` → `EventTokenUsage`
  - `error` → `EventError`
  - Skipped methods (e.g., `thread/started`) → empty events
  - Unknown method → empty events

### Triage tests (`internal/triage/triage_test.go`)

Test `Router.Handle` with constructed `ProviderEvent` values:
- Inline events (e.g., `EventTextDelta`) → verify `emit` was called, store was NOT called
- Heavy events (e.g., `EventDiff`) → verify payload was inserted into store, meta was emitted
- `EventInit` → verify `UpdateSessionRef` was called

### Meta extraction tests (in `triage_test.go` or `meta_test.go`)

- `ExtractDiffMeta` with real unified diff strings: verify file path, change kind, +/- counts, preview truncation
- `ExtractCommandOutputMeta` with varying output sizes: verify line count, preview is last 10 lines
- `ExtractThinkingMeta` with short and long content: verify preview truncation at 200 chars

### Frontend

Manual testing via `wails dev` for v1. No automated frontend tests.

### Quality gate

```sh
go build ./... && go vet ./... && go test -coverprofile=coverage.out ./... -count=1
```

Target: 80% coverage floor on `internal/` packages.

---

## 17. v1 Scope Boundaries

### In scope (v1)

- Claude Code CLI and Codex app-server provider support
- Thread CRUD: create, list, rename, archive, delete
- Message history persisted to SQLite, loaded on thread switch
- Session resume via `session_ref` (Claude session ID / Codex thread ID)
- Streaming text rendering with mutable head
- Tool call display (start → complete lifecycle)
- Diff previews (inline, using meta summary + on-demand full load)
- Command output previews (inline, last 10 lines + on-demand full)
- Thinking block storage + preview
- Approval/permission flow (request → user decision → response to provider)
- Background task tray (start → accumulated output → complete)
- SQLite persistence with on-demand payload loading
- Sidebar with thread list (ordered by updated_at)
- Model selection per thread
- Token usage display (status bar)
- Graceful process shutdown (stdin close → SIGTERM → SIGKILL)

### Not in scope (v2)

- Multiple simultaneous provider sessions per thread
- Image attachments or file mentions (@-mention)
- Thread forking
- Full diff panel (side-by-side view) — inline preview only for v1
- Terminal integration (xterm.js / PTY)
- Settings panel (v1 uses hardcoded defaults)
- Keyboard shortcuts
- Search across threads
- Desktop notifications
- Auto-update
- MCP server management
- OAuth implementation (rely on CLI's existing auth)
- Collaboration mode (Codex plan mode)
- Child thread / subagent visualization
