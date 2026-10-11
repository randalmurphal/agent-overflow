package store

import (
	"maps"
	"testing"
)

// A thread keeps its first mark until a clear, which reports whether there
// was one, and its delete takes the mark with it.
func TestSessionStartFailureMarks(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "a")
	mustCreateThread(t, s, "b")
	for _, mark := range []struct {
		thread string
		at     int64
	}{{"a", 10}, {"a", 20}, {"b", 30}} {
		if err := s.MarkSessionStartFailed(mark.thread, mark.at); err != nil {
			t.Fatalf("mark %s: %v", mark.thread, err)
		}
	}
	marks, err := s.SessionStartFailures()
	if err != nil || !maps.Equal(marks, map[string]int64{"a": 10, "b": 30}) {
		t.Fatalf("marks = %v, %v; want a's first mark and b's", marks, err)
	}
	if cleared, err := s.ClearSessionStartFailed("a"); err != nil || !cleared {
		t.Fatalf("clear a: %v %v", cleared, err)
	}
	if cleared, err := s.ClearSessionStartFailed("a"); err != nil || cleared {
		t.Fatalf("clear a again: %v %v, want nothing to clear", cleared, err)
	}
	if err := s.DeleteThread("b"); err != nil {
		t.Fatalf("delete b: %v", err)
	}
	if marks, err := s.SessionStartFailures(); err != nil || len(marks) != 0 {
		t.Fatalf("marks after the clear and the delete = %v, %v", marks, err)
	}
}
