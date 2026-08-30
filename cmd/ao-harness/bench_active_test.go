package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harnessclient"
)

func TestActiveBenchProgressIntervalProvidesSeveralProofs(t *testing.T) {
	for _, tc := range []struct {
		duration time.Duration
		want     time.Duration
	}{
		{benchActiveMinimumDuration, 7500 * time.Millisecond},
		{2 * time.Minute, 30 * time.Second},
		{10 * time.Minute, 30 * time.Second},
	} {
		if got := activeBenchProgressInterval(tc.duration); got != tc.want {
			t.Errorf("activeBenchProgressInterval(%s) = %s, want %s", tc.duration, got, tc.want)
		}
	}
}

func TestActiveBenchSelectorAcceptsOnlyLiteralThreadIDs(t *testing.T) {
	got, err := activeBenchSelector("08cf4fb1-23_7", `[data-testid="row"]`)
	if err != nil {
		t.Fatal(err)
	}
	want := `[data-ui-surface="chat"][data-thread-id="08cf4fb1-23_7"] [data-testid="row"]`
	if got != want {
		t.Fatalf("selector = %q, want %q", got, want)
	}
	for _, id := range []string{"", `thread\"] body`, "thread\nbody", "thread id", "café"} {
		if _, err := activeBenchSelector(id, "body"); err == nil {
			t.Errorf("unsafe thread id %q was accepted", id)
		}
	}
}

func TestActiveBenchProgressRequiresEveryPaneToKeepGrowing(t *testing.T) {
	ids := []string{"a", "b"}
	previous := benchVisibleProgress{AtMs: 10, TextLengths: map[string]int{"a": 100, "b": 200}}
	current := benchVisibleProgress{AtMs: 20, TextLengths: map[string]int{"a": 101, "b": 250}}
	if err := validateActiveTextGrowth(previous, current, ids); err != nil {
		t.Fatal(err)
	}

	current.TextLengths["b"] = 200
	if err := validateActiveTextGrowth(previous, current, ids); err == nil || !strings.Contains(err.Error(), "thread b") {
		t.Fatalf("stalled pane error = %v, want thread b", err)
	}
	delete(current.TextLengths, "b")
	if err := validateActiveTextGrowth(previous, current, ids); err == nil || !strings.Contains(err.Error(), "omitted") {
		t.Fatalf("missing pane error = %v, want omitted", err)
	}
}

func TestActiveBenchRequiresTimelineGrowthAcrossTheWindow(t *testing.T) {
	ids := []string{"a", "b"}
	first := benchVisibleProgress{ScrollPx: map[string]int{"a": 1000, "b": 2000}}
	last := benchVisibleProgress{ScrollPx: map[string]int{"a": 1500, "b": 2500}}
	if err := validateActiveScrollGrowth(first, last, ids); err != nil {
		t.Fatal(err)
	}

	last.ScrollPx["a"] = 1000
	if err := validateActiveScrollGrowth(first, last, ids); err == nil || !strings.Contains(err.Error(), "thread a") {
		t.Fatalf("flat timeline error = %v, want thread a", err)
	}
}

func TestTurnCompletionMatcherRejectsGapsAndOtherThreads(t *testing.T) {
	match := matchTurnCompletion("wanted")
	for _, tc := range []struct {
		name string
		ev   harnessclient.Event
		want bool
	}{
		{"match", harnessclient.Event{Data: json.RawMessage(`{"threadId":"wanted"}`)}, true},
		{"other", harnessclient.Event{Data: json.RawMessage(`{"threadId":"other"}`)}, false},
		{"gap", harnessclient.Event{Gap: true, Data: json.RawMessage(`{"threadId":"wanted"}`)}, false},
		{"malformed", harnessclient.Event{Data: json.RawMessage(`{`)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := match(tc.ev); got != tc.want {
				t.Fatalf("match() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestActiveBenchShapeIsSixOpenFourStreaming(t *testing.T) {
	if benchActivePaneCount != 6 || benchActiveStreamCount != 4 {
		t.Fatalf("active shape = %d open/%d streaming, want 6/4", benchActivePaneCount, benchActiveStreamCount)
	}
	if benchActiveStreamCount >= benchActivePaneCount {
		t.Fatal("active workload no longer carries inactive mounted panes")
	}
}
