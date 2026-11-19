package store

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/threadmode"
)

// One label names one row for the life of the thread: re-registering replaces
// it, a cancelled row is revived, the other kind cannot take a live label, and
// the live rows are bounded.
func TestRegisterAgentWakeKeepsOneRowPerLabel(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "t")
	first, err := s.RegisterAgentWake(AgentWake{ID: "a", ThreadID: "t", Kind: AgentWakeLoop, Label: "board", Note: "n1", IntervalSeconds: 60, NextAt: 100})
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.RegisterAgentWake(AgentWake{ID: "b", ThreadID: "t", Kind: AgentWakeLoop, Label: "board", Note: "n2", IntervalSeconds: 120, NextAt: 200})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID || again.IntervalSeconds != 120 || again.Note != "n2" || again.NextAt != 200 {
		t.Fatalf("re-register = %+v, want row %s replaced", again, first.ID)
	}
	if _, err := s.RegisterAgentWake(AgentWake{ID: "c", ThreadID: "t", Kind: AgentWakeWatch, Label: "board", Command: "true"}); !errors.Is(err, ErrAgentWakeLabelTaken) {
		t.Fatalf("watch on a live loop label: %v, want ErrAgentWakeLabelTaken", err)
	}
	if _, cancelled, err := s.CancelAgentWake("t", "board", "", "cancelled"); err != nil || !cancelled {
		t.Fatalf("cancel: %v %v", cancelled, err)
	}
	revived, err := s.RegisterAgentWake(AgentWake{ID: "d", ThreadID: "t", Kind: AgentWakeWatch, Label: "board", Command: "true", LongRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	if revived.ID != first.ID || revived.Kind != AgentWakeWatch || revived.State != AgentWakeArmed || !revived.LongRunning {
		t.Fatalf("revived = %+v", revived)
	}
	for i := 1; i < MaxAgentWakesPerThread; i++ {
		if _, err := s.RegisterAgentWake(AgentWake{ID: "x" + strconv.Itoa(i), ThreadID: "t", Kind: AgentWakeLoop, Label: "l" + strconv.Itoa(i), IntervalSeconds: 60, NextAt: 1}); err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
	}
	if _, err := s.RegisterAgentWake(AgentWake{ID: "over", ThreadID: "t", Kind: AgentWakeLoop, Label: "over", IntervalSeconds: 60, NextAt: 1}); !errors.Is(err, ErrAgentWakeLimit) {
		t.Fatalf("past the bound: %v, want ErrAgentWakeLimit", err)
	}
}

// A claim made from a due read loses to a registration of the same label
// since, which reuses the row and leaves it armed: the new command does not
// run early and keeps its own schedule. A claim from a read of the new
// registration, once due, runs it.
func TestAStaleClaimLosesToARegistration(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "t")
	if _, err := s.RegisterAgentWake(AgentWake{ID: "w", ThreadID: "t", Kind: AgentWakeWatch, Label: "poll", Command: "old",
		IntervalSeconds: 60, NextAt: 100}); err != nil {
		t.Fatal(err)
	}
	due, err := s.DueAgentWakes(100, 8)
	if err != nil || len(due) != 1 {
		t.Fatalf("due = %v %v", due, err)
	}
	replaced, err := s.RegisterAgentWake(AgentWake{ID: "other", ThreadID: "t", Kind: AgentWakeWatch, Label: "poll", Command: "new",
		IntervalSeconds: 3600, NextAt: 5_000})
	if err != nil || replaced.ID != "w" || replaced.State != AgentWakeArmed {
		t.Fatalf("re-register = %+v %v, want the row reused and armed", replaced, err)
	}
	if _, claimed, err := s.ClaimAgentWake(due[0], 100, 160, 0); err != nil || claimed {
		t.Fatalf("the stale claim = %v %v, want a lost race", claimed, err)
	}
	if row, _, _ := s.GetAgentWake("w"); row.State != AgentWakeArmed || row.NextAt != 5_000 || row.Command != "new" {
		t.Fatalf("after the stale claim = state %s next %d command %q, want the new registration as it was", row.State, row.NextAt, row.Command)
	}
	due, err = s.DueAgentWakes(5_000, 8)
	if err != nil || len(due) != 1 {
		t.Fatalf("due at the new time = %v %v", due, err)
	}
	if row, claimed, err := s.ClaimAgentWake(due[0], 5_000, 8_600_000, 0); err != nil || !claimed || row.State != AgentWakeRunning {
		t.Fatalf("a claim of the new registration = %+v %v %v", row, claimed, err)
	}
}

