package rollout

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/importir"

	_ "modernc.org/sqlite" // SQLite driver (pure Go, no cgo)
)

// StateDBName is the Codex thread index Agent Overflow reads. The trailing
// number is Codex's schema generation: a bump renames the file, which is why
// a missing file is reported as a Codex-level error rather than "no sessions"
// — see List.
const StateDBName = "state_5.sqlite"

// ListOptions configures List.
type ListOptions struct {
	// CodexHome is the directory holding StateDBName (normally ~/.codex).
	// REQUIRED and injected by the caller: this package never consults
	// os.UserHomeDir or CODEX_HOME, because the app resolves the provider
	// home once (honouring its own credential-home override) and a second
	// answer here would list sessions the app cannot resume.
	CodexHome string
}

// SessionInfo is one importable Codex session, straight off the thread index.
//
// RolloutPath is always inside the ListOptions.CodexHome it was read under —
// see PathInHome for why that is checked rather than trusted.
//
// Model and ReasoningEffort are index-level fallbacks only. Codex leaves them
// NULL for some sources, while the authoritative per-turn values live in the
// rollout's `turn_context` lines (see Parse). Carrying the values through the
// scan is free and lets an import whose rollout lacks turn_context still make
// the best provider-recorded choice without a second database query.
type SessionInfo struct {
	ThreadID         string
	RolloutPath      string
	Cwd              string
	Title            string
	FirstUserMessage string
	GitBranch        string
	CreatedAt        int64 // epoch ms
	LastActivityAt   int64 // epoch ms
	SizeBytes        int64
	Model            string
	ReasoningEffort  string
}

// listQuery selects the threads a user would recognise as their own sessions.
//
// Three of the predicates are upstream's own and one is ours:
//
//   - `archived = 0` and a non-empty `preview` are exactly what Codex's own
//     thread list applies (codex-rs/state/src/runtime/threads.rs,
//     push_thread_filters).
//     `preview` is the emptiness signal; the legacy `has_user_event` column is
//     NOT — nothing has written it since migration 0007, so filtering on it
//     would return zero rows.
//   - the child-thread exclusions are ours, and deliberately belt-and-braces:
//     `thread_source` marks both ordinary subagents and 0.150 Guardian review
//     sessions, the `source` JSON prefix is what
//     older rows carry, and `thread_spawn_edges` is the authoritative child
//     table. A spawned child is a thread AO must not import as a top-level
//     conversation; it is imported (if at all) as part of its parent.
//
// Ordering matches Codex's default recency sort so the two lists agree.
const listQuery = `
SELECT t.id,
       t.rollout_path,
       t.cwd,
       t.title,
       t.first_user_message,
       t.git_branch,
       t.created_at,
       t.updated_at,
       t.created_at_ms,
       t.updated_at_ms,
       t.model,
       t.reasoning_effort
  FROM threads t
 WHERE t.archived = 0
   AND t.preview <> ''
   AND COALESCE(t.thread_source, '') NOT IN ('subagent', 'guardian_review')
   AND t.source NOT LIKE '{"subagent"%'
   AND t.id NOT IN (SELECT child_thread_id FROM thread_spawn_edges)
 ORDER BY COALESCE(t.recency_at_ms, t.updated_at_ms, t.updated_at * 1000) DESC,
          t.id DESC`

