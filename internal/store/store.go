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
type Thread struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Provider        string `json:"provider"`
	SessionRef      string `json:"sessionRef,omitempty"`
	WorkspacePath   string `json:"workspacePath"`
	Model           string `json:"model"`
	ProjectPath     string `json:"projectPath"`
	WorktreePath    string `json:"worktreePath,omitempty"`
	Branch          string `json:"branch,omitempty"`
	InteractionMode string `json:"interactionMode"`
	DiscussionID    string `json:"discussionId,omitempty"`
	ParentThreadID  string `json:"parentThreadId,omitempty"`
	CreatedAt       int64  `json:"createdAt"`
	UpdatedAt       int64  `json:"updatedAt"`
	Archived        bool   `json:"archived"`
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
