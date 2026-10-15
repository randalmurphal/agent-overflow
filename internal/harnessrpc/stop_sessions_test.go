package harnessrpc

import (
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

// A stop settles the requests other threads made of the stopped one, and the
// answer wakes the thread that asked, which starts its session again. The
// stop follows those sessions and stops them too, until none is left.
func TestStopAllSessionsStopsTheSessionsItsStopsWoke(t *testing.T) {
	h, host := newHarnessTestHost(t)
	host.liveAfterStop = [][]string{{"lead"}}
	threads := []store.Thread{{ID: "lead"}, {ID: "peer"}}
	if err := h.stopAllSessions(threads, func(string, error) error { return nil }); err != nil {
		t.Fatalf("stopAllSessions: %v", err)
	}
	if want := []string{"lead", "peer", "lead"}; !slices.Equal(host.stopped, want) {
		t.Fatalf("stopped = %v, want %v", host.stopped, want)
	}
}

// A chain of sessions that keeps starting is a defect, reported rather than
// waited out.
func TestStopAllSessionsReportsSessionsThatKeepStarting(t *testing.T) {
	h, host := newHarnessTestHost(t)
	for range stopPasses {
		host.liveAfterStop = append(host.liveAfterStop, []string{"lead"})
	}
	err := h.stopAllSessions([]store.Thread{{ID: "lead"}}, func(string, error) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "still starting") {
		t.Fatalf("stopAllSessions = %v, want the sessions still starting reported", err)
	}
	if len(host.stopped) != stopPasses {
		t.Fatalf("stopped %d times, want %d passes", len(host.stopped), stopPasses)
	}
}
