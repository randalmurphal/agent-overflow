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

// runMigrations is defined in migrate.go.

// Thread represents a conversation thread.
//
// The shape changed at migration v13: ProjectPath is gone (replaced by the
// ProjectID FK to the projects table), InteractionMode is renamed to Mode,
// and three new per-thread composer controls (ReasoningEffort, FastMode,
// ContextWindow) are persisted so two threads sharing a project can
// diverge on these axes.
type Thread struct {
	ID                 string `json:"id"`
	ProjectID          string `json:"projectId"`
	Title              string `json:"title"`
	Provider           string `json:"provider"`
	Model              string `json:"model"`
	WorkspacePath      string `json:"workspacePath"`
	WorktreePath       string `json:"worktreePath,omitempty"`
	Branch             string `json:"branch,omitempty"`
	SessionRef         string `json:"sessionRef,omitempty"`
	PendingForkRef     string `json:"pendingForkRef,omitempty"`
	Mode               string `json:"mode"`
	ReasoningEffort    string `json:"reasoningEffort"`
	FastMode           bool   `json:"fastMode"`
	ContextWindow      int    `json:"contextWindow"`
	// RuntimeMode is one of provider.RuntimeMode's three values. Kept as
	// a plain string on the struct so store/ doesn't import provider/
	// (which would create a cycle) — provider.NormalizeRuntimeMode is the
	// authoritative normalizer at the binding boundary.
	RuntimeMode        string `json:"runtimeMode"`
	DiscussionID       string `json:"discussionId,omitempty"`
	ParentThreadID     string `json:"parentThreadId,omitempty"`
	ForkedFromThreadID string `json:"forkedFromThreadId,omitempty"`
	LastTokenUsage     string `json:"lastTokenUsage,omitempty"`
	CreatedAt          int64  `json:"createdAt"`
	UpdatedAt          int64  `json:"updatedAt"`
	Archived           bool   `json:"archived"`
}

// Project represents a user-defined grouping of threads rooted at a
// directory. Threads belong to a project via the project_id FK; the
// project's path is the canonical workspace root, though individual
// threads may operate in a worktree that diverges from project.path.
type Project struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	Name         string `json:"name"`
	Color        string `json:"color,omitempty"`
	SortPosition int    `json:"sortPosition"`
	CreatedAt    int64  `json:"createdAt"`
	UpdatedAt    int64  `json:"updatedAt"`
	Archived     bool   `json:"archived"`
}

// Item represents a persisted timeline entry.
type Item struct {
	ID           string `json:"id"`
	ThreadID     string `json:"threadId"`
	TurnIndex    int    `json:"turnIndex"`
	ItemIndex    int    `json:"itemIndex"`
	Kind         string `json:"kind"`
	Role         string `json:"role"`
	Status       string `json:"status"`
	Summary      string `json:"summary"`
	PayloadID    string `json:"payloadId,omitempty"`
	PayloadKind  string `json:"payloadKind,omitempty"`
	PayloadMeta  string `json:"payloadMeta,omitempty"`
	ParentID     string `json:"parentId,omitempty"`
	IsBackground bool   `json:"isBackground,omitempty"`
	CompletionOf string `json:"completionOf,omitempty"`
	ToolName     string `json:"toolName,omitempty"`
	Decision     string `json:"decision,omitempty"`
	Meta         string `json:"meta,omitempty"`
	CreatedAt    int64  `json:"createdAt"`
	UpdatedAt    int64  `json:"updatedAt"`
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
