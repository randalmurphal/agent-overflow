package browser

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
)

// A tab the user opens appears at once, creating, selected and shown, before
// the engine page exists, and turns ready when it does; from no tab at all,
// the companion is shown with it.
func TestANewTabShowsCreatingThenReady(t *testing.T) {
	manager, engine := newLifecycleManager(t)
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	release := engine.hold(&engine.newPageGate)
	defer release()
	type result struct {
		info PageInfo
		err  error
	}
	created := async(func() result {
		info, err := manager.NewCompanionPage(context.Background(), access, "")
		return result{info, err}
	})
	receive(t, engine.creating, "the engine page's creation")
	state := manager.CompanionState(access)
	if len(state.Pages) != 1 || !state.Pages[0].Creating {
		t.Fatalf("tabs while the page is created = %+v, want one creating tab", state.Pages)
	}
	id := state.Pages[0].ID
	if state.ActivePageID != id || (state.Visible == nil || !*state.Visible) {
		t.Fatalf("while created: active %q visible %v, want the new tab shown", state.ActivePageID, state.Visible != nil && *state.Visible)
	}
	release()
	got := await(t, created, "the creation")
	if got.err != nil || got.info.ID != id {
		t.Fatalf("new = %+v, %v, want page %s", got.info, got.err, id)
	}
	state = manager.CompanionState(access)
	if len(state.Pages) != 1 || state.Pages[0].Creating || state.ActivePageID != id || (state.Visible == nil || !*state.Visible) {
		t.Fatalf("after the creation: %+v", state)
	}
}

// A creation the user asked for that fails stays as a tab showing why until
// it is closed; an agent's failed creation leaves no tab.
func TestAFailedCreationShowsOnTheUsersTabOnly(t *testing.T) {
	manager, engine := newLifecycleManager(t)
	engine.newPageErr = errors.New("engine refused the page")
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}

	info, err := manager.NewCompanionPage(t.Context(), access, "")
	if err != nil || info.ID != "" {
		t.Fatalf("a failed user creation = %+v, %v, want no page and no error", info, err)
	}
	state := manager.CompanionState(access)
	if len(state.Pages) != 1 || !strings.Contains(state.Pages[0].Error, "engine refused") || state.Pages[0].Creating {
		t.Fatalf("tabs after the failure = %+v, want one tab with the error", state.Pages)
	}
	failed := state.Pages[0].ID
	if state.ActivePageID != failed {
		t.Fatalf("active %q, want the failed tab", state.ActivePageID)
	}
	if err := manager.ActivateCompanionPage(t.Context(), access, failed); err != nil {
		t.Fatalf("select the failed tab: %v", err)
	}
	if err := manager.CloseCompanionPage(t.Context(), access, failed); err != nil {
		t.Fatalf("close the failed tab: %v", err)
	}
	if got := tabIDs(manager, access); len(got) != 0 {
		t.Fatalf("tabs after closing the failed one = %v", got)
	}

	if _, err := manager.Open(t.Context(), access, "https://example.test/", OpenOptions{}); !errors.Is(err, engine.newPageErr) {
		t.Fatalf("agent open = %v, want the refusal", err)
	}
	if got := tabIDs(manager, access); len(got) != 0 {
		t.Fatalf("an agent's failed creation left tabs %v", got)
	}
	manager.mu.Lock()
	pending := len(manager.pending)
	manager.mu.Unlock()
	if pending != 0 || registeredScopes(manager) != 0 {
		t.Fatalf("%d pending entries and %d scopes after the failures", pending, registeredScopes(manager))
	}
}

