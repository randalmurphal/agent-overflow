package browser

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// assertJarKept fails unless the thread is still in the worktree jar with
// its live tab live, in this manager and in the saved copy a restart reads.
func assertJarKept(t *testing.T, m *Manager, dir string, access Access, liveID string) {
	t.Helper()
	state := m.CompanionState(access.Caller())
	if state.Jar != JarWorktree {
		t.Fatalf("a refused switch left jar %q, want the worktree jar", state.Jar)
	}
	page, ok := findTab(m, access, liveID)
	if !ok || page.Suspended {
		t.Fatalf("a refused switch suspended the live tab: %+v", page)
	}
	restarted, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	if got := restarted.CompanionState(access.Caller()).Jar; got != JarWorktree {
		t.Fatalf("after a refused switch the saved copy names jar %q, want the worktree jar", got)
	}
}

// A switch the user has not confirmed is refused while a tab is live, in
// the hold that captures it, before it writes anything, and changes
// nothing; confirmed, the same switch goes ahead.
func TestAnUnconfirmedSwitchWithALiveTabIsRefused(t *testing.T) {
	dir := t.TempDir()
	manager, engine := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
	live := openPage(t, manager, access, "https://example.test/live")

	writes := 0
	manager.switchWritten = func() { writes++ }
	err := manager.SetJar(context.Background(), access, JarThread, JarSwitchIfNoLiveTab)
	if !errors.Is(err, ErrJarSwitchUnconfirmed) {
		t.Fatalf("an unconfirmed switch with a live tab answered %v, want ErrJarSwitchUnconfirmed", err)
	}
	if writes != 0 {
		t.Fatalf("a switch refused at its capture wrote its saved copy %d times, want none", writes)
	}
	assertJarKept(t, manager, dir, access, live.ID)
	if profile := engine.profile(canonical(t, proj.one)); profile.disposed.Load() {
		t.Fatal("a refused switch closed the old jar's pages")
	}

	setJar(t, manager, access, JarThread)
	if got := manager.CompanionState(access.Caller()).Jar; got != JarThread {
		t.Fatalf("the confirmed switch left jar %q, want thread", got)
	}
}

// With no live tab there is nothing to lose: suspended tabs and tabs that
// failed switch without asking.
func TestAnUnconfirmedSwitchWithNoLiveTabSwitches(t *testing.T) {
	manager, _ := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
	if err := manager.SetJar(context.Background(), access, JarPrivate, JarSwitchIfNoLiveTab); err != nil {
		t.Fatalf("an unconfirmed switch with no tab: %v", err)
	}
	suspended := openPage(t, manager, access, "https://example.test/suspended")
	suspendTab(t, manager, access, suspended.ID)
	if err := manager.SetJar(context.Background(), access, JarThread, JarSwitchIfNoLiveTab); err != nil {
		t.Fatalf("an unconfirmed switch with only a suspended tab: %v", err)
	}
	if got := manager.CompanionState(access.Caller()).Jar; got != JarThread {
		t.Fatalf("the switch left jar %q, want thread", got)
	}
}

// A switch that names no consent is refused before it reads the thread.
func TestASwitchWithNoConsentIsRefused(t *testing.T) {
	manager, _ := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
	for _, consent := range []JarConsent{jarConsentUnset, JarSwitchIfNoLiveTab + 1} {
		if err := manager.SetJar(context.Background(), access, JarThread, consent); err == nil {
			t.Fatalf("a switch with consent %d was admitted", consent)
		}
	}
	if got := manager.CompanionState(access.Caller()).Jar; got != JarWorktree {
		t.Fatalf("a switch with no consent left jar %q, want the worktree jar", got)
	}
}

// A tab opened after an unconfirmed switch captured the thread waits for
// the switch's commit (its structural gate) and opens after it, in the new
// jar: the consent the capture checked still holds at the commit, so the
// switch goes ahead and writes its saved copy once.
func TestATabOpenedDuringAnUnconfirmedSwitchOpensAfterIt(t *testing.T) {
	dir := t.TempDir()
	manager, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
	suspended := openPage(t, manager, access, "https://example.test/suspended")
	suspendTab(t, manager, access, suspended.ID)

	opened := make(chan PageInfo, 1)
	writes := 0
	manager.switchWritten = func() {
		writes++
		go func() { opened <- openPage(t, manager, access, "https://example.test/live") }()
		// Time for the open to reach the gate, which it may not pass.
		time.Sleep(50 * time.Millisecond)
	}
	err := manager.SetJar(context.Background(), access, JarThread, JarSwitchIfNoLiveTab)
	manager.switchWritten = nil
	if err != nil {
		t.Fatalf("an unconfirmed switch with no live tab at its capture answered %v", err)
	}
	if writes != 1 {
		t.Fatalf("the switch wrote its saved copy %d times, want once", writes)
	}
	live := await(t, opened, "the open made during the switch")
	p := livePage(t, manager, access, live.ID)
	manager.mu.Lock()
	current := manager.jarCurrentLocked(p)
	manager.mu.Unlock()
	if !current || manager.CompanionState(access.Caller()).Jar != JarThread {
		t.Fatal("the tab opened during the switch is not live in the new jar")
	}
}

// A switch refused after its write (the Manager's close) whose saved copy
// cannot be written back answers that the copy still names the new jar,
// beside the refusal.
func TestARefusedSwitchThatCannotWriteBackSaysSo(t *testing.T) {
	manager, _ := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
	manager.switchWritten = func() {
		manager.switchWritten = nil
		// The Manager's close begins: the commit refuses the switch.
		manager.mu.Lock()
		manager.closed = true
		manager.mu.Unlock()
		if err := os.RemoveAll(manager.recordDir); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(manager.recordDir, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	err := manager.SetJar(context.Background(), access, JarThread, JarSwitchIfNoLiveTab)
	if !errors.Is(err, errManagerClosed) {
		t.Fatalf("the switch answered %v, want the Manager's close", err)
	}
	if !strings.Contains(err.Error(), "still name the thread cookie jar") {
		t.Fatalf("the answer %q does not say the saved copy names the new jar", err)
	}
	if got := manager.CompanionState(access.Caller()).Jar; got != JarWorktree {
		t.Fatalf("a refused switch left jar %q, want the worktree jar", got)
	}
}

// A switch that meets the Manager's close after its write commits nothing,
// and the saved copy a restart reads names the jar the thread kept, with its
// tabs, though the close saves only threads with a live page.
func TestASwitchMeetingTheManagersCloseLeavesTheOldJarSaved(t *testing.T) {
	dir := t.TempDir()
	manager, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
	suspended := openPage(t, manager, access, "https://example.test/suspended")
	suspendTab(t, manager, access, suspended.ID)

	closed := make(chan error, 1)
	manager.switchWritten = func() {
		manager.switchWritten = nil
		go func() { closed <- manager.Close() }()
		eventually(t, "the close begins", func() bool {
			manager.mu.Lock()
			defer manager.mu.Unlock()
			return manager.closed
		})
	}
	if err := manager.SetJar(context.Background(), access, JarThread, JarSwitchConfirmed); !errors.Is(err, errManagerClosed) {
		t.Fatalf("a switch that met the close answered %v, want errManagerClosed", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	restarted, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	state := restarted.CompanionState(access.Caller())
	if state.Jar != JarWorktree || !slices.Equal(tabURLs(restarted, access), []string{"https://example.test/suspended"}) {
		t.Fatalf("after a switch the close stopped, the saved copy is jar %q with tabs %q, want the worktree jar with the suspended tab", state.Jar, tabURLs(restarted, access))
	}
}
