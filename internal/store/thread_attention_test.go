package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// readMarker is the read marker of threadID, -1 for NULL.
func readMarker(t *testing.T, s *Store, threadID string) int64 {
	t.Helper()
	got, err := s.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread(%s): %v", threadID, err)
	}
	if got.LastReadAt == nil {
		return -1
	}
	return *got.LastReadAt
}

func setReadMarker(t *testing.T, s *Store, threadID string, at *int64) {
	t.Helper()
	if _, _, err := s.setThreadLastRead(threadID, at); err != nil {
		t.Fatalf("setThreadLastRead(%s): %v", threadID, err)
	}
}

func insertTurnAt(t *testing.T, s *Store, threadID, turnID string, index int, startedAt, completedAt int64) {
	t.Helper()
	if err := s.InsertTurn(Turn{TurnID: turnID, ThreadID: threadID, TurnIndex: index, StartedAt: startedAt}); err != nil {
		t.Fatalf("InsertTurn(%s): %v", turnID, err)
	}
	if err := s.UpdateTurnCompleted(turnID, completedAt, "end_turn", "", "", ""); err != nil {
		t.Fatalf("UpdateTurnCompleted(%s): %v", turnID, err)
	}
}

func int64Ptr(v int64) *int64 { return &v }

// A turn nobody needs to read keeps a read thread read, and changes nothing
// on a thread that still owes the user a read or was never tracked.
func TestKeepThreadReadPast(t *testing.T) {
	for _, tc := range []struct {
		name   string
		marker *int64
		setup  func(t *testing.T, s *Store, id string)
		// first leaves out the earlier turn, so the quiet turn is the
		// thread's only one.
		first bool
		moved bool
	}{
		{name: "read thread", marker: int64Ptr(150), moved: true},
		{name: "read exactly at the earlier turn", marker: int64Ptr(100), moved: true},
		{name: "earlier turn unread", marker: int64Ptr(99)},
		{name: "marked unread", marker: int64Ptr(0)},
		{name: "first turn of a read thread", marker: int64Ptr(150), first: true, moved: true},
		{name: "first turn of a thread marked unread", marker: int64Ptr(0), first: true},
		{name: "never tracked", marker: nil},
		{name: "first turn of a thread never tracked", marker: nil, first: true},
		{name: "marker past the turn", marker: int64Ptr(500)},
		{name: "flag pending", marker: int64Ptr(150), setup: func(t *testing.T, s *Store, id string) {
			if _, _, err := s.SetThreadAttention(id, AttentionReview, "PR ready", 160); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "flag read", marker: int64Ptr(150), moved: true, setup: func(t *testing.T, s *Store, id string) {
			if _, _, err := s.SetThreadAttention(id, AttentionReview, "PR ready", 140); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "turn error unread", marker: int64Ptr(150), setup: func(t *testing.T, s *Store, id string) {
			if _, err := s.AppendItem(Item{
				ID: "err-1", ThreadID: id, TurnIndex: 2, Kind: "error", Role: "system",
				Status: "completed", Summary: "boom", CreatedAt: 170, UpdatedAt: 170,
			}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			thr := makeThread("keep-read", "claude")
			if err := s.CreateThread(thr); err != nil {
				t.Fatalf("CreateThread: %v", err)
			}
			if !tc.first {
				insertTurnAt(t, s, thr.ID, "turn-1", 1, 50, 100)
			}
			setReadMarker(t, s, thr.ID, tc.marker)
			if tc.setup != nil {
				tc.setup(t, s, thr.ID)
			}
			before := readMarker(t, s, thr.ID)
			// The quiet turn's own completion is written first, as triage
			// settles it before it keeps the thread read.
			insertTurnAt(t, s, thr.ID, "turn-2", 2, 200, 300)

			moved, err := s.KeepThreadReadPast(thr.ID, "turn-2", 300)
			if err != nil {
				t.Fatalf("KeepThreadReadPast: %v", err)
			}
			if moved != tc.moved {
				t.Fatalf("moved = %v, want %v", moved, tc.moved)
			}
			want := before
			if tc.moved {
				want = 300
			}
			if got := readMarker(t, s, thr.ID); got != want {
				t.Errorf("marker = %d, want %d", got, want)
			}
		})
	}
}

// A turn an earlier round of which failed stays owed a read when a later
// round of it ends cleanly and moves its completion.
func TestKeepThreadReadPastLeavesAFailedTurnOwed(t *testing.T) {
	s := newTestStore(t)
	thr := makeThread("keep-read-failed", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	insertTurnAt(t, s, thr.ID, "turn-1", 1, 50, 100)
	setReadMarker(t, s, thr.ID, int64Ptr(150))
	if err := s.InsertTurn(Turn{TurnID: "turn-2", ThreadID: thr.ID, TurnIndex: 2, StartedAt: 200}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTurnCompleted("turn-2", 300, "error", "", "", "boom"); err != nil {
		t.Fatal(err)
	}
	if moved, err := s.KeepThreadReadPast(thr.ID, "turn-2", 400); err != nil || moved {
		t.Fatalf("KeepThreadReadPast = %v, %v; want unmoved: the turn failed", moved, err)
	}
	if got := readMarker(t, s, thr.ID); got != 150 {
		t.Errorf("marker = %d, want 150", got)
	}
}

// Another completed turn the user has not read keeps the thread owed a
// read even when it completed after the quiet turn started.
func TestKeepThreadReadPastCountsEveryOtherTurn(t *testing.T) {
	s := newTestStore(t)
	thr := makeThread("keep-read-overlap", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	insertTurnAt(t, s, thr.ID, "turn-1", 1, 50, 100)
	setReadMarker(t, s, thr.ID, int64Ptr(100))
	insertTurnAt(t, s, thr.ID, "turn-2", 2, 200, 250)
	insertTurnAt(t, s, thr.ID, "turn-3", 3, 260, 300)
	if moved, err := s.KeepThreadReadPast(thr.ID, "turn-3", 300); err != nil || moved {
		t.Fatalf("KeepThreadReadPast = %v, %v; want unmoved: turn-2 is unread", moved, err)
	}
	if got := readMarker(t, s, thr.ID); got != 100 {
		t.Errorf("marker = %d, want 100", got)
	}
}

// An attention flag is pending until a read covers it, the same way a
// completion is, and the agent can replace or lower it.
func TestThreadAttentionClearsLikeUnread(t *testing.T) {
	s := newTestStore(t)
	thr := makeThread("attention", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	insertTurnAt(t, s, thr.ID, "turn-1", 1, 50, 100)
	setReadMarker(t, s, thr.ID, int64Ptr(100))

	if _, _, err := s.SetThreadAttention(thr.ID, "urgent", "x", 200); err == nil {
		t.Fatal("SetThreadAttention accepted an unknown kind")
	}
	if _, _, err := s.SetThreadAttention(thr.ID, AttentionDecision, "two\nlines", 200); err == nil {
		t.Fatal("SetThreadAttention accepted a reason CheckAttentionReason refuses")
	}
	row, changed, err := s.SetThreadAttention(thr.ID, AttentionDecision, "Pick a release date", 200)
	if err != nil || !changed {
		t.Fatalf("SetThreadAttention = %v, %v; want changed", changed, err)
	}
	if want := (ThreadAttention{Kind: AttentionDecision, Reason: "Pick a release date", At: 200}); row.Attention == nil || *row.Attention != want {
		t.Fatalf("Attention = %+v, want %+v", row.Attention, want)
	}
	if row.UpdatedAt < 200 {
		t.Fatalf("UpdatedAt = %d, want the flag's 200 at least: raising a flag is activity", row.UpdatedAt)
	}
	if _, changed, err := s.SetThreadAttention(thr.ID, AttentionDecision, "Pick a release date", 200); err != nil || changed {
		t.Fatalf("repeat SetThreadAttention = %v, %v; want unchanged", changed, err)
	}

	// The flag is a read target: opening the thread reads it.
	read, changed, err := s.MarkThreadReadNow(context.Background(), thr.ID)
	if err != nil || !changed {
		t.Fatalf("MarkThreadReadNow = %v, %v; want the flag to be owed a read", changed, err)
	}
	if read.LastReadAt == nil || *read.LastReadAt < 200 {
		t.Fatalf("LastReadAt = %v, want past the flag at 200", read.LastReadAt)
	}

	row, changed, err = s.SetThreadAttention(thr.ID, AttentionProblem, "CI is red", *read.LastReadAt+1)
	if err != nil || !changed || row.Attention == nil || row.Attention.Kind != AttentionProblem {
		t.Fatalf("replace = %+v, %v, %v", row.Attention, changed, err)
	}
	row, changed, err = s.ClearThreadAttention(thr.ID)
	if err != nil || !changed || row.Attention != nil {
		t.Fatalf("ClearThreadAttention = %+v, %v, %v; want the flag gone", row.Attention, changed, err)
	}
	if _, changed, err := s.ClearThreadAttention(thr.ID); err != nil || changed {
		t.Fatalf("repeat ClearThreadAttention = %v, %v; want unchanged", changed, err)
	}
}

// A reason is one short line of plain text: nothing that could break the
// row it shows on or make it read as something other than what is stored.
func TestCheckAttentionReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		ok     bool
	}{
		{"a short line", "The release notes are ready", true},
		{"at the bound", strings.Repeat("é", MaxAttentionReasonRunes), true},
		{"empty", "", false},
		{"past the bound", strings.Repeat("é", MaxAttentionReasonRunes+1), false},
		{"a line break", "one\ntwo", false},
		{"a tab", "one\ttwo", false},
		{"a right-to-left override", "fix \u202egnp.exe", false},
		{"a zero-width space", "re\u200bview", false},
		{"a line separator", "one\u2028two", false},
		{"a paragraph separator", "one\u2029two", false},
		{"invalid UTF-8", "bad \xff byte", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := CheckAttentionReason(tc.reason); (err == nil) != tc.ok {
				t.Fatalf("CheckAttentionReason(%q) = %v, want ok %v", tc.reason, err, tc.ok)
			}
		})
	}
}

// The notification facts carry a flag only while it is pending, by the
// same rule as Thread.AttentionPending, with when it was raised.
func TestNotificationFactsCarryAPendingFlag(t *testing.T) {
	for _, tc := range []struct {
		name    string
		marker  *int64
		pending bool
	}{
		{"never tracked", nil, true},
		{"marked unread", int64Ptr(0), true},
		{"read before the flag", int64Ptr(150), true},
		{"read at the flag", int64Ptr(200), false},
		{"read after the flag", int64Ptr(250), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			thr := makeThread("facts-attention", "claude")
			if err := s.CreateThread(thr); err != nil {
				t.Fatalf("CreateThread: %v", err)
			}
			if _, _, err := s.SetThreadAttention(thr.ID, AttentionReview, "PR ready", 200); err != nil {
				t.Fatal(err)
			}
			setReadMarker(t, s, thr.ID, tc.marker)
			row, err := s.GetThread(thr.ID)
			if err != nil {
				t.Fatal(err)
			}
			facts, err := s.GetThreadNotificationFacts(thr.ID)
			if err != nil {
				t.Fatal(err)
			}
			if row.AttentionPending() != tc.pending {
				t.Fatalf("AttentionPending = %v, want %v", row.AttentionPending(), tc.pending)
			}
			want := ThreadNotificationFacts{Title: row.Title, Mode: row.Mode, Exists: true}
			if tc.pending {
				want.Attention, want.AttentionAt = AttentionReview, 200
			}
			if facts != want {
				t.Fatalf("facts = %+v, want %+v", facts, want)
			}
		})
	}
	s := newTestStore(t)
	thr := makeThread("facts-no-flag", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatal(err)
	}
	if facts, err := s.GetThreadNotificationFacts(thr.ID); err != nil || facts.Attention != "" || facts.AttentionAt != 0 {
		t.Fatalf("facts with no flag = %+v, %v", facts, err)
	}
}

// The read mark a spawned thread gets for answering its spawner leaves a
// flag the thread raised pending: its agent asked for the user.
func TestMarkThreadReadThroughLeavesAPendingFlag(t *testing.T) {
	s := newTestStore(t)
	thr := makeThread("worker-flag", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	insertTurnAt(t, s, thr.ID, "turn-1", 1, 50, 100)
	setReadMarker(t, s, thr.ID, int64Ptr(100))
	insertTurnAt(t, s, thr.ID, "turn-2", 2, 200, 300)
	if _, _, err := s.SetThreadAttention(thr.ID, AttentionDecision, "Pick the API shape", 250); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := s.MarkThreadReadThrough(context.Background(), thr.ID, 300); err != nil || changed {
		t.Fatalf("MarkThreadReadThrough = %v, %v; want no change while the flag is pending", changed, err)
	}
	row, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !row.AttentionPending() {
		t.Fatalf("attention = %+v, read %v; want the flag still pending", row.Attention, row.LastReadAt)
	}
	// With the flag read, the answer's read mark applies again.
	if _, _, err := s.MarkThreadReadNow(context.Background(), thr.ID); err != nil {
		t.Fatal(err)
	}
	insertTurnAt(t, s, thr.ID, "turn-3", 3, time.Now().UnixMilli()+60_000, time.Now().UnixMilli()+61_000)
	third, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := s.MarkThreadReadThrough(context.Background(), thr.ID, *third.LatestTurnCompletedAt); err != nil || !changed {
		t.Fatalf("MarkThreadReadThrough after the flag was read = %v, %v; want changed", changed, err)
	}
}
