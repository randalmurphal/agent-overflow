package store

import (
	"context"
	"database/sql"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// openOtherWriter opens a connection on the store's file that is not the
// store's: another process's writer, as far as SQLite can tell. It never
// waits for a lock.
func openOtherWriter(t *testing.T, s *Store) *sql.Conn {
	t.Helper()
	db, err := sql.Open("sqlite", poolDSN(s.path, []connPragma{{name: "busy_timeout", dsnValue: "0"}}))
	if err != nil {
		t.Fatalf("open other writer: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("other writer conn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// landedWrite is the other connection's write: a row no store operation
// under test reads.
const landedWrite = `INSERT INTO ui_state (scope, key, value, updated_at) VALUES ('test', 'landed', '1', 0)`

// sqliteCode is the extended SQLite result code err carries, or 0.
func sqliteCode(err error) int {
	var e *sqlite.Error
	if errors.As(err, &e) {
		return e.Code()
	}
	return 0
}

// A writer transaction holds the write lock from its begin: another
// connection cannot commit between its read and its write, which would
// fail the write at once with SQLITE_BUSY_SNAPSHOT, busy_timeout or not.
func TestWriterTransactionsHoldTheWriteLockFromBegin(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	other := openOtherWriter(t, s)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM threads`).Scan(&n); err != nil {
		t.Fatalf("read: %v", err)
	}
	_, err = other.ExecContext(context.Background(), landedWrite)
	if sqliteCode(err)&0xff != sqlite3.SQLITE_BUSY {
		t.Errorf("another connection's write during the transaction = %v, want SQLITE_BUSY", err)
	}
	if _, err := tx.Exec(`UPDATE threads SET id = id WHERE 0`); err != nil {
		t.Fatalf("write after read: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// runWhileOtherWriterHoldsLock takes the write lock on another connection
// with a write applied but not committed, runs op, and commits once op
// waits for the lock. op must wait out the other writer and succeed; a
// transaction that read before taking the lock fails instead.
func runWhileOtherWriterHoldsLock(t *testing.T, s *Store, opFrame string, op func() error) {
	t.Helper()
	ctx := context.Background()
	other := openOtherWriter(t, s)
	if _, err := other.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("take write lock: %v", err)
	}
	if _, err := other.ExecContext(ctx, landedWrite); err != nil {
		_, _ = other.ExecContext(ctx, "ROLLBACK")
		t.Fatalf("land write: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- op() }()
	if !awaitBusyWaitIn(opFrame, done) {
		_, _ = other.ExecContext(ctx, "ROLLBACK")
		t.Fatalf("%s did not wait for the other writer: %v", opFrame, <-done)
	}
	if _, err := other.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatalf("commit landed write: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("%s after the other writer committed: %v", opFrame, err)
	}
}

// busyWaitFrames are the busy handlers a lock wait sleeps in: SQLite's
// own, which PRAGMA busy_timeout installs, and the driver's, which
// replaces it on a connection once a call there carries a context that
// can end and keeps its schedule (modernc.org/sqlite busy.go).
var busyWaitFrames = []string{"sqliteDefaultBusyCallback", "modernc.org/sqlite.busyHandler("}

// awaitBusyWaitIn reports whether the goroutine running fn sits in a
// busy handler (busyWaitFrames) before done delivers. done is left for
// the caller.
func awaitBusyWaitIn(fn string, done chan error) bool {
	buf := make([]byte, 1<<20)
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		n := runtime.Stack(buf, true)
		if n == len(buf) {
			buf = make([]byte, 2*len(buf))
			continue
		}
		for _, stack := range strings.Split(string(buf[:n]), "\n\n") {
			if !strings.Contains(stack, fn) {
				continue
			}
			for _, frame := range busyWaitFrames {
				if strings.Contains(stack, frame) {
					return true
				}
			}
		}
		select {
		case err := <-done:
			done <- err
			return false
		case <-time.After(time.Millisecond):
		}
	}
	return false
}

func TestMoveThreadDraftWaitsForAnotherWriter(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	a := createDraftCleanupThread(t, s, "source", "chat")
	b := createDraftCleanupThread(t, s, "destination", "chat")
	source := ThreadDraft{ThreadID: a.ID, Content: "moving", Attachments: "[]", TerminalChips: "[]"}
	if _, err := s.UpsertThreadDraft(source); err != nil {
		t.Fatal(err)
	}
	runWhileOtherWriterHoldsLock(t, s, "MoveThreadDraft", func() error {
		_, err := s.MoveThreadDraft(source, ThreadDraft{ThreadID: b.ID, Content: source.Content, Attachments: "[]", TerminalChips: "[]"})
		return err
	})
	if got, _, err := s.GetThreadDraft(b.ID); err != nil || got.Content != source.Content {
		t.Fatalf("destination draft = %+v, %v", got, err)
	}
}

func TestCommitIncomingThreadTransferWaitsForAnotherWriter(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	request := transferRequest("incoming", "copy", "incoming")
	if _, err := s.CreateThreadTransfer(request); err != nil {
		t.Fatal(err)
	}
	advanceTransfer(t, s, request.ID, "prepared")
	source := newTestStore(t)
	target := makeThread(request.ThreadID, "claude")
	if err := source.CreateThread(target); err != nil {
		t.Fatal(err)
	}
	var history strings.Builder
	if err := source.ExportThreadHistory(context.Background(), target.ID, &history); err != nil {
		t.Fatal(err)
	}
	runWhileOtherWriterHoldsLock(t, s, "CommitIncomingThreadTransfer", func() error {
		_, err := s.CommitIncomingThreadTransfer(context.Background(), request.ID, strings.Repeat("b", 64), transferTestSecret(), target, strings.NewReader(history.String()))
		return err
	})
	if row, err := s.GetThreadTransfer(request.ID); err != nil || row.Phase != "complete" {
		t.Fatalf("transfer = %+v, %v", row, err)
	}
}
