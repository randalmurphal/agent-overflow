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
		access := Access{ThreadID: "thread", Workspace: workspace}
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
			_, ok := manager.closePending(access, id)
			return ok
		})
		release()
		if err := await(t, created, "the creation"); err != nil {
			t.Fatalf("iteration %d: creation = %v", i, err)
		}
		await(t, closed, "the close")
		// The tab is closed whether the close found it creating or failed.
		if got := tabIDs(manager, access); len(got) != 0 {
			if _, ok := manager.closePending(access, got[0]); !ok {
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

// A close that found a page live before another close moved it to closing
// joins that close instead of answering that the page is not found.
func TestACloseThatFoundThePageLiveJoinsTheCloseThatWon(t *testing.T) {
	manager, engine := newLifecycleManager(t)
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	page := mustOpen(t, manager, access, "https://example.test/")
	release := engine.hold(&engine.closeGate)
	defer release()
	p, scope, err := manager.lookupOwnedPage(access, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.CloseCompanionPage(t.Context(), access, page.ID); err != nil {
		t.Fatal(err)
	}
	won, err := manager.beginCloseLive(access, page.ID)
	if err != nil {
		t.Fatalf("the running close: %v", err)
	}
	joined, err := manager.closeFoundPage(p, scope)
	if err != nil || joined != won {
		t.Fatalf("the losing close = %p, %v, want the running close %p", joined, err, won)
	}
	release()
	if err := await(t, async(func() error { return joined.wait(context.Background()) }), "the teardown"); err != nil {
		t.Fatal(err)
	}
	if got := engine.closed.Load(); got != 1 {
		t.Fatalf("the engine closed the page %d times", got)
	}
}

// Two closes of one live page that run at once both join its one teardown;
// neither answers that the page is not found.
func TestConcurrentClosesJoinOneTeardown(t *testing.T) {
	manager, engine := newLifecycleManager(t)
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
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
