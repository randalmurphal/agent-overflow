package supervise

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/startupprogress"
)

func TestUpdateEventsRoundTripAndOtherLinesAreIgnored(t *testing.T) {
	var out bytes.Buffer
	events := []UpdateEvent{
		{Type: UpdateEventStarted, PID: 42},
		{Type: UpdateEventProgress, Progress: &startupprogress.Progress{Phase: "update.snapshot", Detail: "Backing up", UpdatedAt: 7, AliveAt: 9}},
		{Type: UpdateEventResult, Outcome: UpdateOutcomeRolledBack, Reason: "the trial exited"},
	}
	out.WriteString("a log line\n")
	for _, event := range events {
		if err := WriteUpdateEvent(&out, event); err != nil {
			t.Fatal(err)
		}
	}
	var got []UpdateEvent
	for _, line := range strings.Split(out.String(), "\n") {
		event, ok, err := ParseUpdateEvent(line + "\r")
		if err != nil {
			t.Fatalf("ParseUpdateEvent(%q): %v", line, err)
		}
		if ok {
			got = append(got, event)
		}
	}
	if len(got) != 3 || got[0].PID != 42 || *got[1].Progress != *events[1].Progress ||
		got[2].Outcome != UpdateOutcomeRolledBack || got[2].Reason != "the trial exited" {
		t.Fatalf("round trip = %+v", got)
	}
	for _, bad := range []string{
		UpdateEventPrefix + "{",
		UpdateEventPrefix + `{"type":"other"}`,
		UpdateEventPrefix + `{"type":"progress"}`,
	} {
		if _, ok, err := ParseUpdateEvent(bad); !ok || err == nil {
			t.Errorf("ParseUpdateEvent(%q) = %v, %v; want a prefixed error", bad, ok, err)
		}
	}
}

// gatedDelivery blocks each delivery until released, recording what arrived.
type gatedDelivery struct {
	mu      sync.Mutex
	got     []startupprogress.Progress
	entered chan struct{}
	release chan struct{}
	fail    error
}

func (g *gatedDelivery) deliver(p startupprogress.Progress) error {
	g.entered <- struct{}{}
	<-g.release
	g.mu.Lock()
	defer g.mu.Unlock()
	g.got = append(g.got, p)
	return g.fail
}

// Coalescing keeps the latest report, which carries every advance of
// UpdatedAt and AliveAt it replaced.
func TestProgressRelayCoalescesToTheLatestReport(t *testing.T) {
	g := &gatedDelivery{entered: make(chan struct{}, 8), release: make(chan struct{}, 8)}
	relay := NewProgressRelay(g.deliver)
	relay.Report(startupprogress.Progress{Detail: "first", UpdatedAt: 1, AliveAt: 1})
	<-g.entered // the first delivery is blocked on a slow reader

	// While it is blocked: a step, then heartbeats. Only the latest
	// survives, and its UpdatedAt still shows the step.
	relay.Report(startupprogress.Progress{Detail: "second", UpdatedAt: 2, AliveAt: 2})
	relay.Report(startupprogress.Progress{Detail: "second", UpdatedAt: 2, AliveAt: 3})
	relay.Report(startupprogress.Progress{Detail: "second", UpdatedAt: 2, AliveAt: 4})
	g.release <- struct{}{}
	<-g.entered
	g.release <- struct{}{}
	if err := relay.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	want := []startupprogress.Progress{
		{Detail: "first", UpdatedAt: 1, AliveAt: 1},
		{Detail: "second", UpdatedAt: 2, AliveAt: 4},
	}
	if len(g.got) != len(want) || g.got[0] != want[0] || g.got[1] != want[1] {
		t.Fatalf("delivered %+v, want %+v", g.got, want)
	}
}

