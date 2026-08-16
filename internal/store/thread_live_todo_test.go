package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func seedLiveTodoThread(t *testing.T, s *Store, id string) {
	t.Helper()
	if err := s.CreateThread(makeThread(id, "claude")); err != nil {
		t.Fatalf("create thread %s: %v", id, err)
	}
}

func TestThreadLiveTodoRoundTrip(t *testing.T) {
	s := newTestStore(t)
	seedLiveTodoThread(t, s, "t-todo-roundtrip")

	if _, found, err := s.ThreadLiveTodo("t-todo-roundtrip"); err != nil || found {
		t.Fatalf("a fresh thread must report no list; found=%v err=%v", found, err)
	}

	todo := ThreadLiveTodo{
		Steps: []ThreadLiveTodoStep{
			{Step: "first", Status: "inProgress", ID: "1", Owner: "helper"},
			{Step: "second", Status: "pending"},
		},
		UpdatedAt: 1_700_000_000_000,
	}
	if err := s.SetThreadLiveTodo("t-todo-roundtrip", todo); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, found, err := s.ThreadLiveTodo("t-todo-roundtrip")
	if err != nil || !found {
		t.Fatalf("read back: found=%v err=%v", found, err)
	}
	if !reflect.DeepEqual(got, todo) {
		t.Fatalf("round trip = %+v, want %+v", got, todo)
	}

	// A report restates the whole list; the second write replaces, never merges.
	replacement := ThreadLiveTodo{
		Steps:     []ThreadLiveTodoStep{{Step: "only", Status: "completed"}},
		UpdatedAt: 1_700_000_001_000,
	}
	if err := s.SetThreadLiveTodo("t-todo-roundtrip", replacement); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, _, err = s.ThreadLiveTodo("t-todo-roundtrip")
	if err != nil {
		t.Fatalf("read replacement: %v", err)
	}
	if !reflect.DeepEqual(got, replacement) {
		t.Fatalf("replacement = %+v, want %+v", got, replacement)
	}
}

