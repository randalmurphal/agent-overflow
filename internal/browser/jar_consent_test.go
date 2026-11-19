package browser

import (
	"errors"
	"testing"
)

// assertJarKept fails unless the thread is still in the worktree jar with
// its live tab live, in this manager and in the saved copy a restart reads.
func assertJarKept(t *testing.T, m *Manager, dir string, access Access, liveID string) {
	t.Helper()
	state := m.CompanionState(access)
	if state.Jar != JarWorktree {
		t.Fatalf("a refused switch left jar %q, want the worktree jar", state.Jar)
	}
	page, ok := findTab(m, access, liveID)
	if !ok || page.Suspended {
		t.Fatalf("a refused switch suspended the live tab: %+v", page)
	}
	restarted, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	if got := restarted.CompanionState(access).Jar; got != JarWorktree {
		t.Fatalf("after a refused switch the saved copy names jar %q, want the worktree jar", got)
	}
}

// A switch the user has not confirmed is refused while a tab is live, and
// changes nothing; confirmed, the same switch goes ahead.
func TestAnUnconfirmedSwitchWithALiveTabIsRefused(t *testing.T) {
	dir := t.TempDir()
	manager, engine := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access("a", proj.one)
	live := openPage(t, manager, access, "https://example.test/live")

	err := manager.SetJarIfNoLiveTab(access, JarThread)
	if !errors.Is(err, ErrJarSwitchUnconfirmed) {
		t.Fatalf("an unconfirmed switch with a live tab answered %v, want ErrJarSwitchUnconfirmed", err)
	}
	assertJarKept(t, manager, dir, access, live.ID)
	if profile := engine.profile(canonical(t, proj.one)); profile.disposed.Load() {
		t.Fatal("a refused switch closed the old jar's pages")
	}

	setJar(t, manager, access, JarThread)
	if got := manager.CompanionState(access).Jar; got != JarThread {
		t.Fatalf("the confirmed switch left jar %q, want thread", got)
	}
}

// With no live tab there is nothing to lose: suspended tabs and tabs that
// failed switch without asking.
func TestAnUnconfirmedSwitchWithNoLiveTabSwitches(t *testing.T) {
	manager, _ := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access("a", proj.one)
	if err := manager.SetJarIfNoLiveTab(access, JarPrivate); err != nil {
		t.Fatalf("an unconfirmed switch with no tab: %v", err)
	}
	suspended := openPage(t, manager, access, "https://example.test/suspended")
	suspendTab(t, manager, access, suspended.ID)
	if err := manager.SetJarIfNoLiveTab(access, JarThread); err != nil {
		t.Fatalf("an unconfirmed switch with only a suspended tab: %v", err)
	}
	if got := manager.CompanionState(access).Jar; got != JarThread {
		t.Fatalf("the switch left jar %q, want thread", got)
	}
}

// A tab that goes live after the switch wrote its saved copy is seen in the
// hold that would commit it: the switch is refused there, and the saved copy
// it wrote is written back with the jar the thread keeps.
func TestATabGoingLiveDuringAnUnconfirmedSwitchRefusesIt(t *testing.T) {
	dir := t.TempDir()
	manager, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access("a", proj.one)
	suspended := openPage(t, manager, access, "https://example.test/suspended")
	suspendTab(t, manager, access, suspended.ID)

	var live PageInfo
	saves := 0
	manager.jarSwitchSaved = func() {
		saves++
		if saves == 1 {
			live = openPage(t, manager, access, "https://example.test/live")
		}
	}
	err := manager.SetJarIfNoLiveTab(access, JarThread)
	manager.jarSwitchSaved = nil
	if !errors.Is(err, ErrJarSwitchUnconfirmed) {
		t.Fatalf("a switch that met a live tab at its commit answered %v, want ErrJarSwitchUnconfirmed", err)
	}
	if saves != 1 {
		t.Fatalf("the switch wrote its saved copy %d times, want once", saves)
	}
	assertJarKept(t, manager, dir, access, live.ID)
}
