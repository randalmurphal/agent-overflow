// Package store manages SQLite persistence for threads, items, and heavy payloads.
package store

import (
	"database/sql"
	"fmt"
	"time"

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
