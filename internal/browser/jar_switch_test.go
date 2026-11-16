package browser

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// tabURLs answers the thread's tabs' addresses in tab order.
func tabURLs(m *Manager, access Access) []string {
	var urls []string
	for _, page := range m.CompanionState(access).Pages {
		urls = append(urls, page.URL)
	}
	return urls
}

// Switching the jar with tabs open closes the live pages and reopens their
// addresses in the new jar in the same order, with the same tab active and
// the same labels; a suspended tab stays suspended and restores into the new
// jar; the old jar, left empty, is disposed.
func TestSwitchingTheJarReopensTheTabsInTheNewJar(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access("a", proj.one)
	first := openPage(t, manager, access, "https://example.test/first")
	second := openPage(t, manager, access, "https://example.test/second")
	third := openPage(t, manager, access, "https://example.test/third")
	if _, err := manager.LabelPage(t.Context(), access, second.ID, "login"); err != nil {
		t.Fatal(err)
	}
	if err := manager.MoveCompanionPage(access, third.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := manager.ActivateCompanionPage(t.Context(), access, second.ID); err != nil {
		t.Fatal(err)
	}
	live, _, err := manager.lookupOwnedPage(access, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if moved, err := manager.suspendPage(t.Context(), live); err != nil || !moved {
		t.Fatalf("suspend the first page: %v", err)
	}
	before := tabURLs(manager, access)
	worktree := canonical(t, proj.one)

	setJar(t, manager, access, JarThread)
	waitSettled(t, manager, access)

	state := manager.CompanionState(access)
	if after := tabURLs(manager, access); !slices.Equal(after, before) {
		t.Fatalf("tabs after the switch = %q, want %q in the same order", after, before)
	}
	if state.Jar != JarThread {
		t.Fatalf("state names jar %q, want thread", state.Jar)
	}
	var active PageInfo
	for _, page := range state.Pages {
		if page.ID == state.ActivePageID {
			active = page
		}
		if page.ID == second.ID || page.ID == third.ID {
			t.Fatalf("live page %s survived the switch under its old id", page.ID)
		}
		if page.ID == first.ID {
			if !page.Suspended {
				t.Fatal("the suspended tab was loaded by the switch")
			}
			continue
		}
		if page.Creating || page.Error != "" || page.Suspended {
			t.Fatalf("reopened tab %+v did not settle live", page)
		}
		if got := pageJar(t, manager, page.ID); got != "thread:a" {
			t.Fatalf("tab %s reopened in %q, want the thread's jar", page.URL, got)
		}
	}
	if active.URL != "https://example.test/second" || active.Label != "login" {
		t.Fatalf("the active tab after the switch is %+v, want the reopened second page with its label", active)
	}
	if !engine.profile(worktree).disposed.Load() {
		t.Fatal("the old jar outlived the switch that emptied it")
	}
	if err := manager.ActivateCompanionPage(t.Context(), access, first.ID); err != nil {
		t.Fatal(err)
	}
	if got := pageJar(t, manager, first.ID); got != "thread:a" {
		t.Fatalf("the suspended tab restored into %q, want the new jar", got)
	}
	if len(tabIDs(manager, access)) != 3 {
		t.Fatalf("the thread has %d tabs, want 3", len(tabIDs(manager, access)))
	}
}

// A tab whose reopen fails stays in its place with its address and the
// failure, and the others reopen.
func TestASwitchReportsAFailedReopenOnItsTab(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access("a", proj.one)
	openPage(t, manager, access, "https://example.test/ok")
	openPage(t, manager, access, "https://example.test/down")
	engine.mu.Lock()
	engine.navErr["https://example.test/down"] = errors.New("net::ERR_CONNECTION_REFUSED")
	engine.mu.Unlock()

	setJar(t, manager, access, JarThread)
	waitSettled(t, manager, access)
	pages := manager.CompanionState(access).Pages
	if len(pages) != 2 {
		t.Fatalf("%d tabs after the switch, want 2", len(pages))
	}
	if pages[0].Error != "" || pages[0].URL != "https://example.test/ok" {
		t.Fatalf("the first tab = %+v, want it reopened", pages[0])
	}
	if pages[1].URL != "https://example.test/down" || !strings.Contains(pages[1].Error, "ERR_CONNECTION_REFUSED") {
		t.Fatalf("the failed tab = %+v, want its address and the failure", pages[1])
	}
	if err := manager.CloseCompanionPage(t.Context(), access, pages[1].ID); err != nil {
		t.Fatalf("close the failed tab: %v", err)
	}
	if got := tabURLs(manager, access); !slices.Equal(got, []string{"https://example.test/ok"}) {
		t.Fatalf("tabs after closing the failed one = %q", got)
	}
}

// A tab's reopen waits for its predecessor to close, so a thread at its page
// cap switches without passing any cap, and its tabs stay within the cap.
func TestASwitchAtTheThreadCapReopensEveryTab(t *testing.T) {
	manager, _ := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access("a", proj.one)
	for range maxPagesPerThread {
		openPage(t, manager, access, "https://example.test/")
	}
	setJar(t, manager, access, JarProject)
	waitSettled(t, manager, access)
	pages := manager.CompanionState(access).Pages
	if len(pages) != maxPagesPerThread {
		t.Fatalf("%d tabs after the switch, want %d", len(pages), maxPagesPerThread)
	}
	for _, page := range pages {
		if page.Error != "" {
			t.Fatalf("tab failed to reopen at the cap: %s", page.Error)
		}
	}
	if _, err := manager.NewPage(t.Context(), access); err == nil || !strings.Contains(err.Error(), "page limit reached for thread") {
		t.Fatalf("a page past the cap after the switch answered %v", err)
	}
}

// A page being created when the jar switches is created again, under its
// tab, in the new jar: nothing lands in the old one.
func TestASwitchFencesAnInFlightCreate(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access("a", proj.one)
	release := engine.holdNewPage()
	t.Cleanup(release)
	created := make(chan PageInfo, 1)
	failed := make(chan error, 1)
	go func() {
		info, err := manager.NewPage(t.Context(), access)
		if err != nil {
			failed <- err
			return
		}
		created <- info
	}()
	receive(t, engine.creating, "the page's creation in the old jar")
	setJar(t, manager, access, JarProject)
	release()
	var info PageInfo
	select {
	case info = <-created:
	case err := <-failed:
		t.Fatalf("the create failed: %v", err)
	}
	if got := pageJar(t, manager, info.ID); got != "project:P" {
		t.Fatalf("a create in flight across the switch landed in %q, want the new jar", got)
	}
	if old := engine.profile(canonical(t, proj.one)); old == nil || !old.disposed.Load() {
		t.Fatal("the old jar was not released by the stale create")
	}
	if ids := tabIDs(manager, access); len(ids) != 1 || ids[0] != info.ID {
		t.Fatalf("tabs = %q, want the one created", ids)
	}
}

// A suspended page being restored when the jar switches is restored again in
// the new jar.
func TestASwitchFencesAnInFlightRestore(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access("a", proj.one)
	page := openPage(t, manager, access, "https://example.test/saved")
	live, _, err := manager.lookupOwnedPage(access, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if moved, err := manager.suspendPage(t.Context(), live); err != nil || !moved {
		t.Fatalf("suspend: %v", err)
	}
	release := engine.holdNav()
	t.Cleanup(release)
	restored := make(chan error, 1)
	go func() { restored <- manager.ActivateCompanionPage(t.Context(), access, page.ID) }()
	if got := <-engine.navigating; got != "https://example.test/saved" {
		t.Fatalf("the restore navigated to %q", got)
	}
	setJar(t, manager, access, JarThread)
	release()
	if err := <-restored; err != nil {
		t.Fatalf("restore across the switch: %v", err)
	}
	if got := pageJar(t, manager, page.ID); got != "thread:a" {
		t.Fatalf("a restore in flight across the switch landed in %q, want the new jar", got)
	}
}

// A popup a page opens while its thread switches jars is not adopted: its
// opener belongs to the old jar.
func TestASwitchFencesPopupAdoption(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access("a", proj.one)
	page := openPage(t, manager, access, "https://example.test/")
	opener := paneHandle(manager, access, page.ID)
	worktree := canonical(t, proj.one)
	engine.mu.Lock()
	engine.onAttach = func() {
		engine.mu.Lock()
		engine.onAttach = nil
		engine.mu.Unlock()
		if err := manager.SetJar(access, JarThread); err != nil {
			t.Errorf("switch: %v", err)
		}
	}
	engine.mu.Unlock()
	manager.adoptPopup(enginePopup{Profile: engine.profile(worktree).Handle(), Opener: opener, Handle: "popup", URL: "https://example.test/popup"})
	waitSettled(t, manager, access)
	if got := tabURLs(manager, access); !slices.Equal(got, []string{"https://example.test/"}) {
		t.Fatalf("tabs after a popup raced the switch = %q, want only the reopened opener", got)
	}
	if got := pageJar(t, manager, tabIDs(manager, access)[0]); got != "thread:a" {
		t.Fatalf("the opener reopened in %q", got)
	}
}

// With no tab the setting just applies, and setting the jar the thread has is
// no switch at all.
func TestSettingTheJarWithNoTabsOnlyApplies(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access("a", proj.one)
	setJar(t, manager, access, JarProject)
	if len(engine.created()) != 0 || len(manager.CompanionState(access).Pages) != 0 {
		t.Fatal("setting the jar with no tab opened something")
	}
	page := openPage(t, manager, access, "https://example.test/")
	setJar(t, manager, access, JarProject)
	if ids := tabIDs(manager, access); len(ids) != 1 || ids[0] != page.ID {
		t.Fatalf("re-setting the same jar reopened the tabs: %q", ids)
	}
	if err := manager.SetJar(access, "shared"); err == nil {
		t.Fatal("an unknown jar was accepted")
	}
	noProject := Access{ThreadID: "b", Workspace: proj.one}
	if err := manager.SetJar(noProject, JarProject); err == nil {
		t.Fatal("a thread with no project chose the project jar")
	}
}

// A reopened tab creates its engine page only once the page it replaces has
// closed, so a workspace at its page cap switches without a tab failing on
// pages still shutting down.
func TestAReopenWaitsForItsPredecessorsClose(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	var threads []Access
	for _, id := range []string{"a", "b", "c"} {
		access := proj.access(id, proj.one)
		threads = append(threads, access)
		for range maxPagesPerThread {
			openPage(t, manager, access, "https://example.test/")
		}
	}
	release := engine.holdClose()
	t.Cleanup(release)
	setJar(t, manager, threads[0], JarThread)
	// Long enough for a reopen that did not wait to have failed on the
	// workspace cap.
	for deadline := time.Now().Add(200 * time.Millisecond); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		for _, page := range manager.CompanionState(threads[0]).Pages {
			if !page.Creating {
				t.Fatalf("tab %+v settled while the pages it replaces were still closing", page)
			}
		}
	}
	release()
	waitSettled(t, manager, threads[0])
	for _, page := range manager.CompanionState(threads[0]).Pages {
		if page.Error != "" {
			t.Fatalf("a reopened tab failed: %s", page.Error)
		}
	}
}

// While a switch reopens the tabs, each reopened tab holds its predecessor's
// place in the thread's cap, once: the thread can open as many more tabs as
// it had room for, and no more.
func TestASwitchKeepsTheThreadCapExact(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access("a", proj.one)
	for range maxPagesPerThread - 1 {
		openPage(t, manager, access, "https://example.test/")
	}
	// The replaced pages stay closing while the thread opens more.
	release := engine.holdClose()
	t.Cleanup(release)
	setJar(t, manager, access, JarThread)
	if _, err := manager.NewPage(t.Context(), access); err != nil {
		t.Fatalf("the thread's last free slot was taken by the switch: %v", err)
	}
	if _, err := manager.NewPage(t.Context(), access); err == nil || !strings.Contains(err.Error(), "page limit reached for thread") {
		t.Fatalf("a page past the cap during the switch answered %v", err)
	}
	release()
	waitSettled(t, manager, access)
	if got := len(manager.CompanionState(access).Pages); got != maxPagesPerThread {
		t.Fatalf("%d tabs after the switch, want %d", got, maxPagesPerThread)
	}
}

// A second switch while the first one's tabs are still reopening moves them
// to the newest jar.
func TestASecondSwitchMovesTabsStillReopening(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access("a", proj.one)
	openPage(t, manager, access, "https://example.test/")
	release := engine.holdNav()
	t.Cleanup(release)
	setJar(t, manager, access, JarThread)
	<-engine.navigating
	setJar(t, manager, access, JarProject)
	release()
	waitSettled(t, manager, access)
	ids := tabIDs(manager, access)
	if len(ids) != 1 {
		t.Fatalf("tabs = %q, want one", ids)
	}
	if got := pageJar(t, manager, ids[0]); got != "project:P" {
		t.Fatalf("the tab settled in %q, want the newest jar", got)
	}
	if thread := engine.profile("thread:a"); thread == nil || !thread.disposed.Load() {
		t.Fatal("the intermediate jar was not released")
	}
}

// A tab still reopening when the browser closes (site data cleared) goes
// with the browser, as its live tabs do.
func TestATabReopeningWhenTheBrowserClosesGoesWithIt(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access("a", proj.one)
	openPage(t, manager, access, "https://example.test/")
	release := engine.holdNav()
	t.Cleanup(release)
	setJar(t, manager, access, JarThread)
	<-engine.navigating
	if err := manager.ClearSiteData(t.Context()); err != nil {
		t.Fatal(err)
	}
	release()
	waitSettled(t, manager, access)
	if state := manager.CompanionState(access); len(state.Pages) != 0 || state.ActivePageID != "" {
		t.Fatalf("after the browser closed: tabs %+v, active %q; want none", state.Pages, state.ActivePageID)
	}
	if len(manager.ownedPages(access.ThreadID)) != 0 {
		t.Fatal("a reopened page was registered in a closed browser")
	}
}

// A reopen that fails and leaves its new jar's profile unable to stop reports
// that cleanup even when the failed tab is closed before it is shown, and the
// close that took the tab answers with it, as a user's failed new tab does.
func TestAFailedReopenClosedBeforeItIsShownReportsItsCleanup(t *testing.T) {
	manager, engine := newLifecycleManager(t)
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	openPage(t, manager, access, "https://example.test/")
	release := engine.hold(&engine.newPageGate)
	t.Cleanup(release)
	var (
		closing  sync.Once
		pending  *pendingPage
		teardown = make(chan CompanionEvent, 4)
	)
	manager.SetEventSink(func(event CompanionEvent) {
		if event.Kind == "teardown" {
			teardown <- event
			return
		}
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
	setJar(t, manager, access, JarThread)
	// The old page has closed and its profile stopped; the reopen is creating
	// its page in the new jar.
	await(t, engine.creating, "the reopen's page creation")
	engine.mu.Lock()
	engine.newPageErr = errors.New("engine refused the page")
	engine.disposeErr = errors.New("the profile's browser would not stop")
	engine.mu.Unlock()
	release()
	deadline := time.Now().Add(5 * time.Second)
	for len(manager.CompanionState(access).Pages) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the failed tab was never closed: %+v", manager.CompanionState(access).Pages)
		}
		time.Sleep(time.Millisecond)
	}
	if pending == nil {
		t.Fatal("the failed tab was never published")
	}
	if err := pending.wait(t.Context()); !errors.Is(err, engine.disposeErr) {
		t.Fatalf("the close of the failed tab = %v, want the cleanup failure", err)
	}
	event := await(t, teardown, "the teardown report")
	if !strings.Contains(event.Error, "would not stop") {
		t.Fatalf("teardown event = %+v", event)
	}
}
