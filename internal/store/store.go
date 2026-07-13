// Package store manages SQLite persistence for threads, items, and heavy payloads.
package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // SQLite driver
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
	db.SetMaxOpenConns(1)

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

// PassiveCheckpoint triggers a non-blocking WAL checkpoint. PASSIVE
// returns immediately without waiting for readers; any pages it can't
// reclaim stay in the WAL and the next call catches up. Safe to call
// from any goroutine. Returns the underlying error from SQLite —
// callers typically log and continue (the checkpoint is opportunistic;
// the autocheckpoint and the next idle-boundary call will retry).
//
// Why we need this on top of wal_autocheckpoint: the default autocheckpoint
// fires when the WAL crosses ~1000 pages (~4MB), but it runs synchronously
// on the next write transaction and bails when any reader transaction is
// open. In a streaming workload the writer is continuously busy and
// readers (the dashboard + active thread paging) overlap with bursts —
// the autocheckpoint window rarely opens. Calling PassiveCheckpoint
// from turn-completion (when streaming is known to be idle for the
// thread) gives the WAL a deterministic opportunity to recycle.
func (s *Store) PassiveCheckpoint() error {
	_, err := s.db.Exec("PRAGMA wal_checkpoint(PASSIVE)")
	return err
}

// runMigrations is defined in migrate.go.

// Thread represents a conversation thread.
//
// The shape changed at migration v13: ProjectPath is no longer persisted on
// threads (ProjectID is the FK to the projects table), InteractionMode is
// renamed to Mode, and three new per-thread composer controls
// (ReasoningEffort, FastMode, ContextWindow) are persisted so two threads
// sharing a project can diverge on these axes.
type Thread struct {
	ID                         string `json:"id"`
	ProjectID                  string `json:"projectId"`
	ProjectPath                string `json:"projectPath"`
	Title                      string `json:"title"`
	Provider                   string `json:"provider"`
	Model                      string `json:"model"`
	WorkspacePath              string `json:"workspacePath"`
	WorktreePath               string `json:"worktreePath,omitempty"`
	Branch                     string `json:"branch,omitempty"`
	PRRef                      string `json:"prRef,omitempty"`
	SessionRef                 string `json:"sessionRef,omitempty"`
	PendingForkRef             string `json:"pendingForkRef,omitempty"`
	Mode                       string `json:"mode"`
	ReasoningEffort            string `json:"reasoningEffort"`
	FastMode                   bool   `json:"fastMode"`
	ContextWindow              int    `json:"contextWindow"`
	AutoCompactStandardPercent int    `json:"autoCompactStandardPercent"`
	AutoCompactExtendedPercent int    `json:"autoCompactExtendedPercent"`
	// RuntimeMode is one of provider.RuntimeMode's three values. Kept as
	// a plain string on the struct so store/ doesn't import provider/
	// (which would create a cycle) — provider.NormalizeRuntimeMode is the
	// authoritative normalizer at the binding boundary.
	RuntimeMode string `json:"runtimeMode"`
	// DisabledMcpServers is the per-thread MCP disabled set. nil = not
	// yet snapshotted (pre-feature thread, lazy-snapshot on first access).
	// Non-nil empty slice = snapshotted with all servers enabled.
	DisabledMcpServers *[]string `json:"disabledMcpServers,omitempty"`
	DiscussionID       string    `json:"discussionId,omitempty"`
	ParentThreadID     string    `json:"parentThreadId,omitempty"`
	ForkedFromThreadID string    `json:"forkedFromThreadId,omitempty"`
	LastTokenUsage     string    `json:"lastTokenUsage,omitempty"`
	CreatedAt          int64     `json:"createdAt"`
	UpdatedAt          int64     `json:"updatedAt"`
	// LatestTurnCompletedAt is the newest completed turn timestamp for
	// sidebar read-state. Unlike UpdatedAt, it ignores metadata-only changes
	// such as session refs, title/model changes, and settings edits.
	LatestTurnCompletedAt *int64 `json:"latestTurnCompletedAt,omitempty"`
	Archived              bool   `json:"archived"`
	// LastReadAt is the Unix-ms timestamp of when the user last viewed
	// the thread. New rows are seeded with a creation-time baseline so a
	// later completion can be detected as unread even if the user switched
	// away before the first turn settled. NULL (nil) means "never tracked"
	// and is treated as read by the UI so pre-migration rows don't all show
	// as unread on first launch. Set by MarkThreadRead, stamped to zero by
	// MarkThreadUnread, and auto-refreshed when the user switches into a
	// thread.
	LastReadAt *int64 `json:"lastReadAt,omitempty"`
	// PinnedAt is the Unix-ms timestamp of when the user pinned the
	// thread. NULL (nil) = unpinned. Pinned threads sort into a
	// dedicated tier above needs-attention in the sidebar. Set by
	// PinThread; cleared by UnpinThread.
	PinnedAt *int64 `json:"pinnedAt,omitempty"`
	// HasActionableProposedPlan is derived for sidebar boot state. It is
	// true when the latest assistant proposed plan is completed and has
	// not been implemented yet. It is not a persisted threads column.
	HasActionableProposedPlan bool `json:"hasActionableProposedPlan"`
	// HasIncompleteTurn is derived from the newest unseen turn row: an
	// in-flight turn (completed_at=NULL) whose start the user hasn't
	// seen, or a settled stop_reason='interrupted' turn whose end the
	// user hasn't seen (boot-swept crashes land here —
	// RecoverCrashedTurns settles NULL rows as interrupted before the
	// frontend ever loads). Either way the sidebar should show
	// Interrupted, not live Working. It is not a persisted threads
	// column.
	HasIncompleteTurn bool `json:"hasIncompleteTurn"`
	// IsDraft is true when no items have been persisted for the thread.
	// Used by the sidebar to render a draft indicator and by the project
	// sort projection to exclude drafts from "last activity" so creating
	// or configuring an unsent thread does not move the project to the
	// top. It is not a persisted threads column.
	IsDraft bool `json:"isDraft"`
}