// The clear's return is what gates the live push, so it has to be exact:
// true only when there was something stored, and idempotent thereafter.
func TestClearThreadLiveTodoReportsWhatItCleared(t *testing.T) {
	s := newTestStore(t)
	seedLiveTodoThread(t, s, "t-todo-clear")

	existed, err := s.ClearThreadLiveTodo("t-todo-clear")
	if err != nil || existed {
		t.Fatalf("clearing an empty column: existed=%v err=%v, want false/nil", existed, err)
	}

	if err := s.SetThreadLiveTodo("t-todo-clear", ThreadLiveTodo{
		Steps:     []ThreadLiveTodoStep{{Step: "one", Status: "pending"}},
		UpdatedAt: 5,
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	existed, err = s.ClearThreadLiveTodo("t-todo-clear")
	if err != nil || !existed {
		t.Fatalf("clearing a stored list: existed=%v err=%v, want true/nil", existed, err)
	}
	if _, found, err := s.ThreadLiveTodo("t-todo-clear"); err != nil || found {
		t.Fatalf("after clear: found=%v err=%v", found, err)
	}
	existed, err = s.ClearThreadLiveTodo("t-todo-clear")
	if err != nil || existed {
		t.Fatalf("second clear: existed=%v err=%v, want false/nil", existed, err)
	}

	// A thread that is gone is the same answer, not an error: the caller's
	// intent (nothing stored for it) already holds.
	existed, err = s.ClearThreadLiveTodo("t-todo-missing")
	if err != nil || existed {
		t.Fatalf("clearing an unknown thread: existed=%v err=%v, want false/nil", existed, err)
	}
}

// An empty list IS a clear, and it must have exactly one representation —
// otherwise a reader has to treat `{"steps":[]}` and an empty column as the
// same thing.
func TestSetThreadLiveTodoRefusesAnEmptyList(t *testing.T) {
	s := newTestStore(t)
	seedLiveTodoThread(t, s, "t-todo-empty")

	err := s.SetThreadLiveTodo("t-todo-empty", ThreadLiveTodo{UpdatedAt: 7})
	if !errors.Is(err, ErrEmptyThreadLiveTodo) {
		t.Fatalf("set with no steps = %v, want ErrEmptyThreadLiveTodo", err)
	}
	if err := s.SetThreadLiveTodo("", ThreadLiveTodo{
		Steps: []ThreadLiveTodoStep{{Step: "one", Status: "pending"}},
	}); err == nil {
		t.Fatal("set with an empty thread id reported success")
	}
}

// A write that matched no row is a lost write, and a lost write that reports
// success is indistinguishable from a stored list.
func TestSetThreadLiveTodoRefusesAnUnknownThread(t *testing.T) {
	s := newTestStore(t)
	err := s.SetThreadLiveTodo("t-todo-nonexistent", ThreadLiveTodo{
		Steps:     []ThreadLiveTodoStep{{Step: "one", Status: "pending"}},
		UpdatedAt: 1,
	})
	if err == nil {
		t.Fatal("writing a todo list for a nonexistent thread reported success")
	}
}

// A blob this build cannot read is an ERROR, never an empty list: silently
// substituting "no todos" would hide the corruption for the thread's lifetime.
func TestThreadLiveTodoRefusesACorruptBlob(t *testing.T) {
	s := newTestStore(t)
	seedLiveTodoThread(t, s, "t-todo-corrupt")

	// json_valid passes, this build's decoder does not — the shape drifted.
	if _, err := s.db.Exec(
		`UPDATE threads SET live_todo = ? WHERE id = ?`,
		`{"steps":[{"step":"one","status":"pending"}],"updatedAt":1,"unexpected":true}`,
		"t-todo-corrupt",
	); err != nil {
		t.Fatalf("seed drifted blob: %v", err)
	}
	if _, found, err := s.ThreadLiveTodo("t-todo-corrupt"); err == nil {
		t.Fatalf("an unreadable blob must be an error; got found=%v", found)
	}

	// The CHECK is the schema's own guard against the half-written case.
	if _, err := s.db.Exec(
		`UPDATE threads SET live_todo = 'not json' WHERE id = ?`, "t-todo-corrupt",
	); err == nil {
		t.Fatal("threads accepted a non-JSON live_todo; CHECK constraint missing")
	}
	if _, err := s.db.Exec(
		`UPDATE threads SET live_todo = NULL WHERE id = ?`, "t-todo-corrupt",
	); err == nil {
		t.Fatal("threads accepted a NULL live_todo")
	}
}

// UpdateThread rewrites the row's mutable columns; the todo list is written by
// its own narrow accessor and must survive an unrelated whole-row update (a
// workspace switch or a rename commits one while a session is reporting).
func TestUpdateThreadPreservesLiveTodo(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("t-todo-preserved", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	todo := ThreadLiveTodo{
		Steps:     []ThreadLiveTodoStep{{Step: "keep me", Status: "inProgress"}},
		UpdatedAt: 42,
	}
	if err := s.SetThreadLiveTodo(thread.ID, todo); err != nil {
		t.Fatalf("set: %v", err)
	}
	thread.Title = "renamed"
	if err := s.UpdateThread(thread); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	got, found, err := s.ThreadLiveTodo(thread.ID)
	if err != nil || !found {
		t.Fatalf("after UpdateThread: found=%v err=%v", found, err)
	}
	if !reflect.DeepEqual(got, todo) {
		t.Fatalf("live todo = %+v after UpdateThread, want %+v", got, todo)
	}
}

// A todo tick is the provider narrating its own work, not user activity: the
// sidebar sorts by updated_at, and the window replica keys off history_rev.
// Neither may move.
func TestThreadLiveTodoWritesLeaveThreadStampsAlone(t *testing.T) {
	s := newTestStore(t)
	seedLiveTodoThread(t, s, "t-todo-stamps")

	read := func() (int64, int64) {
		t.Helper()
		var updatedAt, rev int64
		if err := s.db.QueryRow(
			`SELECT updated_at, history_rev FROM threads WHERE id = ?`, "t-todo-stamps",
		).Scan(&updatedAt, &rev); err != nil {
			t.Fatalf("read stamps: %v", err)
		}
		return updatedAt, rev
	}
	wantUpdatedAt, wantRev := read()

	if err := s.SetThreadLiveTodo("t-todo-stamps", ThreadLiveTodo{
		Steps:     []ThreadLiveTodoStep{{Step: "one", Status: "pending"}},
		UpdatedAt: 9,
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if gotUpdatedAt, gotRev := read(); gotUpdatedAt != wantUpdatedAt || gotRev != wantRev {
		t.Fatalf("set moved thread stamps: updated_at %d->%d, history_rev %d->%d",
			wantUpdatedAt, gotUpdatedAt, wantRev, gotRev)
	}
	if _, err := s.ClearThreadLiveTodo("t-todo-stamps"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if gotUpdatedAt, gotRev := read(); gotUpdatedAt != wantUpdatedAt || gotRev != wantRev {
		t.Fatalf("clear moved thread stamps: updated_at %d->%d, history_rev %d->%d",
			wantUpdatedAt, gotUpdatedAt, wantRev, gotRev)
	}
}

func TestSetThreadLiveTodoRefusesAnOversizedList(t *testing.T) {
	s := newTestStore(t)
	seedLiveTodoThread(t, s, "t-todo-oversize")

	oversized := ThreadLiveTodo{
		Steps:     []ThreadLiveTodoStep{{Step: strings.Repeat("x", maxThreadLiveTodoBytes), Status: "pending"}},
		UpdatedAt: 1,
	}
	err := s.SetThreadLiveTodo("t-todo-oversize", oversized)
	if !errors.Is(err, ErrThreadLiveTodoTooLarge) {
		t.Fatalf("err = %v, want ErrThreadLiveTodoTooLarge", err)
	}
	if _, found, err := s.ThreadLiveTodo("t-todo-oversize"); err != nil || found {
		t.Fatalf("a refused write must store nothing; found=%v err=%v", found, err)
	}
}