// A run is claimed and finished conditionally, so a second claim, a claim of
// a cancelled row and a finish of a row nobody claimed are all lost races.
func TestAgentWakeRunsAreConditional(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "t")
	row, err := s.RegisterAgentWake(AgentWake{ID: "a", ThreadID: "t", Kind: AgentWakeLoop, Label: "l", IntervalSeconds: 60, NextAt: 100})
	if err != nil {
		t.Fatal(err)
	}
	if due, err := s.DueAgentWakes(99, 8); err != nil || len(due) != 0 {
		t.Fatalf("due before its time: %v %v", due, err)
	}
	if due, err := s.DueAgentWakes(100, 8); err != nil || len(due) != 1 {
		t.Fatalf("due at its time: %v %v", due, err)
	}
	if _, finished, err := s.FinishAgentWake(row.ID, "early", "", 0, AgentWakeNoWake); err != nil || finished {
		t.Fatalf("finish before claim: %v %v", finished, err)
	}
	if _, claimed, err := s.ClaimAgentWake(row, 100, 160, 0); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if _, claimed, err := s.ClaimAgentWake(row, 100, 160, 0); err != nil || claimed {
		t.Fatalf("second claim: %v %v", claimed, err)
	}
	if _, finished, err := s.FinishAgentWake(row.ID, "delivered", "", 0, AgentWakeCarried); err != nil || !finished {
		t.Fatalf("finish: %v %v", finished, err)
	}
	if next, err := s.NextAgentWakeDue(); err != nil || !next.Set || next.At != 160 {
		t.Fatalf("next due = %+v %v, want 160", next, err)
	}
	if ids, err := s.CancelAgentWakesForThread("t", "", "archived"); err != nil || len(ids) != 1 {
		t.Fatalf("cancel thread: %v %v", ids, err)
	}
	if _, claimed, err := s.ClaimAgentWake(row, 200, 260, 0); err != nil || claimed {
		t.Fatalf("claim of a cancelled row: %v %v", claimed, err)
	}
	if next, err := s.NextAgentWakeDue(); err != nil || next.Set {
		t.Fatalf("next due after cancel = %+v %v, want none", next, err)
	}
}

// A row's wake is refused while more than its allowance of earlier wakes
// waits on the queue.
func TestQueueAgentWakeRefusesMoreThanAheadQueuedWakes(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "t")
	item := func(id, sendID string) FlushQueueItem {
		return FlushQueueItem{ID: id, ThreadID: "t", SendID: sendID, Message: "m", EnqueuedAt: 1}
	}
	if err := s.QueueAgentWake(item("q1", "agent-wake:a:1"), "agent-wake:a:", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueAgentWake(item("q2", "agent-wake:a:2"), "agent-wake:a:", 0); !errors.Is(err, ErrAgentWakeStillQueued) {
		t.Fatalf("second wake: %v, want ErrAgentWakeStillQueued", err)
	}
	if err := s.QueueAgentWake(item("q3", "agent-wake:b:1"), "agent-wake:b:", 0); err != nil {
		t.Fatalf("another row's wake: %v", err)
	}
	if err := s.DeleteFlushQueueItem("q1"); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueAgentWake(item("q4", "agent-wake:a:3"), "agent-wake:a:", 0); err != nil {
		t.Fatalf("after the first left the queue: %v", err)
	}
	// A wake allowed one ahead of it queues behind the waiting one, and no
	// further.
	if err := s.QueueAgentWake(item("q5", "agent-wake:a:4"), "agent-wake:a:", 1); err != nil {
		t.Fatalf("one ahead allowed: %v", err)
	}
	if err := s.QueueAgentWake(item("q6", "agent-wake:a:5"), "agent-wake:a:", 1); !errors.Is(err, ErrAgentWakeStillQueued) {
		t.Fatalf("two ahead with one allowed: %v, want ErrAgentWakeStillQueued", err)
	}
}