// A tab closed while its page is created leaves at once; an engine page that
// arrives after is closed, the profile it alone held is disposed, and the
// creation answers no page and no error. A failure of that cleanup, after
// the engine page arrived or after the creation failed, is reported and
// answers a close that waits; the creation's own cancellation is not.
func TestClosingATabWhileItIsCreated(t *testing.T) {
	for _, tc := range []struct {
		name               string
		createFails        bool
		disposeFails       bool
		wantClosedByEngine int64
	}{
		{"page arrives", false, false, 1},
		{"page arrives, profile will not stop", false, true, 1},
		{"creation fails", true, false, 0},
		{"creation fails, profile will not stop", true, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, engine := newLifecycleManager(t)
			if tc.createFails {
				engine.newPageErr = errors.New("engine refused the page")
			}
			if tc.disposeFails {
				engine.disposeErr = errors.New("the profile's browser would not stop")
			}
			events := recordEvents(manager)
			access := Access{ThreadID: "thread", Workspace: t.TempDir()}
			release := engine.hold(&engine.newPageGate)
			defer release()
			type result struct {
				info PageInfo
				err  error
			}
			created := async(func() result {
				info, err := manager.NewCompanionPage(context.Background(), access, "")
				return result{info, err}
			})
			receive(t, engine.creating, "the engine page's creation")
			id := tabIDs(manager, access)[0]
			if err := manager.CloseCompanionPage(t.Context(), access, id); err != nil {
				t.Fatalf("close a creating tab: %v", err)
			}
			if got := tabIDs(manager, access); len(got) != 0 {
				t.Fatalf("tabs right after closing the creating one = %v", got)
			}
			// A second close joins the first; it ends when the creation does.
			pending, ok := manager.closePending(access, id)
			if !ok {
				t.Fatal("a second close does not find the creating tab")
			}
			waited := async(func() error { return pending.wait(context.Background()) })
			release()
			if got := await(t, created, "the creation"); got.err != nil || got.info.ID != "" {
				t.Fatalf("creation of a closed tab = %+v, %v, want no page and no error", got.info, got.err)
			}
			err := await(t, waited, "the waiting close")
			if (tc.disposeFails && !errors.Is(err, engine.disposeErr)) || (!tc.disposeFails && err != nil) {
				t.Fatalf("waiting close = %v, want the cleanup failure: %v", err, tc.disposeFails)
			}
			if tc.disposeFails {
				event := events.waitFor(t, "the teardown failure", func(e CompanionEvent) bool { return e.Kind == "teardown" })
				if event.PageID != id || !strings.Contains(event.Error, "would not stop") {
					t.Fatalf("teardown event = %+v", event)
				}
			}
			for len(events.ch) > 0 {
				if event := <-events.ch; event.Kind == "teardown" && !tc.disposeFails {
					t.Fatalf("a teardown report for a clean cleanup: %+v", event)
				}
			}
			if got := engine.closed.Load(); got != tc.wantClosedByEngine {
				t.Fatalf("the late engine page was closed %d times", got)
			}
			if got := engine.disposed.Load(); got != 1 || registeredScopes(manager) != 0 {
				t.Fatalf("%d disposals and %d scopes after the abandoned creation", got, registeredScopes(manager))
			}
		})
	}
}

// A user's failed creation whose profile would not stop reports that cleanup
// even when the tab is closed after the creation settled and before its
// failed tab was published, and the close that took it answers with it.
func TestAFailedTabClosedBeforeItIsShownReportsItsCleanup(t *testing.T) {
	manager, engine := newLifecycleManager(t)
	engine.newPageErr = errors.New("engine refused the page")
	engine.disposeErr = errors.New("the profile's browser would not stop")
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	var (
		closing  sync.Once
		pending  *pendingPage
		teardown = make(chan CompanionEvent, 1)
	)
	manager.SetEventSink(func(event CompanionEvent) {
		if event.Kind == "teardown" {
			teardown <- event
			return
		}
		// The first state with the failed tab: the creation has settled and
		// is publishing it.
		for _, page := range event.Pages {
			if page.Error != "" {
				closing.Do(func() {
					var ok bool
					if pending, ok = manager.closePending(access, page.ID); !ok {
						t.Error("the close does not find the failed tab")
					}
				})
			}
		}
	})
	if _, err := manager.NewCompanionPage(t.Context(), access, ""); err != nil {
		t.Fatal(err)
	}
	if pending == nil {
		t.Fatal("the failed tab was never published")
	}
	if err := pending.wait(t.Context()); !errors.Is(err, engine.disposeErr) {
		t.Fatalf("the close of the failed tab = %v, want the cleanup failure", err)
	}
	select {
	case event := <-teardown:
		if !strings.Contains(event.Error, "would not stop") {
			t.Fatalf("teardown event = %+v", event)
		}
	default:
		t.Fatal("no teardown report for the cleanup failure")
	}
}

