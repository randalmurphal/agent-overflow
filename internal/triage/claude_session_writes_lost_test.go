package triage

import (
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// Claude sends system/session_writes_lost as it exits, after the last
// result. It lands as one error row on the thread's last turn, a repeat
// adds nothing, and it does not reopen the finished turn.
func TestClaudeSessionWritesLostPersistsOneErrorRowAfterTheTurn(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	if err := router.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}

	line := []byte(`{"type":"system","subtype":"session_writes_lost","error_code":"ENOSPC","lost":["transcript","global_config"],"transcript_session_ids":["s-1"],"uuid":"u-1","session_id":"s-1"}`)
	var summary string
	for range 2 {
		events, err := claude.ParseLine("t1", line)
		if err != nil || len(events) != 1 {
			t.Fatalf("parse = %+v, %v", events, err)
		}
		summary = events[0].Content
		if err := router.Handle(events[0]); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var errorRows int
	for _, item := range items {
		if item.Kind != ItemKindError {
			continue
		}
		errorRows++
		if item.Summary != summary || item.TurnIndex != 0 || item.Status != statusCompleted {
			t.Fatalf("error row = %+v, want the composed summary on turn 0", item)
		}
	}
	if errorRows != 1 {
		t.Fatalf("error rows = %d, want 1: %+v", errorRows, items)
	}
	if _, open := router.openTurnIndex("t1"); open {
		t.Fatal("a non-fatal error after the result reopened the turn")
	}
}