// A thread delete reports the home of the assistant record it deletes,
// whether the thread goes or is retired to a holder its forks read; a
// thread that is no assistant's reports nothing.
func TestDeletingAThreadReportsItsAssistantRecord(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"agent", "plain"} {
		mustCreateThread(t, s, id)
	}
	mustCreateAssistant(t, s, "agent", "/home/agent")
	// An assistant thread a pointer fork reads, which its delete retires.
	seedLinearSource(t, s, "held", 2)
	mustCreateAssistant(t, s, "held", "/home/held")
	mustPointerFork(t, s, "held", "reader", ForkCut{})
	var deleted []string
	s.OnAgentRecordDeleted(func(home string) { deleted = append(deleted, home) })
	for _, id := range []string{"plain", "agent", "held"} {
		if err := s.DeleteThread(id); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(deleted, []string{"/home/agent", "/home/held"}) {
		t.Fatalf("reported deleted records %v, want the two assistants' homes", deleted)
	}
	if held, err := s.GetThread("held"); err != nil || held.Mode != threadmode.ModeHolder {
		t.Fatalf("the read thread = %+v, %v; want it retired to a holder", held, err)
	}
}

// A watch's undelivered wake keeps its body until a later undelivered wake
// replaces it or the label is registered again; a delivery leaves it, a loop
// keeps none, and an output past the cap is refused.
func TestAgentWakeKeepsUndeliveredOutput(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "t")
	watch, err := s.RegisterAgentWake(AgentWake{ID: "w", ThreadID: "t", Kind: AgentWakeWatch, Label: "poll", Command: "true",
		IntervalSeconds: 60, TimeoutSeconds: 120, NextAt: 100})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := s.RegisterAgentWake(AgentWake{ID: "l", ThreadID: "t", Kind: AgentWakeLoop, Label: "board", Note: "n", IntervalSeconds: 60, NextAt: 100})
	if err != nil {
		t.Fatal(err)
	}
	read := func(id string) AgentWake {
		t.Helper()
		row, found, err := s.GetAgentWake(id)
		if err != nil || !found {
			t.Fatalf("read %s: %v %v", id, found, err)
		}
		return row
	}
	if _, err := s.NoteAgentWakeUndelivered(watch.ID, "not delivered: gone", "first body"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NoteAgentWakeUndelivered(loop.ID, "not delivered: gone", "loop body"); err != nil {
		t.Fatal(err)
	}
	if row := read(watch.ID); row.LastOutput != "first body" || row.LastOutcome != "not delivered: gone" || row.TimeoutSeconds != 120 {
		t.Fatalf("watch after an undelivered wake = %+v", row)
	}
	if row := read(loop.ID); row.LastOutput != "" {
		t.Fatalf("a loop kept output %q", row.LastOutput)
	}
	if _, err := s.NoteAgentWakeOutcome(watch.ID, "delivered", AgentWakeCarried); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NoteAgentWakeUndelivered(watch.ID, "not delivered: joined", ""); err != nil {
		t.Fatal(err)
	}
	if row := read(watch.ID); row.LastOutput != "first body" {
		t.Fatalf("a delivery or an empty output replaced the kept body: %q", row.LastOutput)
	}
	if _, err := s.ReleaseQueuedAgentWake(watch.ID, "not delivered", "second body", 50); err != nil {
		t.Fatal(err)
	}
	if row := read(watch.ID); row.LastOutput != "second body" || row.NextAt != 50 {
		t.Fatalf("after a boot release = %+v", row)
	}
	if _, err := s.NoteAgentWakeUndelivered(watch.ID, "x", strings.Repeat("x", MaxAgentWakeOutputBytes+1)); err == nil {
		t.Fatal("an output past the cap was written")
	}
	if again, err := s.RegisterAgentWake(AgentWake{ID: "w2", ThreadID: "t", Kind: AgentWakeWatch, Label: "poll", Command: "false",
		IntervalSeconds: 60, NextAt: 100}); err != nil || again.LastOutput != "" || again.TimeoutSeconds != 0 {
		t.Fatalf("re-register = %+v %v, want the kept output and the old timeout cleared", again, err)
	}
}