// Closing a thread closes its tab still being created, so no page registers
// after it, and forgets its failed one.
func TestCloseThreadClosesCreatingAndFailedTabs(t *testing.T) {
	manager, engine := newLifecycleManager(t)
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	engine.newPageErr = errors.New("engine refused the page")
	if _, err := manager.NewCompanionPage(t.Context(), access, ""); err != nil {
		t.Fatal(err)
	}
	engine.mu.Lock()
	engine.newPageErr = nil
	engine.mu.Unlock()
	release := engine.hold(&engine.newPageGate)
	defer release()
	created := async(func() error {
		_, err := manager.NewCompanionPage(context.Background(), access, "")
		return err
	})
	receive(t, engine.creating, "the engine page's creation")
	if got := tabIDs(manager, access); len(got) != 2 {
		t.Fatalf("tabs before the close = %v, want the failed and the creating one", got)
	}
	if err := manager.CloseThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	if got := tabIDs(manager, access); len(got) != 0 {
		t.Fatalf("tabs after closing the thread = %v", got)
	}
	release()
	if err := await(t, created, "the creation"); err != nil {
		t.Fatalf("creation closed by the thread's close = %v", err)
	}
	if got := tabIDs(manager, access); len(got) != 0 {
		t.Fatalf("a page registered after the thread closed: %v", got)
	}
	manager.mu.Lock()
	pending := len(manager.pending)
	manager.mu.Unlock()
	if pending != 0 || registeredScopes(manager) != 0 {
		t.Fatalf("%d pending entries and %d scopes after the thread closed", pending, registeredScopes(manager))
	}
}

// Selecting a tab presents it before any page work: a live page is shown
// before its address is read, and a suspended page is active and restoring
// before its restore ends.
func TestSelectPresentsBeforePageWork(t *testing.T) {
	manager, engine := newLifecycleManager(t)
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	a := mustOpen(t, manager, access, "https://example.test/a")
	b, err := manager.NewCompanionPage(t.Context(), access, "https://example.test/b")
	if err != nil {
		t.Fatal(err)
	}
	attachSizedPane(t, manager, engine.recordingPaneHost, access)

	releaseInfo := engine.hold(&engine.infoGate)
	defer releaseInfo()
	selected := async(func() error {
		_, err := manager.SelectPage(context.Background(), access, a.ID)
		return err
	})
	receive(t, engine.reading, "the page's address read")
	// The pane attached after the pages opened, so the selected page first
	// takes the pane's size (viewport.go) and is shown then. That must not
	// wait for the address read, which is still held here.
	shown := "show:" + paneHandle(manager, access, a.ID)
	var calls []string
	eventually(t, "the selected page shown while its address read is held", func() bool {
		calls = append(calls, engine.take()...)
		return slices.Contains(calls, shown)
	})
	releaseInfo()
	if err := await(t, selected, "the select"); err != nil {
		t.Fatal(err)
	}

	if err := manager.SuspendThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	releaseNav := engine.hold(&engine.navGate)
	defer releaseNav()
	activated := async(func() error { return manager.ActivateCompanionPage(context.Background(), access, b.ID) })
	receive(t, engine.navigating, "the restore's navigation")
	state := manager.CompanionState(access)
	tab, _ := findTab(manager, access, b.ID)
	if state.ActivePageID != b.ID || !tab.Restoring {
		t.Fatalf("while restoring: active %q, tab %+v", state.ActivePageID, tab)
	}
	releaseNav()
	if err := await(t, activated, "the restore"); err != nil {
		t.Fatal(err)
	}
	if tab, _ := findTab(manager, access, b.ID); tab.Suspended || tab.Restoring {
		t.Fatalf("after the restore: %+v", tab)
	}
}

// A tab the user closes while it restores is gone, and the selection that
// restored it is no error.
func TestClosingATabWhileItRestores(t *testing.T) {
	manager, engine := newLifecycleManager(t)
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	page := mustOpen(t, manager, access, "https://example.test/")
	if err := manager.SuspendThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	release := engine.hold(&engine.navGate)
	defer release()
	activated := async(func() error { return manager.ActivateCompanionPage(context.Background(), access, page.ID) })
	receive(t, engine.navigating, "the restore's navigation")
	if err := manager.CloseCompanionPage(t.Context(), access, page.ID); err != nil {
		t.Fatalf("close a restoring tab: %v", err)
	}
	if got := tabIDs(manager, access); len(got) != 0 {
		t.Fatalf("tabs right after closing the restoring one = %v", got)
	}
	release()
	if err := await(t, activated, "the restore"); err != nil {
		t.Fatalf("the selection of a tab closed while it restored = %v", err)
	}
	if got := tabIDs(manager, access); len(got) != 0 {
		t.Fatalf("the restore brought back %v", got)
	}
}
