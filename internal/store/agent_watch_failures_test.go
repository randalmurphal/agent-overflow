package store

import (
	"maps"
	"testing"
)

// A watch whose failure notice is out is marked until the notice is
// withdrawn, so a restart can still withdraw it. A cancelled watch's mark is
// not listed: nothing is left to end its episode.
func TestAgentWatchFailureNoticeMarks(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "t")
	for _, id := range []string{"inbox", "ci", "gone"} {
		if _, err := s.RegisterAgentWake(AgentWake{ID: id, ThreadID: "t", Kind: AgentWakeWatch, Label: id, Command: "true", IntervalSeconds: 60, NextAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	notices := func() map[string]int64 {
		t.Helper()
		got, err := s.AgentWatchFailureNotices()
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := notices(); len(got) != 0 {
		t.Fatalf("before any mark: %v", got)
	}
	for id, at := range map[string]int64{"inbox": 10, "gone": 20} {
		if err := s.MarkAgentWatchFailureNoticed(id, at); err != nil {
			t.Fatal(err)
		}
	}
	if _, cancelled, err := s.CancelAgentWake("t", "gone", "", "cancelled"); err != nil || !cancelled {
		t.Fatalf("cancel: %v %v", cancelled, err)
	}
	if got, want := notices(), map[string]int64{"inbox": 10}; !maps.Equal(got, want) {
		t.Fatalf("marks = %v, want %v", got, want)
	}
	for range 2 {
		if err := s.ClearAgentWatchFailureNoticed("inbox"); err != nil {
			t.Fatal(err)
		}
	}
	if got := notices(); len(got) != 0 {
		t.Fatalf("after the clear: %v", got)
	}
}
