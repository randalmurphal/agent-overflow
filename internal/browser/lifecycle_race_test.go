package browser

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// A close that lands while a failing creation settles removes the tab for
// good: the creation never keeps a closed tab's entry, which no state shows
// and the thread's cap would count forever. The close runs as the creator
// releases startMu, between the engine's answer and the creation's
// settlement.
func TestACloseRacingAFailedCreationLeavesNoHiddenTab(t *testing.T) {
	manager, engine := newLifecycleManager(t)
	engine.newPageErr = errors.New("engine refused the page")
	workspace := t.TempDir()
	for i := range 100 {
		access := manager.Bind(Caller{ThreadID: "thread", Workspace: workspace})
		release := engine.hold(&engine.newPageGate)
		created := async(func() error {
			_, err := manager.NewCompanionPage(context.Background(), access, "")
			return err
		})
		receive(t, engine.creating, "the engine page's creation")
		id := tabIDs(manager, access)[0]
		closed := async(func() bool {
			// The creator holds startMu until the engine has answered; the
			// close runs the moment it lets go.
			for !manager.startMu.TryLock() {
			}
			manager.startMu.Unlock()
			_, ok := closePendingTab(manager, access, id)
			return ok
		})
		release()
		if err := await(t, created, "the creation"); err != nil {
			t.Fatalf("iteration %d: creation = %v", i, err)
		}
		await(t, closed, "the close")
		// The tab is closed whether the close found it creating or failed.
		if got := tabIDs(manager, access); len(got) != 0 {
			if _, ok := closePendingTab(manager, access, got[0]); !ok {
				t.Fatalf("iteration %d: tabs %v that no close finds", i, got)
			}
		}
		manager.mu.Lock()
		pending := len(manager.pending)
		manager.mu.Unlock()
		if pending != 0 {
			t.Fatalf("iteration %d: %d pending entries hold the thread's cap after its tab closed", i, pending)
		}
	}
}

// A close of a tab whose creation becomes a live page as the close runs
// finds the tab in one state or the other: it never answers that the page
// is not found, and the tab is gone.
func TestACloseRacingACreationsPromotionFindsTheTab(t *testing.T) {
	manager, engine := newLifecycleManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	// A page that stays keeps the profile across the iterations.
	mustOpen(t, manager, access, "https://example.test/keep")
	for i := range 2000 {
		release := engine.hold(&engine.newPageGate)
		created := async(func() error {
			_, err := manager.NewCompanionPage(context.Background(), access, "")
			return err
		})
		receive(t, engine.creating, "the engine page's creation")
		id := tabIDs(manager, access)[1]
		start := make(chan struct{})
		closed := async(func() error {
			<-start
			return manager.closePage(context.Background(), access, id, false, false)
		})
		close(start)
		release()
		if err := await(t, created, "the creation"); err != nil && !errors.Is(err, errClosedWhileCreating) {
			t.Fatalf("iteration %d: creation = %v", i, err)
		}
		if err := await(t, closed, "the close"); err != nil {
			t.Fatalf("iteration %d: the close of a tab that existed throughout answered %v", i, err)
		}
		if got := tabIDs(manager, access); len(got) != 1 {
			t.Fatalf("iteration %d: tabs %v after the close", i, got)
		}
	}
}

// A close of a suspended tab whose restore makes it live as the close runs
// finds it as the record or as the page.
func TestACloseRacingARestoreFindsTheTab(t *testing.T) {
	manager, engine := newLifecycleManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	mustOpen(t, manager, access, "https://example.test/keep")
	for i := range 1000 {
		page := mustOpen(t, manager, access, "https://example.test/")
		live, _, err := manager.lookupOwnedPage(access, page.ID)
		if err != nil {
			t.Fatal(err)
		}
		if moved, err := manager.suspendPage(context.Background(), live); err != nil || !moved {
			t.Fatalf("iteration %d: suspend: %v", i, err)
		}
		release := engine.hold(&engine.newPageGate)
		restored := async(func() error {
			_, _, err := manager.resolvePage(context.Background(), access, page.ID, false)
			return err
		})
		receive(t, engine.creating, "the restore's engine page")
		start := make(chan struct{})
		closed := async(func() error {
			<-start
			return manager.closePage(context.Background(), access, page.ID, false, false)
		})
		close(start)
		release()
		// The restore succeeds or finds its record forgotten; either is right.
		_ = await(t, restored, "the restore")
		if err := await(t, closed, "the close"); err != nil {
			t.Fatalf("iteration %d: the close of a tab that existed throughout answered %v", i, err)
		}
		if got := tabIDs(manager, access); len(got) != 1 {
			t.Fatalf("iteration %d: tabs %v after the close", i, got)
		}
	}
}

// Two closes of one live page that run at once both join its one teardown;
// neither answers that the page is not found.
func TestConcurrentClosesJoinOneTeardown(t *testing.T) {
	manager, engine := newLifecycleManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	// A page that stays keeps the profile across the iterations.
	mustOpen(t, manager, access, "https://example.test/keep")
	for i := range 200 {
		page := mustOpen(t, manager, access, "https://example.test/")
		// The teardown is held, so both closes run while it is in flight.
		release := engine.hold(&engine.closeGate)
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for j := range errs {
			wg.Go(func() {
				<-start
				errs[j] = manager.CloseCompanionPage(context.Background(), access, page.ID)
			})
		}
		close(start)
		wg.Wait()
		receive(t, engine.closing, "the engine close")
		waited := closeWaiter(t, manager, access, page.ID)
		release()
		for j, err := range errs {
			if err != nil {
				t.Fatalf("iteration %d: close %d = %v", i, j, err)
			}
		}
		if err := await(t, waited, "the teardown"); err != nil {
			t.Fatalf("iteration %d: teardown = %v", i, err)
		}
	}
	if got := engine.closed.Load(); got != 200 {
		t.Fatalf("the engine closed %d pages, want 200", got)
	}
}
