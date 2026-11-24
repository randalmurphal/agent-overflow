package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil/mockexec"
)

// writeClaudeStdinCaptureBinary is a fake CLI that appends every line AO
// writes to its stdin to capturePath, in order, and answers nothing.
func writeClaudeStdinCaptureBinary(t *testing.T, capturePath string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-capture.sh")
	mockexec.Write(t, path, "#!/bin/sh\ncat >> '"+capturePath+"'\n")
	return path
}

// noSessionQueueThread is a Claude thread with no session and a fake CLI
// that records what it is sent.
func noSessionQueueThread(t *testing.T, app *App, name string) (store.Thread, string) {
	t.Helper()
	thread := testThread(name)
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = initGitRepo(t)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	capture := filepath.Join(t.TempDir(), "stdin.jsonl")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeClaudeStdinCaptureBinary(t, capture)}); err != nil {
		t.Fatalf("set binary: %v", err)
	}
	t.Cleanup(func() { stopSessionChecked(t, app, thread.ID) })
	return thread, capture
}

// The session start a queued message makes runs on the dispatch worker. The
// fixture joins that worker when its test ends, as Shutdown does, so the
// start never runs against a test whose directories are gone.
func TestTheFixtureJoinsTheDispatchWorkerAtCleanup(t *testing.T) {
	t.Parallel()
	var finished atomic.Bool
	t.Run("queue", func(t *testing.T) {
		app := newTestAppWithStore(t)
		thread := testThread("joined-dispatch")
		if err := app.store.CreateThread(thread); err != nil {
			t.Fatal(err)
		}
		started := make(chan struct{})
		app.startSessionFn = func(string) error {
			close(started)
			// Long enough that an unjoined worker is still here when the
			// subtest's cleanups have run.
			time.Sleep(300 * time.Millisecond)
			finished.Store(true)
			return nil
		}
		if _, err := app.RegisterQueueItem(context.Background(), thread.ID, "start me", SendMessageOptions{}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-started:
		case <-time.After(30 * time.Second):
			t.Fatal("the dispatch worker never started the session")
		}
	})
	if !finished.Load() {
		t.Fatal("the dispatch worker was still starting a session after its test's cleanups ran")
	}
}

// holdFlushQueue keeps queued messages in triage's queue, where a test stages
// them: with no dispatcher, a register does not hand them to the dispatch
// worker, which would start the thread's session.
func holdFlushQueue(app *App) { app.triage.SetFlushDispatcher(nil) }

// waitForStdinTexts waits until the fake CLI has been sent every text and
// returns the order the first line naming each one arrived in.
func waitForStdinTexts(t *testing.T, capture string, texts ...string) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw, _ := os.ReadFile(capture)
		var order []string
		for _, line := range strings.Split(string(raw), "\n") {
			for _, text := range texts {
				if strings.Contains(line, text) {
					order = append(order, text)
				}
			}
		}
		if len(order) >= len(texts) {
			return order
		}
		if time.Now().After(deadline) {
			t.Fatalf("the CLI was sent %q, want all of %q", order, texts)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A message queued while its thread has no session is delivered: the queue
// starts the session, as a direct send does, rather than waiting for some
// later send to start one.
func TestMessageQueuedWithNoSessionIsDispatched(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	thread, capture := noSessionQueueThread(t, app, "queue-no-session")

	if _, err := app.RegisterQueueItem(context.Background(), thread.ID, "queued with no session", SendMessageOptions{SendID: "send-lone"}); err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}

	waitForStdinTexts(t, capture, "queued with no session")
	waitUntil(t, 10*time.Second, func() bool { return len(durableQueueRows(t, app, thread.ID)) == 0 })
	if !app.hasActiveSession(thread.ID) {
		t.Fatal("no session is running the message")
	}
}

// A message queued with no session goes out before a message the person
// sends after it, which waits behind it in the queue.
func TestLaterSendWaitsBehindAMessageQueuedWithNoSession(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	thread, capture := noSessionQueueThread(t, app, "queue-no-session-order")
	// The thread lock keeps the queued message from being dispatched
	// before the later send is admitted: the later send must see it.
	unlock := app.threadLocks().Lock(thread.ID)
	if _, err := app.RegisterQueueItem(context.Background(), thread.ID, "first, queued", SendMessageOptions{SendID: "send-first"}); err != nil {
		unlock()
		t.Fatalf("RegisterQueueItem: %v", err)
	}
	unlock()

	if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "second, sent", SendMessageOptions{
		SendID: "send-second", ReconcileBySendID: true, ConsumeDraft: &DraftSnapshot{},
	}); err != nil {
		t.Fatalf("SendMessageWithOptions: %v", err)
	}

	order := waitForStdinTexts(t, capture, "first, queued", "second, sent")
	if order[0] != "first, queued" {
		t.Fatalf("the CLI was sent %q, want the queued message first", order)
	}
}

// A message queued with no session whose session cannot start goes back
// to the composer, with the reason on the thread.
func TestMessageQueuedWithNoSessionReturnsToTheComposerWhenTheStartFails(t *testing.T) {
	t.Parallel()
	app, _ := newAppForFlushQueueRPC(t)
	thread := testThread("queue-no-session-start-fails")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": filepath.Join(t.TempDir(), "no-such-claude")}); err != nil {
		t.Fatalf("set binary: %v", err)
	}

	if _, err := app.RegisterQueueItem(context.Background(), thread.ID, "cannot be sent yet", SendMessageOptions{SendID: "send-stuck", ConsumeDraft: &DraftSnapshot{}}); err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}

	waitUntil(t, 10*time.Second, func() bool {
		draft, _, err := app.store.GetThreadDraft(thread.ID)
		return err == nil && draft.Content == "cannot be sent yet"
	})
	if rows := durableQueueRows(t, app, thread.ID); len(rows) != 0 {
		t.Fatalf("durable rows = %+v, want none once the composer has the message", rows)
	}
	if state, err := app.GetQueueState(thread.ID); err != nil || len(state) != 0 {
		t.Fatalf("queue = %+v (%v), want it empty", state, err)
	}
	// The reason follows the restore.
	waitUntil(t, 10*time.Second, func() bool {
		items, err := app.store.ListItems(thread.ID)
		if err != nil {
			t.Fatalf("ListItems: %v", err)
		}
		for _, item := range items {
			if item.Kind == "error" && strings.Contains(item.Summary, "back in the composer") {
				return true
			}
		}
		return false
	})
}
