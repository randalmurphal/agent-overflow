package browser

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

type showResult struct {
	outcome ShowOutcome
	page    string
	err     error
}

func show(m *Manager, ctx context.Context, access Access) <-chan showResult {
	return async(func() showResult {
		outcome, page, err := m.ShowCompanion(ctx, access)
		return showResult{outcome, string(page), err}
	})
}

func mustShow(t *testing.T, got showResult, outcome ShowOutcome, page string) {
	t.Helper()
	if got.err != nil || got.outcome != outcome || got.page != page {
		t.Fatalf("show = %v %q %v, want %v %q", got.outcome, got.page, got.err, outcome, page)
	}
}

func shownState(m *Manager, access Access) bool {
	state := m.CompanionState(access.Caller())
	return state.Visible != nil && *state.Visible
}

func hide(t *testing.T, m *Manager, access Access) {
	t.Helper()
	visible := false
	if _, err := m.Visibility(t.Context(), access, &visible, ""); err != nil {
		t.Fatal(err)
	}
}

// Two shows of a thread with no tab, the second arriving while the first's
// page is created, open one tab between them: the second shows the tab the
// first added.
func TestTwoShowsAtOnceOpenOneTab(t *testing.T) {
	m, engine := newLifecycleManager(t)
	access := m.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	release := engine.hold(&engine.newPageGate)
	defer release()
	first := show(m, context.Background(), access)
	receive(t, engine.creating, "the first show's page")
	ids := tabIDs(m, access)
	if len(ids) != 1 {
		t.Fatalf("tabs while the first show creates = %v, want one", ids)
	}
	mustShow(t, await(t, show(m, context.Background(), access), "the second show"), ShowShown, ids[0])
	release()
	mustShow(t, await(t, first, "the first show"), ShowOpened, ids[0])
	if got := tabIDs(m, access); !slices.Equal(got, ids) || activeTab(m, access) != ids[0] || !shownState(m, access) {
		t.Fatalf("after both shows: tabs %v active %s shown %v; want %v, shown", got, activeTab(m, access), shownState(m, access), ids)
	}
}

// A thread with tabs gets none from a show: the selected tab is shown, and
// with none selected the one used last.
func TestAShowOfAThreadWithTabsAddsNone(t *testing.T) {
	m, access, ids := newTabsManager(t, addr("a"), addr("b"))
	hide(t, m, access)
	mustShow(t, await(t, show(m, t.Context(), access), "the show"), ShowShown, ids[1])
	if got := tabIDs(m, access); !slices.Equal(got, ids) || !shownState(m, access) {
		t.Fatalf("after the show: tabs %v shown %v, want %v shown", got, shownState(m, access), ids)
	}

	// No tab selected: the one used last, b, which the open of b left so.
	hide(t, m, access)
	setActiveTab(m, access, "")
	mustShow(t, await(t, show(m, t.Context(), access), "the show"), ShowShown, ids[1])
	if got := tabIDs(m, access); !slices.Equal(got, ids) || activeTab(m, access) != ids[1] || !shownState(m, access) {
		t.Fatalf("after the show: tabs %v active %s, want %v with b active, shown", got, activeTab(m, access), ids)
	}
}

// A show waits for its turn in the pane's order: an agent's tab opened
// before that turn is what it shows, and no blank tab is added beside it.
func TestAShowRacingAnAgentsOpenAddsNoBlankTab(t *testing.T) {
	m, _ := newLifecycleManager(t)
	installGapClock(m)
	access := m.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	mount := mustAttachEmpty(t, m, access)
	t.Cleanup(func() { assertSelectionsSettled(t, m) })
	later := show(m, numbered(context.Background(), mount, 2), access)
	awaitWaiting(t, m, mount, 1)
	page := mustOpen(t, m, access, addr("agent"))
	mustShow(t, await(t, show(m, numbered(context.Background(), mount, 1), access), "the first show"), ShowShown, page.ID)
	mustShow(t, await(t, later, "the second show"), ShowShown, page.ID)
	if got := tabIDs(m, access); !slices.Equal(got, []string{page.ID}) || !shownState(m, access) {
		t.Fatalf("tabs %v shown %v, want only the agent's tab, shown", got, shownState(m, access))
	}
}

// A show takes its turn after the user's earlier selection and close: it
// shows the tab they leave active, never the one active when it arrived.
func TestAShowShowsTheTabItsTurnLeavesActive(t *testing.T) {
	m, access, ids, mount := newSelectionManager(t)
	hide(t, m, access)
	// d is active; the show arrives before the selection of b it follows.
	shownB := show(m, numbered(context.Background(), mount, 2), access)
	awaitWaiting(t, m, mount, 1)
	if err := m.ActivateCompanionPage(numbered(t.Context(), mount, 1), access, PageID(ids[1])); err != nil {
		t.Fatal(err)
	}
	mustShow(t, await(t, shownB, "the show after the selection"), ShowShown, ids[1])

	// The show arrives before the close of b it follows.
	hide(t, m, access)
	shownNext := show(m, numbered(context.Background(), mount, 4), access)
	awaitWaiting(t, m, mount, 1)
	if err := m.CloseCompanionPage(numbered(t.Context(), mount, 3), access, PageID(ids[1])); err != nil {
		t.Fatal(err)
	}
	if _, open := findTab(m, access, ids[1]); open {
		t.Fatalf("the close did not close b: %v", tabIDs(m, access))
	}
	active := activeTab(m, access)
	mustShow(t, await(t, shownNext, "the show after the close"), ShowShown, active)
	if active == "" || active == ids[1] || len(tabIDs(m, access)) != 3 || !shownState(m, access) {
		t.Fatalf("shown %q, tabs %v, shown %v: want the tab the close left active, no tab added", active, tabIDs(m, access), shownState(m, access))
	}
}

