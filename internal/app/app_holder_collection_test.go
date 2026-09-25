package app

import (
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/triage"
)

// seedHeldSource creates the pending-delete source with forks pointer
// forks of it, deletes it through the app, and returns its id and the
// forks'. The delete keeps the source as the holder the forks read.
func seedHeldSource(t *testing.T, app *App, forks int) (string, []string) {
	t.Helper()
	id := seedPendingDeleteSource(t, app, provider.Codex)
	source, err := app.store.GetThread(id)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, forks)
	for i := range ids {
		fork := store.BuildForkedThread(source)
		if err := app.store.CreatePointerFork(fork, id, store.ForkCut{}, func(s string) string { return s }, 1); err != nil {
			t.Fatal(err)
		}
		ids[i] = fork.ID
	}
	if err := app.DeleteThread(id); err != nil {
		t.Fatal(err)
	}
	if held, err := app.store.GetThread(id); err != nil || held.Mode != threadmode.ModeHolder {
		t.Fatalf("the deleted source = %+v, %v; want the holder its forks read", held, err)
	}
	for _, fork := range ids {
		if items, err := app.store.ListItems(fork); err != nil || len(items) != 600 {
			t.Fatalf("fork %s reads %d rows, %v; want the source's 600", fork, len(items), err)
		}
	}
	return id, ids
}

// requireHolderGone waits for the delete of holder and checks it took the
// rows with it.
func requireHolderGone(t *testing.T, app *App, holder string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; {
		_, err := app.store.GetThread(holder)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the released holder was never deleted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if items, err := app.store.ListItems(holder); err != nil || len(items) != 0 {
		t.Fatalf("the deleted holder left %d rows, %v", len(items), err)
	}
}

// TestHolderIsDeletedWhenItsLastForkGoes: a running app deletes a holder
// once the last fork that reads it is deleted, and not before. Clients
// were told the source was deleted when it became the holder; the
// holder's own delete tells them nothing.
func TestHolderIsDeletedWhenItsLastForkGoes(t *testing.T) {
	app := newTestAppWithStore(t)
	app.maintenance.chunkPause = time.Millisecond
	var mu sync.Mutex
	deleted := map[string]int{}
	app.emitEventFn = func(name string, data any) {
		if event, ok := data.(triage.ThreadUpdateEvent); ok && name == "thread:updated" && event.Action == triage.ThreadActionDeleted {
			mu.Lock()
			deleted[event.ID]++
			mu.Unlock()
		}
	}
	t.Cleanup(func() {
		app.appCancel()
		app.waitPendingThreadDeletes()
	})
	app.startPendingThreadDeletes()
	holder, forks := seedHeldSource(t, app, 2)

	if err := app.DeleteThread(forks[0]); err != nil {
		t.Fatal(err)
	}
	// The fork's delete released no holder: the walk it started finds
	// none and ends.
	for deadline := time.Now().Add(10 * time.Second); ; {
		p := &app.pendingThreadDeletes
		p.mu.Lock()
		collecting := p.collecting
		p.mu.Unlock()
		if !collecting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the walk over released holders never ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if held, err := app.store.GetThread(holder); err != nil || held.Mode != threadmode.ModeHolder {
		t.Fatalf("with a fork still reading it the holder = %+v, %v", held, err)
	}
	if items, err := app.store.ListItems(forks[1]); err != nil || len(items) != 600 {
		t.Fatalf("the remaining fork reads %d rows, %v", len(items), err)
	}

	if err := app.DeleteThread(forks[1]); err != nil {
		t.Fatal(err)
	}
	requireHolderGone(t, app, holder)
	mu.Lock()
	defer mu.Unlock()
	if deleted[holder] != 1 || deleted[forks[0]] != 1 || deleted[forks[1]] != 1 || len(deleted) != 3 {
		t.Fatalf("deleted broadcasts = %v, want one each for the source %s and its two forks", deleted, holder)
	}
}

// TestReleasedHolderIsDeletedAtBoot: a holder released while no walk
// could delete it, as a crash leaves it, is gone to every client and the
// next boot deletes it once the first client has read its catalogs.
func TestReleasedHolderIsDeletedAtBoot(t *testing.T) {
	app, path := newTestAppWithStorePath(t)
	holder, forks := seedHeldSource(t, app, 1)
	if err := app.DeleteThread(forks[0]); err != nil {
		t.Fatal(err)
	}
	if released, err := app.store.ListReleasedHolders(); err != nil || len(released) != 1 || released[0] != holder {
		t.Fatalf("released holders = %v, %v; want %s", released, err, holder)
	}
	requirePendingDelete(t, app, holder, true)
	app.appCancel()
	if err := app.store.Close(); err != nil {
		t.Fatal(err)
	}
	requireBootCompletesDelete(t, path, holder)
}