// List reads the Codex thread index and returns every session AO could import.
//
// Failure is loud and total: an absent, unreadable, locked, or schema-moved
// database returns an error and no rows, so the app can surface "Codex
// sessions are unavailable" instead of silently showing an empty list that
// looks like "you have no Codex sessions". There is deliberately no fallback
// walk of the sessions directory — a directory walk cannot answer archived,
// subagent, or preview, so it would produce a DIFFERENT list rather than a
// degraded one.
//
// Warnings are per-row and never fatal: a thread whose rollout file has been
// deleted is dropped from the result with a warning naming it.
func List(ctx context.Context, opts ListOptions) ([]SessionInfo, []importir.Warning, error) {
	home := strings.TrimSpace(opts.CodexHome)
	if home == "" {
		return nil, nil, errors.New("rollout: CodexHome is required")
	}
	dbPath := filepath.Join(home, StateDBName)
	if _, err := os.Stat(dbPath); err != nil {
		return nil, nil, fmt.Errorf("rollout: open codex thread index %s: %w", dbPath, err)
	}

	db, err := sql.Open("sqlite", readOnlyDSN(dbPath))
	if err != nil {
		return nil, nil, fmt.Errorf("rollout: open codex thread index %s: %w", dbPath, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	rows, err := db.QueryContext(ctx, listQuery)
	if err != nil {
		return nil, nil, fmt.Errorf("rollout: query codex thread index %s (schema may have moved): %w", dbPath, err)
	}
	defer rows.Close()

	var (
		sessions []SessionInfo
		warnings []importir.Warning
	)
	for rows.Next() {
		info, err := scanSessionRow(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("rollout: read codex thread index %s (schema may have moved): %w", dbPath, err)
		}
		if info.RolloutPath == "" {
			warnings = append(warnings, importir.Warning{
				Code:    WarnRolloutMissing,
				Message: fmt.Sprintf("Codex session %s has no rollout file recorded and cannot be imported.", info.ThreadID),
			})
			continue
		}
		// Containment BEFORE the stat: `rollout_path` is a path out of a
		// database AO does not own, so a row naming a file outside the home
		// must not even have its existence probed. See PathInHome.
		contained, pathErr := PathInHome(home, info.RolloutPath)
		if pathErr != nil {
			warnings = append(warnings, importir.Warning{
				Code: WarnRolloutOutside,
				Message: fmt.Sprintf(
					"Codex session %s records a session file outside %s and was skipped.",
					info.ThreadID, home),
			})
			continue
		}
		info.RolloutPath = contained
		stat, statErr := os.Stat(info.RolloutPath)
		if statErr != nil {
			warnings = append(warnings, importir.Warning{
				Code:    WarnRolloutMissing,
				Message: fmt.Sprintf("Codex session %s references a rollout file that no longer exists (%s).", info.ThreadID, info.RolloutPath),
			})
			continue
		}
		info.SizeBytes = stat.Size()
		sessions = append(sessions, info)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("rollout: read codex thread index %s: %w", dbPath, err)
	}
	return sessions, warnings, nil
}

// readOnlyDSN builds the SQLite URI List opens the thread index with.
//
// `immutable=1` is what makes this safe against a running Codex: SQLite skips
// the -wal/-shm files entirely, so AO creates no lock files, takes no locks,
// and cannot be blocked by (or block) the live process. The cost is that the
// snapshot is the last checkpointed state of the main database file — a
// session written seconds ago may not appear until Codex checkpoints. That is
// the right trade for a list the user can refresh; a plain `mode=ro` open
// would need a writable -shm to read the WAL and fails outright when Codex has
// not left one behind.
//
// The path is %-escaped for URI parsing: '%', '?' and '#' are the only
// characters that can cut the path short or corrupt the query string.
func readOnlyDSN(dbPath string) string {
	escaped := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(dbPath)
	return "file:" + escaped + "?mode=ro&immutable=1"
}

func scanSessionRow(rows *sql.Rows) (SessionInfo, error) {
	var (
		info                     SessionInfo
		cwd, title, firstMessage sql.NullString
		branch, rolloutPath      sql.NullString
		model, reasoningEffort   sql.NullString
		createdSec, updatedSec   sql.NullInt64
		createdMS, updatedMS     sql.NullInt64
	)
	if err := rows.Scan(
		&info.ThreadID,
		&rolloutPath,
		&cwd,
		&title,
		&firstMessage,
		&branch,
		&createdSec,
		&updatedSec,
		&createdMS,
		&updatedMS,
		&model,
		&reasoningEffort,
	); err != nil {
		return SessionInfo{}, err
	}
	info.RolloutPath = strings.TrimSpace(rolloutPath.String)
	info.Cwd = cwd.String
	info.Title = strings.TrimSpace(title.String)
	info.FirstUserMessage = strings.TrimSpace(firstMessage.String)
	info.GitBranch = strings.TrimSpace(branch.String)
	info.CreatedAt = millis(createdMS, createdSec)
	info.LastActivityAt = millis(updatedMS, updatedSec)
	info.Model = strings.TrimSpace(model.String)
	info.ReasoningEffort = strings.TrimSpace(reasoningEffort.String)
	if info.Title == "" {
		info.Title = info.FirstUserMessage
	}
	return info, nil
}

// millis prefers the millisecond column and falls back to the seconds column
// Codex's triggers derive it from. Both are nullable in practice on rows
// written before the ms columns landed.
func millis(ms, seconds sql.NullInt64) int64 {
	if ms.Valid && ms.Int64 > 0 {
		return ms.Int64
	}
	if seconds.Valid && seconds.Int64 > 0 {
		return seconds.Int64 * 1000
	}
	return 0
}
