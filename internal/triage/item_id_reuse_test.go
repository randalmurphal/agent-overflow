package triage

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func rowSummaries(t *testing.T, st *store.Store, threadID, kind string) []string {
	t.Helper()
	rows := findItemsByKind(t, st, threadID, kind)
	ids := make(map[string]bool, len(rows))
	summaries := make([]string, 0, len(rows))
	for _, row := range rows {
		if ids[row.ID] {
			t.Fatalf("two %s rows share id %s", kind, row.ID)
		}
		ids[row.ID] = true
		summaries = append(summaries, row.Summary)
	}
	return summaries
}

// A row id built from a per-thread counter must not name a saved row once
// the counter is gone, or the new row replaces the saved one. The counters
// go with the thread's state at CleanupThread and with the process at a
// restart; the rows stay.
func TestCounterBuiltRowIDsSkipSavedRows(t *testing.T) {
	noticeMeta, _ := json.Marshal(map[string]string{"kind": "warning"})
	notice := func(text string) func(*Router) error {
		return func(r *Router) error {
			return r.HandleSynthetic(provider.ProviderEvent{Kind: provider.EventNotification, ThreadID: "t1", Content: text, Meta: noticeMeta, Timestamp: time.Now()})
		}
	}
	providerError := func(text string) func(*Router) error {
		return func(r *Router) error {
			return r.HandleSynthetic(provider.ProviderEvent{Kind: provider.EventError, ThreadID: "t1", Content: text, Timestamp: time.Now()})
		}
	}
	turnError := func(text string) func(*Router) error {
		return func(r *Router) error { return r.PersistTurnError("t1", 0, text) }
	}
	commandResult := func(text string) func(*Router) error {
		return func(r *Router) error { return r.HandleSynthetic(commandResultEvent("t1", "", text)) }
	}
	rows := []struct {
		name, kind string
		write      func(string) func(*Router) error
		// summarized is false for a kind whose text is not its summary.
		summarized bool
	}{
		{"notice", itemKindNotification, notice, true},
		{"provider error", ItemKindError, providerError, true},
		{"send failure error", ItemKindError, turnError, true},
		{"command result without a provider id", itemKindCommandResult, commandResult, false},
	}
	losses := []struct {
		name string
		lose func(*testing.T, *Router, *store.Store) *Router
	}{
		{"session cleanup", func(_ *testing.T, r *Router, _ *store.Store) *Router {
			r.CleanupThread("t1")
			return r
		}},
		{"restart", func(t *testing.T, _ *Router, st *store.Store) *Router {
			next := NewRouter(st, func(eventchan.Channel, any) {})
			t.Cleanup(next.DrainWireItemRefresh)
			return next
		}},
	}
	for _, row := range rows {
		for _, loss := range losses {
			t.Run(row.name+" after "+loss.name, func(t *testing.T) {
				router, st, _ := newTestRouter(t)
				createTestThread(t, st, "t1")
				if err := row.write("first")(router); err != nil {
					t.Fatalf("first row: %v", err)
				}
				router = loss.lose(t, router, st)
				if err := row.write("second")(router); err != nil {
					t.Fatalf("second row: %v", err)
				}

				got := rowSummaries(t, st, "t1", row.kind)
				if len(got) != 2 {
					t.Fatalf("%s rows = %q, want the first and the second", row.kind, got)
				}
				if row.summarized && !slices.Equal(got, []string{"first", "second"}) {
					t.Fatalf("%s rows = %q, want the first kept beside the second", row.kind, got)
				}
			})
		}
	}
}

// A provider that re-sends a turn's start (Claude after an interrupt) does
// not hand the turn's saved error rows to the errors that follow.
func TestErrorAfterATurnRestartKeepsTheEarlierError(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	handle := func(evt provider.ProviderEvent) {
		t.Helper()
		if err := router.Handle(evt); err != nil {
			t.Fatalf("handle %s: %v", evt.Kind, err)
		}
	}
	failure := func(text string) provider.ProviderEvent {
		return provider.ProviderEvent{Kind: provider.EventError, ThreadID: "t1", Content: text, Timestamp: time.Now()}
	}

	handle(turnStartEvent("t1", 1))
	handle(failure("first"))
	handle(turnStartEvent("t1", 1))
	handle(failure("second"))

	if got := rowSummaries(t, st, "t1", ItemKindError); !slices.Equal(got, []string{"first", "second"}) {
		t.Fatalf("error rows = %q, want the first kept beside the second", got)
	}
}