func TestProgressRelayCloseFlushesAndAFailureStopsIt(t *testing.T) {
	var got []string
	relay := NewProgressRelay(func(p startupprogress.Progress) error {
		got = append(got, p.Detail)
		return nil
	})
	relay.Report(startupprogress.Progress{Detail: "last"})
	if err := relay.Close(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "last" {
		t.Fatalf("Close did not flush: %v", got)
	}
	relay.Report(startupprogress.Progress{Detail: "after close"})
	if len(got) != 1 {
		t.Fatalf("a report after Close was delivered: %v", got)
	}

	broken := errors.New("broken pipe")
	failing := NewProgressRelay(func(startupprogress.Progress) error { return broken })
	failing.Report(startupprogress.Progress{})
	select {
	case <-failing.Failed():
	case <-time.After(5 * time.Second):
		t.Fatal("a failed delivery was not signalled")
	}
	if err := failing.Close(); !errors.Is(err, broken) {
		t.Fatalf("Close = %v, want the delivery error", err)
	}
}

// TestCommandRelayStampsOneClock: the launcher judges a command by one
// reporter. The command's steps are progress; a heartbeat is a sign of
// life, and progress only when the shared sampler saw the command working;
// a trial's report is progress where its UpdatedAt changed and life where
// only its AliveAt did, restamped in the command's clock.
func TestCommandRelayStampsOneClock(t *testing.T) {
	var clock int64 = 5000
	now := func() time.Time { return time.UnixMilli(clock) }
	var work startupprogress.ProcessWork
	sampler := startupprogress.NewSampler(startupprogress.SamplerOptions{
		Interval: time.Second,
		ReadWork: func() (startupprogress.ProcessWork, error) { return work, nil },
		Logf:     t.Logf,
	})
	var mu sync.Mutex
	var delivered []startupprogress.Progress
	r := newCommandRelay(func(p startupprogress.Progress) error {
		mu.Lock()
		defer mu.Unlock()
		delivered = append(delivered, p)
		return nil
	}, sampler, now, time.Hour)

	type stamps struct{ updated, alive int64 }
	current := func() (startupprogress.Progress, stamps) {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.current, stamps{r.current.UpdatedAt, r.current.AliveAt}
	}
	expect := func(what string, want stamps) {
		t.Helper()
		if _, got := current(); got != want {
			t.Fatalf("%s: stamps = %+v, want %+v", what, got, want)
		}
	}

	r.beat()
	if p, _ := current(); p.Phase != "" || p.StartedAt != 5000 {
		t.Fatalf("a beat before any step reported %+v", p)
	}
	expect("a beat before any step", stamps{0, 0})

	clock = 6000
	r.step("update.lock", "Waiting for the previous version to stop")
	expect("a step", stamps{6000, 6000})
	clock = 7000
	r.beat()
	expect("an idle beat", stamps{6000, 7000})
	clock = 8000
	work.CPU += time.Second
	r.beat()
	expect("a beat that saw work", stamps{8000, 8000})
	// A clock that stood still still advances.
	r.beat()
	expect("an idle beat on a stopped clock", stamps{8000, 8001})

	clock = 9000
	child := startupprogress.Progress{Phase: "store.migrate", Detail: "Applying migration 1 of 2", Step: 1, Steps: 2, UpdatedAt: 100, AliveAt: 100, UpdatingTo: "2.0.0"}
	r.forward(child)
	expect("the first trial report", stamps{9000, 9000})
	if p, _ := current(); p.Phase != child.Phase || p.Detail != child.Detail || p.Step != 1 || p.Steps != 2 || p.UpdatingTo != "2.0.0" {
		t.Fatalf("forwarded %+v, want the trial's phase and detail", p)
	}
	clock = 10000
	child.AliveAt = 101
	r.forward(child)
	expect("a trial heartbeat", stamps{9000, 10000})
	clock = 11000
	r.forward(child)
	expect("a repeated trial report", stamps{9000, 10000})
	child.UpdatedAt, child.AliveAt, child.Detail = 102, 102, "Applying migration 2 of 2"
	r.forward(child)
	expect("trial progress", stamps{11000, 11000})

	clock = 12000
	r.step("update.trial.stop", "Stopping the trial")
	expect("a step after the trial", stamps{12000, 12000})
	clock = 13000
	r.forward(child)
	expect("a trial report after a step of the command's", stamps{13000, 13000})

	r.close()
	mu.Lock()
	defer mu.Unlock()
	if last, _ := current(); len(delivered) == 0 || delivered[len(delivered)-1] != last {
		t.Fatalf("close did not deliver the last report: %+v", delivered)
	}
}
