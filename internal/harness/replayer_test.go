package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type emittedEvent struct {
	kind string
	data string
}

type replaySink struct {
	mu     sync.Mutex
	events []emittedEvent
	states []string
}

func (s *replaySink) emit(kind string, data json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, emittedEvent{kind: kind, data: string(data)})
}

func (s *replaySink) progress(st ReplayStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states = append(s.states, st.State)
}

func (s *replaySink) eventCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

func writeRecording(t *testing.T, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rec.jsonl")
	data := ""
	for _, l := range lines {
		data += l + "\n"
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	return path
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestReplayerPlaysInOrderWithTiming(t *testing.T) {
	sink := &replaySink{}
	r := NewReplayer(sink.emit, sink.progress)
	path := writeRecording(t, []string{
		`{"ts":1000,"threadId":"t1","kind":"provider:item_event","data":{"n":1}}`,
		`{"ts":1040,"threadId":"t1","kind":"provider:item_event","data":{"n":2}}`,
		`{"ts":99999999,"threadId":"t1","kind":"provider:turn_completed","data":{"n":3}}`,
	})

	start := time.Now()
	if _, err := r.Start(path, ReplayOptions{MaxGapMs: 50}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "replay done", func() bool { return r.Status().State == "done" })

	// The absurd third-event gap must have been capped at MaxGapMs.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("replay took %s; MaxGapMs cap not applied", elapsed)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.events) != 3 {
		t.Fatalf("events = %+v, want 3", sink.events)
	}
	if sink.events[0].data != `{"n":1}` || sink.events[2].kind != "provider:turn_completed" {
		t.Fatalf("events out of order: %+v", sink.events)
	}
	if st := r.Status(); st.Position != 3 || st.Total != 3 {
		t.Fatalf("final status = %+v", st)
	}
}

func TestReplayerPauseResumeStep(t *testing.T) {
	sink := &replaySink{}
	r := NewReplayer(sink.emit, sink.progress)
	var lines []string
	for i := 1; i <= 5; i++ {
		lines = append(lines, fmt.Sprintf(`{"ts":%d,"threadId":"t1","kind":"e","data":{"n":%d}}`, 1000+i, i))
	}
	path := writeRecording(t, lines)

	if _, err := r.Start(path, ReplayOptions{StartPaused: true}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Paused start: nothing emits.
	time.Sleep(50 * time.Millisecond)
	if n := sink.eventCount(); n != 0 {
		t.Fatalf("paused replay emitted %d events", n)
	}

	// Two steps → exactly two events.
	if _, err := r.Step(); err != nil {
		t.Fatalf("Step: %v", err)
	}
	waitFor(t, "first stepped event", func() bool { return sink.eventCount() == 1 })
	if _, err := r.Step(); err != nil {
		t.Fatalf("Step 2: %v", err)
	}
	waitFor(t, "second stepped event", func() bool { return sink.eventCount() == 2 })
	time.Sleep(50 * time.Millisecond)
	if n := sink.eventCount(); n != 2 {
		t.Fatalf("steps leaked: %d events after 2 steps", n)
	}

	// Resume plays out the rest.
	r.Resume()
	waitFor(t, "replay done", func() bool { return r.Status().State == "done" })
	if n := sink.eventCount(); n != 5 {
		t.Fatalf("events after resume = %d, want 5", n)
	}
}

func TestReplayerStop(t *testing.T) {
	sink := &replaySink{}
	r := NewReplayer(sink.emit, sink.progress)
	path := writeRecording(t, []string{
		`{"ts":1,"threadId":"t1","kind":"e","data":{"n":1}}`,
		`{"ts":2,"threadId":"t1","kind":"e","data":{"n":2}}`,
	})
	if _, err := r.Start(path, ReplayOptions{StartPaused: true}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	r.Stop()
	waitFor(t, "stopped", func() bool { return r.Status().State == "stopped" })

	// A stopped replayer accepts a fresh run.
	if _, err := r.Start(path, ReplayOptions{}); err != nil {
		t.Fatalf("restart after stop: %v", err)
	}
	waitFor(t, "second run done", func() bool { return r.Status().State == "done" })
}

// TestReplayerStopIsSynchronous: Stop must not return while the run
// goroutine can still emit — a reset that starts deleting state right
// after Stop() would otherwise race one final stale event.
func TestReplayerStopIsSynchronous(t *testing.T) {
	sink := &replaySink{}
	r := NewReplayer(sink.emit, sink.progress)
	path := writeRecording(t, []string{
		`{"ts":0,"threadId":"t1","kind":"e","data":{"n":1}}`,
		`{"ts":60000,"threadId":"t1","kind":"e","data":{"n":2}}`, // 60s gap, uncapped
	})
	if _, err := r.Start(path, ReplayOptions{MaxGapMs: -1}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "first event", func() bool { return sink.eventCount() == 1 })

	st := r.Stop()
	if st.State != "stopped" {
		t.Fatalf("Stop returned state %q, want the terminal \"stopped\"", st.State)
	}
	if n := sink.eventCount(); n != 1 {
		t.Fatalf("events after Stop = %d, want 1 — the run goroutine emitted past Stop's return", n)
	}
	// The no-emissions guarantee holds immediately: a fresh run is
	// accepted with no still-active refusal window.
	if _, err := r.Start(path, ReplayOptions{StartPaused: true}); err != nil {
		t.Fatalf("restart immediately after Stop: %v", err)
	}
	r.Stop()
}

func TestReplayerRefusesConcurrentRuns(t *testing.T) {
	sink := &replaySink{}
	r := NewReplayer(sink.emit, sink.progress)
	path := writeRecording(t, []string{`{"ts":1,"threadId":"t1","kind":"e","data":{}}`})
	if _, err := r.Start(path, ReplayOptions{StartPaused: true}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := r.Start(path, ReplayOptions{}); err == nil {
		t.Fatal("second Start succeeded while a replay was active")
	}
	r.Stop()
}

func TestReplayerThreadFilterAndBadInput(t *testing.T) {
	sink := &replaySink{}
	r := NewReplayer(sink.emit, sink.progress)
	path := writeRecording(t, []string{
		`{"ts":1,"threadId":"t1","kind":"e","data":{"n":1}}`,
		`{"ts":2,"threadId":"t2","kind":"e","data":{"n":2}}`,
	})
	if _, err := r.Start(path, ReplayOptions{ThreadFilter: "t2"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "done", func() bool { return r.Status().State == "done" })
	sink.mu.Lock()
	if len(sink.events) != 1 || sink.events[0].data != `{"n":2}` {
		t.Fatalf("filtered events = %+v", sink.events)
	}
	sink.mu.Unlock()

	// Mid-file corruption fails the load; a mid-write truncated final
	// line (no terminating newline) is tolerated; a newline-complete
	// malformed final line is corruption, not truncation.
	corrupt := writeRecording(t, []string{
		`{"ts":1,"threadId":"t1","kind":"e"}`,
		`{"not json`,
		`{"ts":2,"threadId":"t1","kind":"e"}`,
	})
	if _, err := r.Start(corrupt, ReplayOptions{}); err == nil {
		t.Fatal("Start accepted a corrupt recording")
	}
	truncated := filepath.Join(t.TempDir(), "truncated.jsonl")
	if err := os.WriteFile(truncated, []byte(
		`{"ts":1,"threadId":"t1","kind":"e"}`+"\n"+
			`{"ts":2,"threadId":"t1","kind":"e","data":{"n":`, // no trailing newline
	), 0o600); err != nil {
		t.Fatalf("write truncated recording: %v", err)
	}
	if _, err := r.Start(truncated, ReplayOptions{}); err != nil {
		t.Fatalf("Start rejected a recording with only a truncated tail: %v", err)
	}
	waitFor(t, "truncated-tail done", func() bool { return r.Status().State == "done" })

	corruptTail := writeRecording(t, []string{
		`{"ts":1,"threadId":"t1","kind":"e"}`,
		`{"not json`,
	})
	if _, err := r.Start(corruptTail, ReplayOptions{}); err == nil {
		t.Fatal("Start accepted a newline-complete corrupt final line as a truncated tail")
	}

	if _, err := r.Start(writeRecording(t, []string{""}), ReplayOptions{}); err == nil {
		t.Fatal("Start accepted an empty recording")
	}
}

// TestReplayerPauseDuringGapHoldsEmission pins the mid-gap pause
// contract: once Pause returns, no further event may emit until
// resume/step, even when the pause lands inside a recorded inter-event
// gap whose timer is already running.
func TestReplayerPauseDuringGapHoldsEmission(t *testing.T) {
	sink := &replaySink{}
	r := NewReplayer(sink.emit, sink.progress)
	path := writeRecording(t, []string{
		`{"ts":0,"threadId":"t1","kind":"e","data":{"n":1}}`,
		`{"ts":600,"threadId":"t1","kind":"e","data":{"n":2}}`,
	})
	if _, err := r.Start(path, ReplayOptions{MaxGapMs: -1}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "first event", func() bool { return sink.eventCount() == 1 })
	r.Pause() // lands inside the 600ms gap before event 2

	time.Sleep(800 * time.Millisecond) // well past the recorded gap
	if n := sink.eventCount(); n != 1 {
		t.Fatalf("paused replay emitted event 2 out of the gap timer (%d events)", n)
	}

	r.Resume()
	waitFor(t, "done after resume", func() bool { return r.Status().State == "done" })
	if n := sink.eventCount(); n != 2 {
		t.Fatalf("events after resume = %d, want 2", n)
	}
}

// TestReplayerStepSkipsGapWhilePaused: a step during a mid-gap pause
// releases the next event immediately — operator-paced stepping is not
// throttled by the recorded gap.
func TestReplayerStepSkipsGapWhilePaused(t *testing.T) {
	sink := &replaySink{}
	r := NewReplayer(sink.emit, sink.progress)
	path := writeRecording(t, []string{
		`{"ts":0,"threadId":"t1","kind":"e","data":{"n":1}}`,
		`{"ts":60000,"threadId":"t1","kind":"e","data":{"n":2}}`, // 60s gap, uncapped
	})
	if _, err := r.Start(path, ReplayOptions{MaxGapMs: -1}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "first event", func() bool { return sink.eventCount() == 1 })
	r.Pause()
	if _, err := r.Step(); err != nil {
		t.Fatalf("Step: %v", err)
	}
	// waitFor's 5s deadline is the assertion: without gap-skipping the
	// second event sits behind the 60s gap.
	waitFor(t, "stepped event skipped the gap", func() bool { return sink.eventCount() == 2 })
}

// blockingSink gates emit() so tests can hold the run goroutine inside
// an emission and observe the ticket channel deterministically.
type blockingSink struct {
	replaySink
	gate chan struct{}
}

func (s *blockingSink) blockedEmit(kind string, data json.RawMessage) {
	<-s.gate
	s.emit(kind, data)
}

func TestReplayerStepRefusesWhenTicketPending(t *testing.T) {
	sink := &blockingSink{gate: make(chan struct{}, 16)}
	r := NewReplayer(sink.blockedEmit, sink.progress)
	path := writeRecording(t, []string{
		`{"ts":0,"threadId":"t1","kind":"e","data":{"n":1}}`,
		`{"ts":1,"threadId":"t1","kind":"e","data":{"n":2}}`,
		`{"ts":2,"threadId":"t1","kind":"e","data":{"n":3}}`,
	})
	if _, err := r.Start(path, ReplayOptions{StartPaused: true}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := r.Step(); err != nil {
		t.Fatalf("Step 1: %v", err)
	}
	// The run goroutine consumed ticket 1 and is now blocked inside
	// emit(event 1) — it cannot consume another ticket. The second
	// step queues; the third must be refused, not silently coalesced.
	waitFor(t, "run goroutine blocked in emit", func() bool {
		if _, err := r.Step(); err != nil {
			return false // ticket 1 not consumed yet; try again
		}
		return true
	})
	if _, err := r.Step(); err == nil {
		t.Fatal("Step succeeded while a ticket was already pending")
	}
	// Release everything and let the two owed steps drain.
	sink.gate <- struct{}{}
	sink.gate <- struct{}{}
	waitFor(t, "two stepped events", func() bool { return sink.eventCount() == 2 })
	r.Stop()
	waitFor(t, "stopped", func() bool { return r.Status().State == "stopped" })
}
