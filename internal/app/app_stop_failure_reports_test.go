package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/kerneltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claudetui"
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

// claude-tui reverts natively on the Esc the un-send sends. When the Esc
// cannot be delivered the TUI keeps the turn, so AO keeps its copy too and
// reports the failure.
func TestInterruptAndRevertIfCleanClaudeTUIKeepsTheMessageWhenTheEscFails(t *testing.T) {
	kerneltest.DetachHome(t)
	app := newTestApp(t)
	var completions []triage.TurnCompletedEvent
	app.triage = triage.NewRouter(app.store, app.emit)
	app.testEmitHook = func(name string, data any) {
		if evt, ok := data.(triage.TurnCompletedEvent); ok && name == "provider:turn_completed" {
			completions = append(completions, evt)
		}
	}
	dir := t.TempDir()
	thread := createAppTestThread(t, app, "revert-tui-esc-fails", "claude-tui", dir)
	insertUserItem(t, app.store, thread.ID, "u:0", 0, "the original prompt")
	turnEvent := func(kind provider.EventKind, complete *provider.WireTurnCompleteMeta) {
		t.Helper()
		if err := app.triage.Handle(provider.ProviderEvent{Kind: kind, ThreadID: thread.ID, TurnID: "turn-0", TurnComplete: complete, Timestamp: time.Now()}); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
	}
	turnEvent(provider.EventTurnStart, nil)

	binary := filepath.Join(dir, "mock-claude-tui")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nwhile IFS= read -r _; do :; done\n"), 0o755); err != nil {
		t.Fatalf("write PTY stand-in: %v", err)
	}
	sess, err := claudetui.NewSession(context.Background(), thread.ID, claudetui.Config{
		Binary:  binary,
		WorkDir: dir,
		Env:     []string{"HOME=" + dir, "PATH=" + os.Getenv("PATH")},
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("claudetui.NewSession: %v", err)
	}
	// A closed session has no terminal to write the Esc to.
	if err := sess.Close(); err != nil {
		t.Fatalf("close TUI session: %v", err)
	}
	app.sessionManager().put(thread.ID, session{Provider: string(provider.ClaudeTUI), Token: "tui", ClaudeTUI: sess})

	result, err := app.InterruptAndRevertIfClean(thread.ID, InterruptRevertOptions{}, nil)
	if err == nil || !strings.Contains(err.Error(), "provider interrupt") {
		t.Fatalf("InterruptAndRevertIfClean = %+v, %v; want the failed interrupt", result, err)
	}
	if result.Reverted {
		t.Fatal("the un-send reverted a turn the TUI kept")
	}
	if _, found, err := app.store.GetThreadItem(thread.ID, "u:0"); err != nil || !found {
		t.Fatalf("the message must stay: found=%v err=%v", found, err)
	}
	if _, ok, err := app.store.GetThreadDraft(thread.ID); err != nil || ok {
		t.Fatalf("the composer draft was restored for a message that stays: ok=%v err=%v", ok, err)
	}
	// The turn the TUI kept ends as a kept message, not a reverted one.
	turnEvent(provider.EventTurnComplete, &provider.WireTurnCompleteMeta{StopReason: "end_turn"})
	if len(completions) != 1 || completions[0].RevertedUserMessage {
		t.Fatalf("completions = %+v, want one completion not marked reverted", completions)
	}
}
