package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
)

// Capture the real JSON-RPC writes while retaining the existing mock's
// response grammar. It acknowledges dispatch without emitting user echoes,
// exposing the interval between a successful write and canonical persistence.
func installReceiptCaptureSession(t *testing.T, app *App, thread store.Thread) func() []string {
	t.Helper()
	capture := filepath.Join(t.TempDir(), "requests.ndjson")
	binary := writeCodexSteerBinary(t, thread.ID+"-provider", "ok")
	script, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	loop := "while IFS= read -r line; do\n"
	quotedCapture := "'" + strings.ReplaceAll(capture, "'", "'\\''") + "'"
	script = []byte(strings.Replace(string(script), loop, loop+"    printf '%s\\n' \"$line\" >> "+quotedCapture+"\n", 1))
	if err := os.WriteFile(binary, script, 0o700); err != nil {
		t.Fatal(err)
	}
	sess, err := codex.NewSession(context.Background(), thread.ID, codex.Config{
		Binary: binary, WorkDir: thread.WorkspacePath,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	codex.SetActiveTurnIDForTest(sess, "receipt-turn")
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Codex), Token: "receipt-test", Codex: sess})
	// Registered last: workers finish before the fixture closes the session
	// or its SQLite store. Queue registration adds the worker synchronously.
	t.Cleanup(func() { app.flushDispatch.wg.Wait() })
	return func() []string {
		t.Helper()
		data, err := os.ReadFile(capture)
		if err != nil {
			t.Fatal(err)
		}
		var methods []string
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var request struct {
				Method string `json:"method"`
			}
			if err := json.Unmarshal([]byte(line), &request); err != nil {
				t.Fatal(err)
			}
			if request.Method == "turn/steer" || request.Method == "turn/start" {
				methods = append(methods, request.Method)
			}
		}
		return methods
	}
}