// A row's undelivered outcome stays through every write that sent no wake
// (a quiet or failed run, a process exit, a quit's recovery) with the count a
// skip left, and goes only when a wake carries it or the label is registered
// again. Every way a wake is lost sets it.
func TestAgentWakeUndeliveredStaysUntilAWakeCarriesIt(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "t")
	if _, err := s.RegisterAgentWake(AgentWake{ID: "w", ThreadID: "t", Kind: AgentWakeWatch, Label: "poll", Command: "true",
		IntervalSeconds: 60, NextAt: 100}); err != nil {
		t.Fatal(err)
	}
	read := func() AgentWake {
		t.Helper()
		row, found, err := s.GetAgentWake("w")
		if err != nil || !found {
			t.Fatalf("read: %v %v", found, err)
		}
		return row
	}
	at := int64(100)
	finish := func(outcome, output string, missed int64, carry AgentWakeCarry) AgentWake {
		t.Helper()
		if _, claimed, err := s.ClaimAgentWake(read(), at, at+60_000, missed); err != nil || !claimed {
			t.Fatalf("claim at %d: %v %v", at, claimed, err)
		}
		at += 60_000
		if _, finished, err := s.FinishAgentWake("w", outcome, output, missed, carry); err != nil || !finished {
			t.Fatalf("finish %q: %v %v", outcome, finished, err)
		}
		return read()
	}
	const skipped = "skipped: the previous wake is still queued"

	if row := finish(skipped, "skipped body", 1, AgentWakeLost); row.Undelivered != skipped || row.Missed != 1 ||
		row.LastOutput != "skipped body" {
		t.Fatalf("after a skip = %+v", row)
	}
	if row := finish("no output", "", 1, AgentWakeNoWake); row.LastOutcome != "no output" || row.Undelivered != skipped ||
		row.Missed != 1 || row.LastOutput != "skipped body" || row.LastRunAt != at-60_000 {
		t.Fatalf("a quiet run cleared what the skip left: %+v", row)
	}
	if row := finish("error: exit status 3", "", 1, AgentWakeNoWake); row.Undelivered != skipped || row.Missed != 1 {
		t.Fatalf("a failed run cleared what the skip left: %+v", row)
	}
	if _, err := s.NoteAgentWakeOutcome("w", "exited; restarting in 1s", AgentWakeNoWake); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.ClaimAgentWake(read(), at, at+60_000, 1); err != nil || !claimed {
		t.Fatalf("claim before the quit: %v %v", claimed, err)
	}
	if _, _, err := s.RecoverAgentWake("w", "cancelled-by-quit"); err != nil {
		t.Fatal(err)
	}
	if row := read(); row.LastOutcome != "cancelled-by-quit" || row.Undelivered != skipped {
		t.Fatalf("an exit or a quit's recovery cleared it: %+v", row)
	}
	if row := finish("delivered", "", 2, AgentWakeCarried); row.Undelivered != "" || row.Missed != 2 ||
		row.LastOutput != "skipped body" {
		t.Fatalf("the wake that carried it = %+v, want it cleared and the body still readable", row)
	}

	for _, lose := range []struct {
		name  string
		write func(outcome string) error
	}{
		{"finish", func(outcome string) error {
			if _, claimed, err := s.ClaimAgentWake(read(), at, at+60_000, 0); err != nil || !claimed {
				return fmt.Errorf("claim: %v %v", claimed, err)
			}
			at += 60_000
			_, _, err := s.FinishAgentWake("w", outcome, "", 0, AgentWakeLost)
			return err
		}},
		{"session end", func(outcome string) error { _, err := s.NoteAgentWakeUndelivered("w", outcome, ""); return err }},
		{"shutdown", func(outcome string) error {
			if _, claimed, err := s.ClaimAgentWake(read(), at, at+60_000, 0); err != nil || !claimed {
				return fmt.Errorf("claim: %v %v", claimed, err)
			}
			_, err := s.ReleaseUndeliveredAgentWake("w", outcome, "", at)
			return err
		}},
		{"boot", func(outcome string) error { _, err := s.ReleaseQueuedAgentWake("w", outcome, "", at); return err }},
	} {
		outcome := "not delivered: " + lose.name
		if err := lose.write(outcome); err != nil {
			t.Fatalf("%s: %v", lose.name, err)
		}
		if row := read(); row.Undelivered != outcome {
			t.Fatalf("%s left undelivered %q", lose.name, row.Undelivered)
		}
		if _, err := s.NoteAgentWakeOutcome("w", "delivered", AgentWakeCarried); err != nil || read().Undelivered != "" {
			t.Fatalf("a long-running watch's delivery after %s did not carry it: %+v %v", lose.name, read(), err)
		}
	}

	if _, err := s.NoteAgentWakeUndelivered("w", "not delivered", ""); err != nil {
		t.Fatal(err)
	}
	if again, err := s.RegisterAgentWake(AgentWake{ID: "w2", ThreadID: "t", Kind: AgentWakeWatch, Label: "poll",
		Command: "false", IntervalSeconds: 60, NextAt: at}); err != nil || again.Undelivered != "" {
		t.Fatalf("re-register = %+v %v, want undelivered cleared", again, err)
	}
}
