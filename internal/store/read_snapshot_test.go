package store

import "testing"

func TestReadSnapshotSurvivesConcurrentHistoryWrite(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatal(err)
	}
	seedItem(t, s, "t", "a", 0, 0, "")
	old, err := readSnapshot(s.reader(), "test history", func(q sqlQueryer) (Item, error) {
		first, _, err := s.getThreadItem(q, "t", "a")
		if err != nil {
			return Item{}, err
		}
		if _, err := s.db.Exec(`UPDATE items SET summary = 'new' WHERE thread_id = 't' AND id = 'a'`); err != nil {
			return Item{}, err
		}
		second, _, err := s.getThreadItem(q, "t", "a")
		if err == nil && (second.Rev != first.Rev || second.Summary != first.Summary) {
			t.Error("one read mixed two history snapshots")
		}
		return second, err
	})
	if err != nil {
		t.Fatal(err)
	}
	current, _, err := s.GetThreadItem("t", "a")
	if err != nil {
		t.Fatal(err)
	}
	if current.Rev <= old.Rev || current.Summary != "new" {
		t.Fatal("read snapshot blocked or lost the concurrent write")
	}
}