func TestSendReceiptDeduplicatesDeferredQueueBeforeProviderEcho(t *testing.T) {
	app, rec := newAppForFlushQueueRPC(t)
	thread := testThread("receipt-deferred")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if err := app.store.InsertTurn(store.Turn{TurnID: "receipt-turn", ThreadID: thread.ID, TurnIndex: 0, StartedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	requests := installReceiptCaptureSession(t, app, thread)
	opts := SendMessageOptions{SendID: "deferred-send"}
	first, err := app.RegisterQueueItem(context.Background(), thread.ID, "only once", opts)
	if err != nil {
		t.Fatal(err)
	}
	app.flushDispatch.wg.Wait()
	flushed := emittedQueueFlushed(rec)
	if len(flushed) != 1 || len(flushed[0].Items) != 1 || flushed[0].Items[0].QueueItemID != first.ID {
		t.Fatalf("flush acknowledgement = %+v", flushed)
	}
	if rows := durableQueueRows(t, app, thread.ID); len(rows) != 0 {
		t.Fatalf("successful dispatch retained durable queue rows: %+v", rows)
	}
	if _, found, err := app.store.FindUserTextItemBySendID(thread.ID, opts.SendID); err != nil || found {
		t.Fatalf("user history must await provider echo: found=%v err=%v", found, err)
	}
	if count := app.triage.DeferredPendingFlushItemCount(thread.ID); count != 1 {
		t.Fatalf("pending provider echoes = %d, want 1", count)
	}

	// Both public admission paths must recognize the accepted pending send,
	// even though it currently exists in neither SQLite history nor the queue.
	retriedQueue, err := app.RegisterQueueItem(context.Background(), thread.ID, "only once", opts)
	if err != nil {
		t.Fatal(err)
	}
	if retriedQueue.ID != flushed[0].Items[0].UserItemID || retriedQueue.SendID != opts.SendID {
		t.Fatalf("queue retry did not name the accepted pending row: %+v", retriedQueue)
	}
	if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "only once", opts); err != nil {
		t.Fatal(err)
	}
	app.flushDispatch.wg.Wait()
	if got := requests(); len(got) != 1 || got[0] != "turn/steer" {
		t.Fatalf("provider dispatches = %v, want exactly one steer", got)
	}
	if count := app.triage.DeferredPendingFlushItemCount(thread.ID); count != 1 {
		t.Fatalf("retry duplicated pending sends: %d", count)
	}
	if rows := durableQueueRows(t, app, thread.ID); len(rows) != 0 {
		t.Fatalf("retry created durable queue rows: %+v", rows)
	}
}

func TestSendReceiptLegacySteerRetryAfterCompletionHasNoSideEffects(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)
	thread := testThread("receipt-steer")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if err := app.store.InsertTurn(store.Turn{TurnID: "receipt-turn", ThreadID: thread.ID, TurnIndex: 0, StartedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	requests := installReceiptCaptureSession(t, app, thread)
	opts := SendMessageOptions{SendID: "legacy-steer-send"}
	first, err := app.SteerMessageWithOptions(context.Background(), thread.ID, "steer once", opts)
	if err != nil {
		t.Fatal(err)
	}
	item, found, err := app.store.FindUserTextItemBySendID(thread.ID, opts.SendID)
	if err != nil || !found {
		t.Fatalf("legacy steer lost sendId metadata: found=%v err=%v", found, err)
	}
	if err := app.store.UpdateTurnCompleted("receipt-turn", time.Now().UnixMilli(), "end_turn", "", "", ""); err != nil {
		t.Fatal(err)
	}
	// A delayed retry is the prior send, even when its obsolete runtime
	// selection differs from the thread's current setting and no turn is open.
	opts.RuntimeMode = "full-access"
	retried, err := app.SteerMessageWithOptions(context.Background(), thread.ID, "steer once", opts)
	if err != nil {
		t.Fatal(err)
	}
	if retried.ID != first.ID || retried.RuntimeMode != first.RuntimeMode {
		t.Fatalf("retry mutated thread: first=%+v retry=%+v", first, retried)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != item.ID || items[0].Meta != item.Meta {
		t.Fatalf("retry changed canonical rows: %+v", items)
	}
	if got := requests(); fmt.Sprint(got) != "[turn/steer]" {
		t.Fatalf("provider dispatches = %v, want exactly one steer", got)
	}
}

// The wrappers previously used different locks: send held the thread action
// lock and queue held its mutation lock. Both could observe no receipt before
// either persisted it. Pin the shared admission lock with ref counts, without
// sleeping and hoping to hit that interleaving.
func TestSendReceiptSerializesConcurrentDirectAndQueueAdmission(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)
	thread := testThread("receipt-concurrent")
	thread.Provider = string(provider.Codex)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if err := app.store.InsertTurn(store.Turn{TurnID: "receipt-turn", ThreadID: thread.ID, TurnIndex: 0, StartedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	requests := installReceiptCaptureSession(t, app, thread)
	opts := SendMessageOptions{ReconcileBySendID: true, SendID: "concurrent-send"}
	release := sync.OnceFunc(app.threadLocks().Lock(thread.ID))
	defer release()

	directResult := make(chan error, 1)
	go func() {
		_, err := app.SendMessageWithOptions(context.Background(), thread.ID, "once across clients", opts)
		directResult <- err
	}()
	// The direct request holds admission and is provably waiting for action.
	waitForThreadLockRefs(t, app.threadLocks(), thread.ID, 2)
	queueResult := make(chan error, 1)
	go func() {
		_, err := app.RegisterQueueItem(context.Background(), thread.ID, "once across clients", opts)
		queueResult <- err
	}()
	// The concurrent queue wrapper must wait at that same admission key,
	// before it can race the first request's receipt lookup or registration.
	waitForThreadLockRefs(t, app.sendAdmissionLocks(), sendAdmissionKey(thread.ID, opts.SendID), 2)
	release()
	for name, result := range map[string]<-chan error{"direct": directResult, "queue": queueResult} {
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("%s request: %v", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s request remained blocked after admission release", name)
		}
	}
	app.flushDispatch.wg.Wait()
	if got := requests(); len(got) != 1 || got[0] != "turn/steer" {
		t.Fatalf("simultaneous requests dispatched %v, want one steer", got)
	}
	if pending := app.triage.DeferredPendingFlushItemCount(thread.ID); pending != 1 {
		t.Fatalf("simultaneous requests accepted %d sends, want one", pending)
	}
	if refs := app.sendAdmissionLocks().Refs(sendAdmissionKey(thread.ID, opts.SendID)); refs != 0 {
		t.Fatalf("admission key retained %d references", refs)
	}
}

func TestSendReceiptWorkflowRetryDoesNotPrepareTakeover(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("receipt-workflow")
	thread.Mode = threadmode.ModeWorkflow
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	insertUserItemWithMeta(t, app.store, thread.ID, "user:accepted", 0, "already accepted", `{"sendId":"workflow-retry"}`)
	before, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	// No workflow engine or provider exists in this fixture. Preparing a
	// takeover before recognizing the accepted receipt would fail here; in a
	// running app the same mistake could interrupt a different workflow phase.
	result, err := app.SendMessageWithOptions(context.Background(), thread.ID, "already accepted", SendMessageOptions{SendID: "workflow-retry"})
	if err != nil {
		t.Fatalf("accepted retry attempted workflow takeover: %v", err)
	}
	if result.ID != thread.ID || result.Mode != threadmode.ModeWorkflow {
		t.Fatalf("retry changed workflow thread: %+v", result)
	}
	after, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || len(before) != 1 || after[0] != before[0] {
		t.Fatalf("retry changed accepted history: before=%+v after=%+v", before, after)
	}
	if _, exists := app.sessionManager().get(thread.ID); exists {
		t.Fatal("accepted retry started a provider session")
	}
}
