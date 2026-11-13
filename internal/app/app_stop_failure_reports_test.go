package app

import (
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// A Stop or a session end whose bookkeeping fails tells the person in the
// thread, instead of leaving rows that read as running with only a log line
// to say why.

func threadErrorRows(t *testing.T, st *store.Store, threadID, prefix string) []string {
	t.Helper()
	items, err := st.ListItems(threadID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var rows []string
	for _, item := range items {
		if item.Kind == triage.ItemKindError && strings.HasPrefix(item.Summary, prefix) {
			rows = append(rows, item.Summary)
		}
	}
	return rows
}

func TestInterruptTurnReportsAStopItCouldNotRecord(t *testing.T) {
	t.Parallel()
	f := newClaudeAgentKillFixture(t)
	execOnFile(t, f.dbPath, `CREATE TRIGGER fail_stop_row BEFORE INSERT ON items WHEN NEW.summary = 'Stopped by user' BEGIN SELECT RAISE(ABORT, 'injected stop write failure'); END`)

	if err := f.app.InterruptTurn(f.thread.ID, nil); err != nil {
		t.Fatalf("InterruptTurn: %v", err)
	}
	if n := f.interrupts(t); n != 1 {
		t.Fatalf("interrupts = %d, want 1", n)
	}
	rows := threadErrorRows(t, f.app.store, f.thread.ID, "The turn stopped, but recording the stop failed")
	if len(rows) != 1 || !strings.Contains(rows[0], "injected stop write failure") {
		t.Fatalf("error rows = %q, want one naming the failed write", rows)
	}
}

func TestStopSessionReportsAFailedBackgroundSettle(t *testing.T) {
	t.Parallel()
	f := newClaudeAgentKillFixture(t)
	f.launchAgent(t, "agent", "task-agent", "")
	execOnFile(t, f.dbPath, `CREATE TRIGGER fail_sibling BEFORE INSERT ON items WHEN NEW.completion_of = 'agent' BEGIN SELECT RAISE(ABORT, 'injected sibling write failure'); END`)

	if err := f.app.StopSession(f.thread.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	rows := threadErrorRows(t, f.app.store, f.thread.ID, "Background work from the ended session could not all be settled")
	if len(rows) != 1 || !strings.Contains(rows[0], "injected sibling write failure") {
		t.Fatalf("error rows = %q, want one naming the failed settle", rows)
	}
}