type ThreadContextSettings struct {
	Provider                   string
	Model                      string
	ProjectID                  string
	ContextWindow              int
	AutoCompactStandardPercent int
	AutoCompactExtendedPercent int
}

// ThreadWorkspaceRef is the narrow thread shape needed for workspace/worktree
// ownership checks. It deliberately includes archived threads because archived
// rows can be restored later and must not point at a removed worktree.
type ThreadWorkspaceRef struct {
	ID            string
	WorkspacePath string
	WorktreePath  string
}

// Project represents a user-defined grouping of threads rooted at a
// directory. Threads belong to a project via the project_id FK; the
// project's path is the canonical workspace root, though individual
// threads may operate in a worktree that diverges from project.path.
type Project struct {
	ID                  string `json:"id"`
	Path                string `json:"path"`
	Name                string `json:"name"`
	Slug                string `json:"slug"`
	Color               string `json:"color,omitempty"`
	SortPosition        int    `json:"sortPosition"`
	WorkflowQueuePaused bool   `json:"workflowQueuePaused"`
	WorkflowConcurrency int    `json:"workflowConcurrency"`
	CreatedAt           int64  `json:"createdAt"`
	UpdatedAt           int64  `json:"updatedAt"`
	Archived            bool   `json:"archived"`
}

// Item represents a persisted timeline entry.
type Item struct {
	ID             string `json:"id"`
	ThreadID       string `json:"threadId"`
	TurnIndex      int    `json:"turnIndex"`
	ItemIndex      int    `json:"itemIndex"`
	Kind           string `json:"kind"`
	Role           string `json:"role"`
	Status         string `json:"status"`
	Summary        string `json:"summary"`
	PayloadID      string `json:"payloadId,omitempty"`
	PayloadKind    string `json:"payloadKind,omitempty"`
	PayloadMeta    string `json:"payloadMeta,omitempty"`
	InputPayloadID string `json:"inputPayloadId,omitempty"`
	ParentID       string `json:"parentId,omitempty"`
	IsBackground   bool   `json:"isBackground,omitempty"`
	CompletionOf   string `json:"completionOf,omitempty"`
	ToolName       string `json:"toolName,omitempty"`
	Decision       string `json:"decision,omitempty"`
	Meta           string `json:"meta,omitempty"`
	CreatedAt      int64  `json:"createdAt"`
	UpdatedAt      int64  `json:"updatedAt"`
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

// ChatBarFavorite is one starred entry in the composer model menu.
// Kind is "model" or "discussion". Provider is set only for model
// favorites; Value is the model slug or discussion definition id.
type ChatBarFavorite struct {
	Kind      string `json:"kind"`
	Provider  string `json:"provider,omitempty"`
	Value     string `json:"value"`
	Label     string `json:"label"`
	CreatedAt int64  `json:"createdAt"`
}

// ChatModelProfile remembers the last selected chat-bar settings for a
// provider/model pair. New draft threads seed from the most recently
// updated profile, and model switches restore that model's profile.
type ChatModelProfile struct {
	Provider                   string `json:"provider"`
	Model                      string `json:"model"`
	ReasoningEffort            string `json:"reasoningEffort"`
	FastMode                   bool   `json:"fastMode"`
	ContextWindow              int    `json:"contextWindow"`
	AutoCompactStandardPercent int    `json:"autoCompactStandardPercent"`
	AutoCompactExtendedPercent int    `json:"autoCompactExtendedPercent"`
	RuntimeMode                string `json:"runtimeMode"`
	UpdatedAt                  int64  `json:"updatedAt"`
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
