package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// seedPendingDeleteSource creates a 600-row thread of the provider that
// drains in two chunks and answers "bilby" by title.
func seedPendingDeleteSource(t *testing.T, app *App, name provider.ProviderKind) string {
	t.Helper()
	source := testThread("pending-delete")
	source.Title = "bilby source"
	source.Provider = string(name)
	source.SessionRef = "provider-session"
	if err := app.store.CreateThread(source); err != nil {
		t.Fatal(err)
	}
	for i := range 600 {
		kind, role := "assistant_text", "assistant"
		if i%10 == 0 {
			kind, role = "user_text", "user"
		}
		if err := app.store.InsertItem(store.Item{
			ID: fmt.Sprintf("r%d", i), ThreadID: source.ID, TurnIndex: i / 10, ItemIndex: i % 10,
			Kind: kind, Role: role, Status: "completed", Summary: "row",
		}); err != nil {
			t.Fatal(err)
		}
	}
	requirePendingDelete(t, app, source.ID, false)
	return source.ID
}

// requirePendingDelete checks what a client sees of a thread whose delete
// began and did not finish: no sidebar row, no read, no search hit, and a
// fork of either kind refused with the public sentence. With pending false
// it checks the thread is an ordinary one.
func requirePendingDelete(t *testing.T, app *App, id string, pending bool) {
	t.Helper()
	threads, err := app.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, thread := range threads {
		listed = listed || thread.ID == id
	}
	if listed == pending {
		t.Fatalf("ListThreads lists %s: %v, pending %v", id, listed, pending)
	}
	if _, err := app.GetThread(id); errors.Is(err, sql.ErrNoRows) != pending {
		t.Fatalf("GetThread(%s) = %v, pending %v", id, err, pending)
	}
	hits, err := app.SearchThreadMessages("bilby", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, hit := range hits {
		found = found || hit.ThreadID == id
	}
	if found == pending {
		t.Fatalf("SearchThreadMessages finds %s: %v, pending %v", id, found, pending)
	}
	queued, err := app.store.ListPendingThreadDeletes()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(queued) == 1 && queued[0] == id; got != pending {
		t.Fatalf("ListPendingThreadDeletes = %v, pending %v", queued, pending)
	}
	if !pending {
		return
	}
	_, forkErr := app.ForkThread(context.Background(), id, nil)
	_, messageErr := app.ForkThreadFromMessage(context.Background(), id, "r550")
	for op, err := range map[string]error{"ForkThread": forkErr, "ForkThreadFromMessage": messageErr} {
		if !errors.Is(err, store.ErrForkSourceDeleted) {
			t.Fatalf("%s of a pending delete = %v, want store.ErrForkSourceDeleted", op, err)
		}
		code, message, public := errorsx.PublicDetails(err)
		if !public || code != "fork_source_deleted" || message != "This thread was deleted, so it cannot be forked." {
			t.Fatalf("%s error %q shows code=%q message=%q public=%v", op, err, code, message, public)
		}
	}
	all, err := app.store.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("threads after the refused forks = %+v, want none", all)
	}
}