// A show whose pane went before its turn does nothing, even for a thread
// with no tab.
func TestAShowWhosePaneWentOpensNothing(t *testing.T) {
	m, _ := newLifecycleManager(t)
	access := m.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	mustShow(t, await(t, show(m, numbered(context.Background(), "gone", 1), access), "the show"), ShowPassed, "")
	if got := tabIDs(m, access); len(got) != 0 || shownState(m, access) {
		t.Fatalf("tabs %v shown %v after a passed show, want none", got, shownState(m, access))
	}
}

// A show admitted at its turn whose pane detaches before its commit
// shows nothing and opens nothing, with tabs or without.
func TestAShowWhosePaneDetachesBeforeItsCommitDoesNothing(t *testing.T) {
	for _, tabs := range []bool{true, false} {
		m, _ := newLifecycleManager(t)
		access := m.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
		page := mustOpen(t, m, access, addr("a"))
		mount, err := m.AttachPane(access.Caller(), untiedPane)
		if err != nil {
			t.Fatal(err)
		}
		if !tabs {
			if err := m.CloseCompanionPage(t.Context(), access, PageID(page.ID)); err != nil {
				t.Fatal(err)
			}
		}
		hide(t, m, access)
		before := tabIDs(m, access)
		admitted, resume := pauseAdmissions(m, mount.ID)
		shown := show(m, numbered(context.Background(), mount.ID, 1), access)
		select {
		case <-admitted:
		case <-time.After(10 * time.Second):
			t.Fatal("the show was never admitted")
		}
		m.DetachPane(MountID(mount.ID))
		close(resume)
		mustShow(t, await(t, shown, "the show"), ShowPassed, "")
		if got := tabIDs(m, access); !slices.Equal(got, before) || shownState(m, access) {
			t.Fatalf("with tabs %v: after the show tabs %v shown %v, want %v hidden", tabs, got, shownState(m, access), before)
		}
	}
}

// A show that took its turn and waits out a jar switch's structural gate,
// whose pane detaches meanwhile, shows nothing and opens nothing once the
// switch commits, with tabs or without: its commit finds the turn passed.
func TestAShowWhosePaneDetachesDuringASwitchDoesNothing(t *testing.T) {
	for _, tabs := range []bool{true, false} {
		manager, _ := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
		proj := newProject(t, "P")
		access := proj.access(manager, "a", proj.one)
		page := mustOpen(t, manager, access, addr("a"))
		mount, err := manager.AttachPane(access.Caller(), untiedPane)
		if err != nil {
			t.Fatal(err)
		}
		if !tabs {
			if err := manager.CloseCompanionPage(t.Context(), access, PageID(page.ID)); err != nil {
				t.Fatal(err)
			}
		}
		hide(t, manager, access)
		before := tabIDs(manager, access)
		var shown <-chan showResult
		manager.switchWritten = func() {
			shown = show(manager, numbered(context.Background(), mount.ID, 1), access)
			deadline := time.Now().Add(10 * time.Second)
			for !orderStats(manager, mount.ID).Taken {
				if time.Now().After(deadline) {
					t.Error("the show never took its turn")
					break
				}
				time.Sleep(time.Millisecond)
			}
			// Time for the show to reach the gate, which it may not pass.
			time.Sleep(50 * time.Millisecond)
			manager.DetachPane(MountID(mount.ID))
		}
		err = manager.SetJar(context.Background(), access, JarThread, JarSwitchConfirmed)
		manager.switchWritten = nil
		if err != nil {
			t.Fatal(err)
		}
		mustShow(t, await(t, shown, "the show"), ShowPassed, "")
		if got := tabIDs(manager, access); !slices.Equal(got, before) || shownState(manager, access) {
			t.Fatalf("with tabs %v: after the show tabs %v shown %v, want %v hidden", tabs, got, shownState(manager, access), before)
		}
	}
}

// A show bound to a lifetime the thread's close detached opens nothing in
// the fresh one.
func TestAShowOfAClosedLifetimeOpensNothing(t *testing.T) {
	m, _ := newLifecycleManager(t)
	caller := Caller{ThreadID: "thread", Workspace: t.TempDir()}
	access := m.Bind(caller)
	if err := m.CloseThread(t.Context(), caller.ThreadID); err != nil {
		t.Fatal(err)
	}
	got := await(t, show(m, t.Context(), access), "the show")
	if !errors.Is(got.err, ErrThreadClosed) || got.outcome != ShowPassed {
		t.Fatalf("show = %v %q %v, want ErrThreadClosed", got.outcome, got.page, got.err)
	}
	if ids := tabIDs(m, m.Bind(caller)); len(ids) != 0 {
		t.Fatalf("the fresh lifetime has tabs %v", ids)
	}
}

// A show reaching a lifetime the thread's close detached while its tabs
// are still being closed shows none of them.
func TestAShowDuringTheThreadsCloseShowsNothing(t *testing.T) {
	m, _ := newLifecycleManager(t)
	access := m.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	mustOpen(t, m, access, addr("a"))
	hide(t, m, access)
	reached, releaseClose := holdCloseAfterDetach(m, access)
	defer releaseClose()
	closed := async(func() error { return m.CloseThread(context.Background(), access.ThreadID()) })
	<-reached
	got := await(t, show(m, context.Background(), access), "the show")
	releaseClose()
	if err := await(t, closed, "the thread's close"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(got.err, ErrThreadClosed) || got.outcome != ShowPassed {
		t.Fatalf("show = %v %q %v, want ErrThreadClosed", got.outcome, got.page, got.err)
	}
	if access.tb.session.Visible {
		t.Fatal("the show made the detached lifetime visible")
	}
}
