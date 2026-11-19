package store

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestRemoteWatchScheduleIsTheEarliestRowTheWatcherServes: the watcher's
// two loops sleep until these moments, so each reads exactly the rows its
// due list would serve, and nothing when there are none.
func TestRemoteWatchScheduleIsTheEarliestRowTheWatcherServes(t *testing.T) {
	s, threadID := remoteWatchTestThread(t)
	moment := func(read func() (ScheduledMoment, error)) ScheduledMoment {
		t.Helper()
		got, err := read()
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := moment(s.NextRemoteWatchCheck); got.Set {
		t.Fatalf("no watches, next check = %+v, want none", got)
	}
	if got := moment(s.NextOwedRemoteCancel); got.Set {
		t.Fatalf("no watches, next owed cancel = %+v, want none", got)
	}

	base := time.Now().Add(time.Hour).UnixMilli()
	watch := func(threadID string, next int64) RemoteWatch {
		t.Helper()
		w := RemoteWatch{ComputerID: uuid.NewString(), RequestID: uuid.NewString(), ThreadID: threadID, Fingerprint: strings.Repeat("a", 64), Label: "test"}
		if _, err := s.RegisterRemoteWatch(w); err != nil {
			t.Fatal(err)
		}
		running := RemoteJob{ID: w.RequestID, SourceThreadID: threadID, State: "running"}
		if err := s.ObserveRemoteWatch(w.ComputerID, w.RequestID, running, "", next); err != nil {
			t.Fatal(err)
		}
		return w
	}
	watch(threadID, base+2000)
	owed := watch(threadID, base)
	if err := s.OweRemoteCancel(owed.ComputerID, owed.RequestID, base, base+500); err != nil {
		t.Fatal(err)
	}
	archived := Thread{ID: uuid.NewString(), Provider: "codex"}
	if err := s.CreateThread(archived); err != nil {
		t.Fatal(err)
	}
	watch(archived.ID, base+100)
	if _, _, err := s.ArchiveThread(archived.ID); err != nil {
		t.Fatal(err)
	}
	dismissed := watch(threadID, base+200)
	if err := s.DismissRemoteWatch(dismissed.ComputerID, dismissed.RequestID); err != nil {
		t.Fatal(err)
	}

	// A watch whose cancel is owed, one held for an archived thread and a
	// dismissed one are not checked: the next check is the later watch's.
	if got := moment(s.NextRemoteWatchCheck); got != (ScheduledMoment{At: base + 2000, Set: true}) {
		t.Fatalf("next check = %+v, want the later watch's at %d", got, base+2000)
	}
	if got := moment(s.NextOwedRemoteCancel); got != (ScheduledMoment{At: base + 500, Set: true}) {
		t.Fatalf("next owed cancel = %+v, want %d", got, base+500)
	}

	// Unarchiving brings the held completion back; settling the cancel
	// brings its watch back.
	if _, _, err := s.UnarchiveThread(archived.ID); err != nil {
		t.Fatal(err)
	}
	if got := moment(s.NextRemoteWatchCheck); got.At != base+100 {
		t.Fatalf("after the unarchive, next check = %+v, want the held watch's at %d", got, base+100)
	}
	if err := s.SettleRemoteCancel(owed.ComputerID, owed.RequestID); err != nil {
		t.Fatal(err)
	}
	if got := moment(s.NextRemoteWatchCheck); got.At != base {
		t.Fatalf("after the cancel settled, next check = %+v, want its watch's at %d", got, base)
	}
	if got := moment(s.NextOwedRemoteCancel); got.Set {
		t.Fatalf("no cancel is owed, next owed cancel = %+v", got)
	}
}

// TestNextRemoteWatchCheckReadsThePendingIndex: the watcher reads its
// schedule after every pass, so the read stops at the first row of the
// partial index rather than scanning the table.
func TestNextRemoteWatchCheckReadsThePendingIndex(t *testing.T) {
	s, _ := remoteWatchTestThread(t)
	if plan := explainWakeCarrierPlan(t, s.db, nextRemoteWatchCheckSQL); !strings.Contains(plan, "idx_remote_watches_pending") {
		t.Fatalf("next check plan = %q, want idx_remote_watches_pending", plan)
	}
}
