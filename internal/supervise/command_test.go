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
		{Type: UpdateEventProgress, Progress: &startupprogress.Progress{Phase: "update.snapshot", Detail: "Backing up", UpdatedAt: 7}, Liveness: true},
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
	if len(got) != 3 || got[0].PID != 42 || got[1].Progress.Detail != "Backing up" || !got[1].Liveness ||
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
	got     []relayedProgress
	entered chan struct{}
	release chan struct{}
	fail    error
}

func (g *gatedDelivery) deliver(p startupprogress.Progress, liveness bool) error {
	g.entered <- struct{}{}
	<-g.release
	g.mu.Lock()
	defer g.mu.Unlock()
	g.got = append(g.got, relayedProgress{progress: p, liveness: liveness})
	return g.fail
}

func TestProgressRelayCoalescesWithoutLosingRealProgress(t *testing.T) {
	g := &gatedDelivery{entered: make(chan struct{}, 8), release: make(chan struct{}, 8)}
	relay := NewProgressRelay(g.deliver)
	relay.Report(startupprogress.Progress{Detail: "first"}, false)
	<-g.entered // the first delivery is blocked on a slow reader

	// While it is blocked: a real step, then heartbeats. Only the latest
	// survives, and it must still count as progress.
	relay.Report(startupprogress.Progress{Detail: "second"}, false)
	relay.Report(startupprogress.Progress{Detail: "second", UpdatedAt: 2}, true)
	relay.Report(startupprogress.Progress{Detail: "second", UpdatedAt: 3}, true)
	g.release <- struct{}{}
	<-g.entered
	g.release <- struct{}{}

	// Heartbeats alone coalesce into a heartbeat.
	time.Sleep(10 * time.Millisecond)
	relay.Report(startupprogress.Progress{Detail: "second", UpdatedAt: 4}, true)
	<-g.entered
	g.release <- struct{}{}
	if err := relay.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	want := []relayedProgress{
		{progress: startupprogress.Progress{Detail: "first"}},
		{progress: startupprogress.Progress{Detail: "second", UpdatedAt: 3}},
		{progress: startupprogress.Progress{Detail: "second", UpdatedAt: 4}, liveness: true},
	}
	if len(g.got) != len(want) {
		t.Fatalf("delivered %+v, want %+v", g.got, want)
	}
	for i := range want {
		if g.got[i] != want[i] {
			t.Fatalf("delivered %+v, want %+v", g.got, want)
		}
	}
}

func TestProgressRelayCloseFlushesAndAFailureStopsIt(t *testing.T) {
	var got []string
	relay := NewProgressRelay(func(p startupprogress.Progress, _ bool) error {
		got = append(got, p.Detail)
		return nil
	})
	relay.Report(startupprogress.Progress{Detail: "last"}, false)
	if err := relay.Close(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "last" {
		t.Fatalf("Close did not flush: %v", got)
	}
	relay.Report(startupprogress.Progress{Detail: "after close"}, false)
	if len(got) != 1 {
		t.Fatalf("a report after Close was delivered: %v", got)
	}

	broken := errors.New("broken pipe")
	failing := NewProgressRelay(func(startupprogress.Progress, bool) error { return broken })
	failing.Report(startupprogress.Progress{}, false)
	select {
	case <-failing.Failed():
	case <-time.After(5 * time.Second):
		t.Fatal("a failed delivery was not signalled")
	}
	if err := failing.Close(); !errors.Is(err, broken) {
		t.Fatalf("Close = %v, want the delivery error", err)
	}
}
