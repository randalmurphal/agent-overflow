package store

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

// A database failure is the engine or the handle failing, however deep in a
// wrapped error. A decision the store made, or a missing row, is not one.
func TestIsDatabaseFailure(t *testing.T) {
	s := newTestStore(t)
	_, engine := s.db.Exec(`INSERT INTO no_such_table (id) VALUES (1)`)
	if engine == nil {
		t.Fatal("a write to a missing table succeeded")
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"an SQLite error", engine, true},
		{"an SQLite error under wrapping", fmt.Errorf("store: accept receipt: %w", fmt.Errorf("begin: %w", engine)), true},
		{"a finished transaction", fmt.Errorf("store: commit: %w", sql.ErrTxDone), true},
		{"a closed connection", sql.ErrConnDone, true},
		{"a missing row", fmt.Errorf("store: get thread: %w", sql.ErrNoRows), false},
		{"a refusal the store decided", errors.New("store: queue thread wake: token is required"), false},
		{"another package's failure", errors.New("git worktree add failed: fatal: not a git repository"), false},
		{"no error", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsDatabaseFailure(tc.err); got != tc.want {
				t.Fatalf("IsDatabaseFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
