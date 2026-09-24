package startupprogress

import (
	"testing"
	"time"
)

// TestStallWatchJudgesUpdatedAtAsProgressAndAliveAtAsLife: a report whose
// UpdatedAt changed resets both clocks, a heartbeat that advances only
// AliveAt resets only the sign of life, and a stopped heartbeat is reported
// ahead of missing progress.
func TestStallWatchJudgesUpdatedAtAsProgressAndAliveAtAsLife(t *testing.T) {
	const window = 30 * time.Second
	start := time.Unix(1000, 0)
	at := func(d time.Duration) time.Time { return start.Add(d) }
	migrate := Progress{Phase: "store.migrate", Detail: "Applying migration 1 of 2", UpdatedAt: 5, AliveAt: 5}

	w := NewStallWatch(window, start)
	if w.Window() != window {
		t.Fatalf("Window = %s", w.Window())
	}
	if _, had := w.Last(); had {
		t.Fatal("a new watch has a last report")
	}
	if w.Check(at(time.Hour)) != nil {
		t.Fatal("Check judged a start that never reported; silence is the caller's to name")
	}
	if w.Silent(at(window - 1)) {
		t.Fatal("silent before the window")
	}
	if !w.Silent(at(window)) {
		t.Fatal("not silent a window after the start")
	}

	if !w.Report(migrate, at(10*time.Second)) {
		t.Fatal("the first report is not progress")
	}
	if w.Silent(at(time.Hour)) {
		t.Fatal("silent after a report")
	}
	if got := w.Deadline(); !got.Equal(at(40 * time.Second)) {
		t.Fatalf("Deadline = %s, want a window after the report", got.Sub(start))
	}

	// Heartbeats keep it alive but are not progress.
	for i, d := range []time.Duration{20 * time.Second, 30 * time.Second} {
		beat := migrate
		beat.AliveAt += int64(i + 1)
		if w.Report(beat, at(d)) {
			t.Fatalf("a heartbeat at %s counted as progress", d)
		}
	}
	if got := w.Deadline(); !got.Equal(at(40 * time.Second)) {
		t.Fatalf("a heartbeat moved the deadline to %s", got.Sub(start))
	}
	if s := w.Check(at(40*time.Second - 1)); s != nil {
		t.Fatalf("stalled before the window: %v", s)
	}
	s := w.Check(at(40 * time.Second))
	if s == nil || s.Unresponsive || s.Quiet != window || s.Progress.AliveAt != migrate.AliveAt+2 {
		t.Fatalf("Check = %+v, want no progress for the window with the last heartbeat", s)
	}
	if got, want := s.Error(), "no progress for 30s in phase store.migrate (Applying migration 1 of 2)"; got != want {
		t.Fatalf("Error = %q, want %q", got, want)
	}

	// Progress restarts both clocks.
	next := Progress{Phase: "store.migrate", Detail: "Applying migration 2 of 2", UpdatedAt: 9, AliveAt: 9}
	if !w.Report(next, at(41*time.Second)) {
		t.Fatal("a changed UpdatedAt is not progress")
	}
	if s := w.Check(at(70 * time.Second)); s != nil {
		t.Fatalf("stalled after progress: %v", s)
	}
	// A repeated report is neither progress nor life.
	if w.Report(next, at(60*time.Second)) {
		t.Fatal("an identical report counted as progress")
	}
	s = w.Check(at(71 * time.Second))
	if s == nil || !s.Unresponsive || s.Quiet != window {
		t.Fatalf("Check = %+v, want stopped responding: the repeat was no sign of life", s)
	}
	if got, want := s.Error(), "backend stopped responding for 30s in phase store.migrate (Applying migration 2 of 2)"; got != want {
		t.Fatalf("Error = %q, want %q", got, want)
	}
	if last, had := w.Last(); !had || last != next {
		t.Fatalf("Last = %+v %v", last, had)
	}
}

// TestStallWatchStoppedHeartbeatTakesPrecedence: when neither field moved
// for the window the stall names the stopped heartbeat, even though
// progress is missing too.
func TestStallWatchStoppedHeartbeatTakesPrecedence(t *testing.T) {
	start := time.Unix(1000, 0)
	w := NewStallWatch(0, start)
	if w.Window() != StallWindow {
		t.Fatalf("a zero window took %s, want StallWindow", w.Window())
	}
	w.Report(Progress{Phase: "store.open", UpdatedAt: 1, AliveAt: 1}, start)
	s := w.Check(start.Add(StallWindow))
	if s == nil || !s.Unresponsive {
		t.Fatalf("Check = %+v, want stopped responding", s)
	}
}

func TestStallErrorRoundsTheQuietTime(t *testing.T) {
	p := Progress{Phase: "store.open"}
	for _, c := range []struct {
		quiet time.Duration
		want  string
	}{
		{30*time.Second + 400*time.Millisecond, "no progress for 30s in phase store.open (Starting)"},
		{400*time.Millisecond + 300*time.Microsecond, "no progress for 400ms in phase store.open (Starting)"},
	} {
		s := Stall{Progress: p, Quiet: c.quiet}
		if got := s.Error(); got != c.want {
			t.Errorf("Error(%s) = %q, want %q", c.quiet, got, c.want)
		}
	}
}