// requireBootCompletesDelete reopens the database at path as a boot does
// and checks the pending delete of id waits for the first catalog reads,
// then completes.
func requireBootCompletesDelete(t *testing.T, path, id string) {
	t.Helper()
	app := newTestAppAtStorePath(t, path)
	app.maintenance.firstReadsFallback = time.Hour
	app.maintenance.chunkPause = time.Millisecond
	t.Cleanup(func() {
		app.appCancel()
		app.waitPendingThreadDeletes()
	})
	app.startPendingThreadDeletes()
	for until := time.Now().Add(300 * time.Millisecond); time.Now().Before(until); {
		if _, err := app.store.GetThread(id); err != nil {
			t.Fatalf("the delete ran before the first catalog reads: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	requirePendingDelete(t, app, id, true)
	// The walk deletes under the thread's action lock.
	unlock := app.threadLocks().Lock(id)
	if _, err := app.ListProjects(); err != nil {
		t.Fatal(err)
	}
	for until := time.Now().Add(300 * time.Millisecond); time.Now().Before(until); {
		if _, err := app.store.GetThread(id); err != nil {
			t.Fatalf("the delete ran without the thread's action lock: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	unlock()
	for deadline := time.Now().Add(10 * time.Second); ; {
		_, err := app.store.GetThread(id)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the boot never completed the pending delete")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if queued, err := app.store.ListPendingThreadDeletes(); err != nil || len(queued) != 0 {
		t.Fatalf("pending after boot = %v, err %v", queued, err)
	}
}

// interruptDelete runs the app's delete of id and stops it at its first
// pause, as a crash would.
func interruptDelete(t *testing.T, app *App, id string) {
	t.Helper()
	stopped := make(chan bool, 1)
	go func() {
		returned := false
		defer func() { stopped <- returned }()
		unlock := app.threadLocks().Lock(id)
		defer unlock()
		_ = app.deleteThreadTreePacedLocked(id, func() { runtime.Goexit() })
		returned = true
	}()
	if <-stopped {
		t.Fatal("the delete returned instead of stopping at its first pause")
	}
}

// TestInterruptedThreadDeleteIsGoneAndBootCompletesIt: an app delete
// stopped after its first chunk, as a crash stops it, leaves a thread no
// client lists, reads, searches or forks, and the next boot completes it
// once the first client has read its catalogs.
// A Claude fork reads the source's transcript before the store is asked,
// so the app refuses a pending delete before either provider's fork work.
func TestInterruptedThreadDeleteIsGoneAndBootCompletesIt(t *testing.T) {
	for _, name := range []provider.ProviderKind{provider.Codex, provider.Claude} {
		t.Run(string(name), func(t *testing.T) {
			app, path := newTestAppWithStorePath(t)
			id := seedPendingDeleteSource(t, app, name)
			interruptDelete(t, app, id)
			requirePendingDelete(t, app, id, true)
			app.appCancel()
			if err := app.store.Close(); err != nil {
				t.Fatal(err)
			}
			requireBootCompletesDelete(t, path, id)
		})
	}
}

// TestFailedThreadDeleteIsGoneAndBootCompletesIt: an app delete whose
// second chunk fails returns the error and leaves the same state as one a
// crash stopped.
func TestFailedThreadDeleteIsGoneAndBootCompletesIt(t *testing.T) {
	app, path := newTestAppWithStorePath(t)
	id := seedPendingDeleteSource(t, app, provider.Codex)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// The first chunk leaves 100 of the 600 rows.
	if _, err := raw.Exec(`CREATE TRIGGER fail_second_chunk BEFORE DELETE ON items
		WHEN OLD.thread_id = '` + id + `' AND (SELECT COUNT(*) FROM items WHERE thread_id = '` + id + `') <= 100
		BEGIN SELECT RAISE(ABORT, 'injected chunk failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := app.DeleteThread(id); err == nil || !strings.Contains(err.Error(), "injected chunk failure") {
		t.Fatalf("DeleteThread = %v, want the injected failure", err)
	}
	if _, err := raw.Exec(`DROP TRIGGER fail_second_chunk`); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM items WHERE thread_id = ?`, id).Scan(&rows); err != nil || rows != 100 {
		t.Fatalf("rows after the failed delete = %d, err %v; want the 100 the first chunk left", rows, err)
	}
	requirePendingDelete(t, app, id, true)
	app.appCancel()
	if err := app.store.Close(); err != nil {
		t.Fatal(err)
	}
	requireBootCompletesDelete(t, path, id)
}

// The join ends a walk still waiting for its first reads once the app
// context ends, and waits for a walk that is deleting.
func TestPendingThreadDeletesJoin(t *testing.T) {
	joined := func(app *App) <-chan struct{} {
		done := make(chan struct{})
		go func() {
			app.waitPendingThreadDeletes()
			close(done)
		}()
		return done
	}
	t.Run("waiting for the first reads", func(t *testing.T) {
		app := newTestAppWithStore(t)
		app.maintenance.firstReadsFallback = time.Hour
		app.startPendingThreadDeletes()
		app.appCancel()
		select {
		case <-joined(app):
		case <-time.After(10 * time.Second):
			t.Fatal("the join did not return after the app context ended")
		}
	})
	t.Run("deleting", func(t *testing.T) {
		app := newTestAppWithStore(t)
		id := seedPendingDeleteSource(t, app, provider.Codex)
		interruptDelete(t, app, id)
		unlock := app.threadLocks().Lock(id)
		app.startPendingThreadDeletes()
		for _, read := range []func() error{
			func() error { _, err := app.ListThreads(); return err },
			func() error { _, err := app.ListProjects(); return err },
		} {
			if err := read(); err != nil {
				t.Fatal(err)
			}
		}
		done := joined(app)
		select {
		case <-done:
			t.Fatal("the join returned while the walk waited to delete")
		case <-time.After(300 * time.Millisecond):
		}
		unlock()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the join did not return after the delete")
		}
		if _, err := app.store.GetThread(id); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("the walk the join waited for left %s: %v", id, err)
		}
	})
}
