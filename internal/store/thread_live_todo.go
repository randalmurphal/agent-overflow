package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ThreadLiveTodo is the durable copy of a thread's activity-rail todo list
// (migration v65). Producers are the provider's own reports — Claude
// TodoWrite / the Task* family, Codex update_plan — normalised to one step
// shape before they reach this package.
//
// It is written whole and read whole: a report restates the entire list, so
// there is nothing here to merge.
type ThreadLiveTodo struct {
	Steps []ThreadLiveTodoStep `json:"steps"`
	// UpdatedAt is the epoch-ms timestamp of the provider event that produced
	// this list, not the moment of the write. Readers age the list against it
	// (an all-completed list stops being worth showing), so it has to describe
	// the report rather than the persistence.
	UpdatedAt int64 `json:"updatedAt"`
}

// ThreadLiveTodoStep is one entry of the list. Status uses the camelCase
// vocabulary both providers are normalised into upstream (`pending` |
// `inProgress` | `completed`); ID and Owner are populated only by the Claude
// Task* path and are empty for every other producer.
type ThreadLiveTodoStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
	ID     string `json:"id,omitempty"`
	Owner  string `json:"owner,omitempty"`
}

// ErrEmptyThreadLiveTodo is returned by SetThreadLiveTodo for a list with no
// steps. An empty list IS a clear, and ClearThreadLiveTodo is the one way to
// express it — otherwise "cleared" would have two representations (an empty
// column and `{"steps":[]}`) and every reader would have to agree about both.
var ErrEmptyThreadLiveTodo = errors.New("store: empty thread live todo")

// maxThreadLiveTodoBytes bounds the encoded blob. The triage producers bound
// step count (maxTodoSteps on BOTH the TodoWrite decode and the Task*
// projection) and every per-field rune count, Status included; the worst
// bounded list — 256 steps of maximal 4-byte-rune fields with JSON escaping —
// stays under this with real margin. The cap exists so the row's size is a
// property of the ACCESSOR rather than of every caller's discipline: the
// column rides the thread row, and an unbounded write would tax every future
// read of that row. Refused loudly, never truncated — a list the writer
// cannot store whole is a caller bug to surface, not data to quietly lose
// the tail of.
const maxThreadLiveTodoBytes = 1 << 20

// ErrThreadLiveTodoTooLarge is returned by SetThreadLiveTodo when the encoded
// list exceeds maxThreadLiveTodoBytes.
var ErrThreadLiveTodoTooLarge = errors.New("store: thread live todo exceeds size bound")

// SetThreadLiveTodo replaces the thread's stored todo list.
//
// It deliberately leaves updated_at alone: a todo tick is the provider
// narrating its own work, not user activity, and the sidebar sorts by
// updated_at — bumping it would float the thread on a background report (same
// rule SetThreadWorktreeSetupState follows). It also does not advance
// history_rev: the column is not window-visible, it rides GetThreadLiveState
// rather than SyncThreadWindow.
func (s *Store) SetThreadLiveTodo(threadID string, todo ThreadLiveTodo) error {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return fmt.Errorf("store: set thread live todo: empty thread id")
	}
	if len(todo.Steps) == 0 {
		return fmt.Errorf("%w: %s (use ClearThreadLiveTodo)", ErrEmptyThreadLiveTodo, threadID)
	}
	encoded, err := json.Marshal(todo)
	if err != nil {
		return fmt.Errorf("store: encode thread %s live todo: %w", threadID, err)
	}
	if len(encoded) > maxThreadLiveTodoBytes {
		return fmt.Errorf("%w: thread %s, %d bytes", ErrThreadLiveTodoTooLarge, threadID, len(encoded))
	}
	result, err := s.db.Exec(
		`UPDATE threads SET live_todo = ? WHERE id = ?`,
		string(encoded), threadID,
	)
	if err != nil {
		return fmt.Errorf("store: set thread %s live todo: %w", threadID, err)
	}
	// A thread deleted underneath the write is the one case where silence
	// would be indistinguishable from a stored list, so it is named and
	// returned rather than counted as success.
	return requireRowsAffected(result, fmt.Sprintf("store: set thread %s live todo", threadID))
}

// ClearThreadLiveTodo drops the thread's stored todo list and reports whether
// there was one to drop. Clearing an already-empty column — or a thread that
// no longer exists — is existed=false with no error: the caller's intent
// (nothing stored) already holds.
//
// The caller uses the return to decide whether an empty-list clear is worth
// pushing to live panes; a pane holds the last non-empty list in memory, so a
// clear that clears nothing is a frame nobody needs.
func (s *Store) ClearThreadLiveTodo(threadID string) (bool, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return false, fmt.Errorf("store: clear thread live todo: empty thread id")
	}
	result, err := s.db.Exec(
		`UPDATE threads SET live_todo = '' WHERE id = ? AND live_todo <> ''`,
		threadID,
	)
	if err != nil {
		return false, fmt.Errorf("store: clear thread %s live todo: %w", threadID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: clear thread %s live todo: %w", threadID, err)
	}
	return affected > 0, nil
}

// ThreadLiveTodo returns the thread's stored todo list and whether one is
// stored at all. An empty column is the no-list state; a non-empty column that
// does not decode is an ERROR, never an empty list — a reader that silently
// substituted "no todos" for an unreadable blob would hide the corruption for
// as long as the thread lives (same refusal as ProjectWorktreeSetup).
//
// Decoding is strict: an unknown field means the blob was written by something
// that does not agree with this build about what a todo list is.
func (s *Store) ThreadLiveTodo(threadID string) (ThreadLiveTodo, bool, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return ThreadLiveTodo{}, false, fmt.Errorf("store: get thread live todo: empty thread id")
	}
	var raw string
	if err := s.reader().QueryRow(
		`SELECT live_todo FROM threads WHERE id = ?`, threadID,
	).Scan(&raw); err != nil {
		return ThreadLiveTodo{}, false, fmt.Errorf("store: get thread %s live todo: %w", threadID, err)
	}
	if strings.TrimSpace(raw) == "" {
		return ThreadLiveTodo{}, false, nil
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.DisallowUnknownFields()
	var todo ThreadLiveTodo
	if err := decoder.Decode(&todo); err != nil {
		return ThreadLiveTodo{}, false, fmt.Errorf("store: decode thread %s live todo: %w", threadID, err)
	}
	return todo, true, nil
}
