package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
)

// tabURLs answers the thread's tabs' addresses in tab order.
func tabURLs(m *Manager, access Access) []string {
	var urls []string
	for _, page := range m.CompanionState(access.Caller()).Pages {
		urls = append(urls, page.URL)
	}
	return urls
}

// waitDisposed waits until the profile of jar has been disposed.
func waitDisposed(t *testing.T, engine *jarEngine, jar string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if profile := engine.profile(jar); profile != nil && profile.disposed.Load() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the profile of %s was never disposed", jar)
		}
		time.Sleep(time.Millisecond)
	}
}

// restore restores one tab as the pane's selection does, in the background.
func restore(m *Manager, access Access, pageID string) <-chan error {
	done := make(chan error, 1)
	go func() { done <- m.ActivateCompanionPage(context.Background(), access, PageID(pageID)) }()
	return done
}

// Switching the jar with tabs open suspends every live tab into the new jar
// under its id, order, label and address, the active tab staying active; a
// tab already suspended stays so; the old jar, left empty, is disposed; and
// each tab restores into the new jar when it is used.
func TestSwitchingTheJarSuspendsTheTabsIntoTheNewJar(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
	first := openPage(t, manager, access, "https://example.test/first")
	second := openPage(t, manager, access, "https://example.test/second")
	third := openPage(t, manager, access, "https://example.test/third")
	if _, err := manager.LabelPage(t.Context(), access, PageID(second.ID), "login"); err != nil {
		t.Fatal(err)
	}
	if err := manager.MoveCompanionPage(access, PageID(third.ID), 0); err != nil {
		t.Fatal(err)
	}
	if err := manager.ActivateCompanionPage(t.Context(), access, PageID(second.ID)); err != nil {
		t.Fatal(err)
	}
	live, _, err := manager.lookupOwnedPage(access, PageID(first.ID))
	if err != nil {
		t.Fatal(err)
	}
	if moved, err := manager.suspendPage(t.Context(), live); err != nil || !moved {
		t.Fatalf("suspend the first page: %v", err)
	}
	beforeIDs, beforeURLs := tabIDs(manager, access), tabURLs(manager, access)

	setJar(t, manager, access, JarThread)

	state := manager.CompanionState(access.Caller())
	if state.Jar != JarThread {
		t.Fatalf("state names jar %q, want thread", state.Jar)
	}
	if ids, urls := tabIDs(manager, access), tabURLs(manager, access); !slices.Equal(ids, beforeIDs) || !slices.Equal(urls, beforeURLs) {
		t.Fatalf("tabs after the switch = %q %q, want %q %q", ids, urls, beforeIDs, beforeURLs)
	}
	for _, page := range state.Pages {
		if !page.Suspended {
			t.Fatalf("tab %+v is not suspended after the switch", page)
		}
		if page.ID == second.ID && page.Label != "login" {
			t.Fatalf("the second tab lost its label: %+v", page)
		}
	}
	if state.ActivePageID != second.ID {
		t.Fatalf("active tab after the switch is %q, want %q", state.ActivePageID, second.ID)
	}
	waitDisposed(t, engine, canonical(t, proj.one))

	for _, id := range []string{second.ID, first.ID} {
		if err := manager.ActivateCompanionPage(t.Context(), access, PageID(id)); err != nil {
			t.Fatalf("restore %s: %v", id, err)
		}
		if got := pageJar(t, manager, id); got != "thread:a" {
			t.Fatalf("tab %s restored into %q, want the new jar", id, got)
		}
	}
	if page, _ := findTab(manager, access, third.ID); !page.Suspended {
		t.Fatal("a tab nobody used was loaded")
	}
}

// The switch's records are on disk before any page's teardown begins: a
// shutdown or crash while the old pages close loses no tab and keeps the new
// jar.
func TestASwitchSavesItsTabsBeforeTheOldPagesClose(t *testing.T) {
	dir := t.TempDir()
	manager, engine := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
	openPage(t, manager, access, "https://example.test/one")
	openPage(t, manager, access, "https://example.test/two")
	release := engine.holdClose()
	t.Cleanup(release)
	setJar(t, manager, access, JarThread)

	// A second process reading the same saved copy while the old pages are
	// still closing: what a restart after a crash here would find.
	restarted, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	state := restarted.CompanionState(access.Caller())
	if state.Jar != JarThread || !slices.Equal(tabURLs(restarted, access), []string{"https://example.test/one", "https://example.test/two"}) {
		t.Fatalf("saved copy during the switch: jar %q, tabs %q", state.Jar, tabURLs(restarted, access))
	}
	release()
}

// A page that changes its title during every write of a switch's saved copy
// neither holds the switch back nor, through it, another thread's save: the
// switch commits the copy it wrote, and the record in memory is the record
// on disk.
func TestABusyPageDoesNotHoldASwitch(t *testing.T) {
	dir := t.TempDir()
	manager, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
	page := openPage(t, manager, access, "https://example.test/busy")
	handle := paneHandle(manager, access, page.ID)
	var titles uint64
	saved := make(chan error, 1)
	manager.switchWritten = func() {
		titles++
		manager.updatePageInfo(ids.PageHandle(handle), "https://example.test/busy", fmt.Sprintf("title %d", titles), titles)
		if titles == 1 {
			go func() { saved <- manager.persistThreads([]ThreadID{"other"}) }()
		}
	}
	switched := make(chan error, 1)
	go func() { switched <- manager.SetJar(context.Background(), access, JarThread, JarSwitchConfirmed) }()
	for _, done := range []chan error{saved, switched} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a busy page's title held the jar switch, or the saves behind it")
		}
	}
	restarted, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	if got, want := restarted.CompanionState(access.Caller()).Pages, manager.CompanionState(access.Caller()).Pages; len(got) != 1 || len(want) != 1 || got[0].Title != want[0].Title {
		t.Fatalf("the saved tab %+v is not the switched tab %+v", got, want)
	}
}

// A tab opened and a tab closed during a switch's write wait for the
// switch (its structural gate), so the switch suspends exactly the tabs it
// captured and wrote: the new tab opens after it, live in the new jar, the
// closed one closes after it, and a restart loads the tabs as the thread
// left them, in the new jar.
func TestTabsChangedDuringASwitchChangeAfterIt(t *testing.T) {
	dir := t.TempDir()
	manager, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
	gone := openPage(t, manager, access, "https://example.test/gone")
	openPage(t, manager, access, "https://example.test/kept")
	opened := make(chan PageInfo, 1)
	closed := make(chan error, 1)
	manager.switchWritten = func() {
		manager.switchWritten = nil
		go func() {
			info, err := manager.Open(context.Background(), access, "https://example.test/new", OpenOptions{})
			if err != nil {
				t.Error(err)
			}
			opened <- info
		}()
		go func() { closed <- manager.ClosePage(context.Background(), access, PageID(gone.ID)) }()
		// Time for both to reach the gate; neither may pass it.
		time.Sleep(50 * time.Millisecond)
		if got := tabURLs(manager, access); !slices.Equal(got, []string{"https://example.test/gone", "https://example.test/kept"}) {
			t.Errorf("during the switch's write the tabs are %q", got)
		}
	}
	setJar(t, manager, access, JarThread)
	fresh := await(t, opened, "the open made during the switch")
	if err := await(t, closed, "the close made during the switch"); err != nil {
		t.Fatal(err)
	}
	want := []string{"https://example.test/kept", "https://example.test/new"}
	if got := tabURLs(manager, access); !slices.Equal(got, want) {
		t.Fatalf("after the switch the tabs are %q, want %q", got, want)
	}
	p := livePage(t, manager, access, fresh.ID)
	manager.mu.Lock()
	current := manager.jarCurrentLocked(p)
	manager.mu.Unlock()
	if !current {
		t.Fatal("the tab opened during the switch is live in the old jar")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	if got := tabURLs(restarted, access); !slices.Equal(got, want) || restarted.CompanionState(access.Caller()).Jar != JarThread {
		t.Fatalf("the saved copy holds %q in jar %q, want %q in the thread jar", got, restarted.CompanionState(access.Caller()).Jar, want)
	}
}

// A close of the thread (an archive) asked for during a switch's write
// waits for the switch, which holds the thread's saved copy, and then
// closes every tab the switch left and removes the copy. An action bound
// before the close changes nothing after it.
func TestAThreadClosedDuringASwitchClosesWhatTheSwitchLeft(t *testing.T) {
	dir := t.TempDir()
	manager, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
	openPage(t, manager, access, "https://example.test/")
	closed := make(chan error, 1)
	manager.switchWritten = func() {
		go func() { closed <- manager.CloseThread(context.Background(), access.ThreadID()) }()
		// Time for the close to reach the switch's hold.
		select {
		case err := <-closed:
			t.Errorf("the close finished during the switch's write: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err := manager.SetJar(t.Context(), access, JarThread, JarSwitchConfirmed); err != nil {
		t.Fatalf("the switch answered %v", err)
	}
	if err := await(t, closed, "the thread's close"); err != nil {
		t.Fatal(err)
	}
	if state := manager.CompanionState(access.Caller()); len(state.Pages) != 0 || state.Jar == JarThread {
		t.Fatalf("after the close the thread has %d tabs in jar %q", len(state.Pages), state.Jar)
	}
	if err := manager.SetJar(t.Context(), access, JarProject, JarSwitchConfirmed); !errors.Is(err, ErrThreadClosed) {
		t.Fatalf("a switch bound before the close answered %v, want ErrThreadClosed", err)
	}
	restarted, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	if state := restarted.CompanionState(access.Caller()); len(state.Pages) != 0 || state.Jar == JarThread {
		t.Fatalf("the saved copy of a closed thread holds %d tabs in jar %q", len(state.Pages), state.Jar)
	}
}

// A switch whose saved copy cannot be written changes nothing: the jar stays,
// the pages stay live, and a retry writes and switches.
func TestASwitchThatCannotSaveKeepsTheJarAndThePages(t *testing.T) {
	dir := t.TempDir()
	manager, engine := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
	openPage(t, manager, access, "https://example.test/one")
	openPage(t, manager, access, "https://example.test/two")
	if err := os.WriteFile(manager.recordDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetJar(context.Background(), access, JarThread, JarSwitchConfirmed); err == nil {
		t.Fatal("a switch whose saved copy failed reported success")
	}
	state := manager.CompanionState(access.Caller())
	if state.Jar != JarWorktree {
		t.Fatalf("a failed switch left jar %q, want the worktree jar", state.Jar)
	}
	for _, page := range state.Pages {
		if page.Suspended {
			t.Fatalf("a failed switch suspended tab %+v", page)
		}
	}
	if profile := engine.profile(canonical(t, proj.one)); profile.disposed.Load() {
		t.Fatal("a failed switch closed the old jar's pages")
	}

	if err := os.Remove(manager.recordDir); err != nil {
		t.Fatal(err)
	}
	setJar(t, manager, access, JarThread)
	restarted, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	if got := restarted.CompanionState(access.Caller()); got.Jar != JarThread || len(got.Pages) != 2 {
		t.Fatalf("the retried switch saved jar %q with %d tabs, want thread with 2", got.Jar, len(got.Pages))
	}
}

// A tab that cannot come back in the new jar stays suspended, with the
// failure reported to whoever restored it, and the others come back.
func TestASwitchedTabThatCannotComeBackStaysSuspended(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
	ok := openPage(t, manager, access, "https://example.test/ok")
	down := openPage(t, manager, access, "https://example.test/down")
	engine.mu.Lock()
	engine.navErr["https://example.test/down"] = errors.New("net::ERR_CONNECTION_REFUSED")
	engine.mu.Unlock()

	setJar(t, manager, access, JarThread)
	if err := manager.ActivateCompanionPage(t.Context(), access, PageID(down.ID)); err == nil || !strings.Contains(err.Error(), "ERR_CONNECTION_REFUSED") {
		t.Fatalf("restoring the failing tab answered %v", err)
	}
	if page, found := findTab(manager, access, down.ID); !found || !page.Suspended || page.URL != "https://example.test/down" {
		t.Fatalf("the failed tab = %+v (found %v), want it suspended at its address", page, found)
	}
	if err := manager.ActivateCompanionPage(t.Context(), access, PageID(ok.ID)); err != nil {
		t.Fatalf("restore the other tab: %v", err)
	}
}

// A thread at its page cap switches without passing it, and every tab comes
// back.
func TestASwitchAtTheThreadCapKeepsEveryTab(t *testing.T) {
	manager, _ := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
	for range maxPagesPerThread {
		openPage(t, manager, access, "https://example.test/")
	}
	setJar(t, manager, access, JarProject)
	if _, err := manager.NewPage(t.Context(), access); err == nil || !strings.Contains(err.Error(), "page limit reached for thread") {
		t.Fatalf("a page past the cap after the switch answered %v", err)
	}
	for _, id := range tabIDs(manager, access) {
		if err := manager.ActivateCompanionPage(t.Context(), access, PageID(id)); err != nil {
			t.Fatalf("restore %s at the cap: %v", id, err)
		}
	}
}

// While the old pages close, each one and the record that replaced it are one
// tab in the thread's cap: the thread can open as many more as it had room
// for, and no more.
func TestASwitchKeepsTheThreadCapExact(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
	for range maxPagesPerThread - 1 {
		openPage(t, manager, access, "https://example.test/")
	}
	release := engine.holdClose()
	t.Cleanup(release)
	setJar(t, manager, access, JarThread)
	if _, err := manager.NewPage(t.Context(), access); err != nil {
		t.Fatalf("the thread's last free slot was taken by the switch: %v", err)
	}
	if _, err := manager.NewPage(t.Context(), access); err == nil || !strings.Contains(err.Error(), "page limit reached for thread") {
		t.Fatalf("a page past the cap during the switch answered %v", err)
	}
}

// Closing the switched tabs while their old pages still close does not free
// the slots those pages hold: eight live pages with a slow teardown, a
// switch, all eight tabs closed, and the thread still cannot open a ninth
// engine page until the old ones are gone.
func TestClosedSwitchedTabsKeepTheirSlotsUntilTheirPagesClose(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
	for range maxPagesPerThread {
		openPage(t, manager, access, "https://example.test/")
	}
	release := engine.holdClose()
	t.Cleanup(release)
	setJar(t, manager, access, JarThread)
	for _, id := range tabIDs(manager, access) {
		if err := manager.CloseCompanionPage(t.Context(), access, PageID(id)); err != nil {
			t.Fatalf("close switched tab %s: %v", id, err)
		}
	}
	if ids := tabIDs(manager, access); len(ids) != 0 {
		t.Fatalf("tabs after closing every switched tab = %q", ids)
	}
	if _, err := manager.NewPage(t.Context(), access); err == nil || !strings.Contains(err.Error(), "still shutting down") {
		t.Fatalf("a page while the eight old pages still close answered %v", err)
	}
	release()
	// Each old page finishes closing on its own goroutine and frees its slot
	// then, so every one of the eight new pages may wait for one.
	deadline := time.Now().Add(5 * time.Second)
	for i := range maxPagesPerThread {
		for {
			_, err := manager.NewPage(t.Context(), access)
			if err == nil {
				break
			}
			if !strings.Contains(err.Error(), "still shutting down") || time.Now().After(deadline) {
				t.Fatalf("page %d after the old pages closed: %v", i+1, err)
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// A restore waits for every page the switch closed, so a workspace at its
// page cap switches without a tab failing on pages still shutting down.
func TestARestoreWaitsForTheSwitchsTeardown(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	var threads []Access
	for _, id := range []string{"a", "b", "c"} {
		access := proj.access(manager, id, proj.one)
		threads = append(threads, access)
		for range maxPagesPerThread {
			openPage(t, manager, access, "https://example.test/")
		}
	}
	release := engine.holdClose()
	t.Cleanup(release)
	setJar(t, manager, threads[0], JarThread)
	restored := restore(manager, threads[0], tabIDs(manager, threads[0])[0])
	select {
	case err := <-restored:
		t.Fatalf("the restore ended while the pages it replaces were still closing: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	if err := <-restored; err != nil {
		t.Fatalf("restore after the switch's teardown: %v", err)
	}
}

// A restore that would meet the profile cap only because the switch's own
// old profile is still closing waits for its disposal: twelve live profiles,
// two tabs of the switching thread in one, and the first old page closed
// while the second still closes.
func TestARestoreWaitsForTheSwitchsOldProfile(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	switching := proj.access(manager, "a", proj.one)
	openPage(t, manager, switching, "https://example.test/one")
	openPage(t, manager, switching, "https://example.test/two")
	for i := range maxWorkspaceContexts - 1 {
		other := proj.access(manager, "other-"+string(rune('a'+i)), proj.one)
		setJar(t, manager, other, JarThread)
		openPage(t, manager, other, "https://example.test/")
	}
	step := engine.stepCloses(t)
	setJar(t, manager, switching, JarThread)
	step()
	// Both tabs, so the one whose own old page has closed is among them.
	var restores []<-chan error
	for _, id := range tabIDs(manager, switching) {
		restores = append(restores, restore(manager, switching, id))
	}
	for _, restored := range restores {
		select {
		case err := <-restored:
			t.Fatalf("a restore ended while the old profile still had a page closing: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
	}
	step()
	for _, restored := range restores {
		if err := <-restored; err != nil {
			t.Fatalf("restore after the old profile's disposal: %v", err)
		}
	}
}

// Clearing site data while a switched tab waits to come back leaves nothing
// to come back: no tab, no page, no profile of the new jar.
func TestClearingSiteDataDuringASwitchLeavesNothing(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
	page := openPage(t, manager, access, "https://example.test/")
	release := engine.holdClose()
	t.Cleanup(release)
	setJar(t, manager, access, JarThread)
	restored := restore(manager, access, page.ID)
	// The restore is waiting for the switch's teardown.
	deadline := time.Now().Add(5 * time.Second)
	for tab, _ := findTab(manager, access, page.ID); !tab.Restoring; tab, _ = findTab(manager, access, page.ID) {
		if time.Now().After(deadline) {
			t.Fatal("the switched tab never started restoring")
		}
		time.Sleep(time.Millisecond)
	}
	cleared := make(chan error, 1)
	go func() { cleared <- manager.ClearSiteData(t.Context()) }()
	for len(tabIDs(manager, access)) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("clearing site data kept the switched tab")
		}
		time.Sleep(time.Millisecond)
	}
	release()
	if err := <-cleared; err != nil {
		t.Fatal(err)
	}
	// The tab is gone, so the selection that asked for it answers nothing.
	if err := <-restored; err != nil {
		t.Fatalf("restoring a cleared tab answered %v", err)
	}
	if ids := tabIDs(manager, access); len(ids) != 0 || len(manager.ownedPages(access.tb)) != 0 {
		t.Fatalf("after the clear: tabs %q, pages %d", ids, len(manager.ownedPages(access.tb)))
	}
	if engine.profile("thread:a") != nil {
		t.Fatal("the new jar's profile was created after its tabs were cleared")
	}
}

// A page being created when the jar switches is created again, under its
// tab, in the new jar: nothing lands in the old one.
func TestASwitchFencesAnInFlightCreate(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
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
	access := proj.access(manager, "a", proj.one)
	page := openPage(t, manager, access, "https://example.test/saved")
	live, _, err := manager.lookupOwnedPage(access, PageID(page.ID))
	if err != nil {
		t.Fatal(err)
	}
	if moved, err := manager.suspendPage(t.Context(), live); err != nil || !moved {
		t.Fatalf("suspend: %v", err)
	}
	release := engine.holdNav()
	t.Cleanup(release)
	restored := restore(manager, access, page.ID)
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
// opener has left for the new jar.
func TestASwitchFencesPopupAdoption(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
	page := openPage(t, manager, access, "https://example.test/")
	opener := paneHandle(manager, access, page.ID)
	worktree := canonical(t, proj.one)
	engine.mu.Lock()
	engine.onAttach = func() {
		engine.mu.Lock()
		engine.onAttach = nil
		engine.mu.Unlock()
		if err := manager.SetJar(context.Background(), access, JarThread, JarSwitchConfirmed); err != nil {
			t.Errorf("switch: %v", err)
		}
	}
	engine.mu.Unlock()
	manager.adoptPopup(enginePopup{Profile: engine.profile(worktree).Handle(), Opener: ids.PageHandle(opener), Handle: "popup", URL: "https://example.test/popup"})
	if ids := tabIDs(manager, access); !slices.Equal(ids, []string{page.ID}) {
		t.Fatalf("tabs after a popup raced the switch = %q, want only the opener", ids)
	}
	if err := manager.ActivateCompanionPage(t.Context(), access, PageID(page.ID)); err != nil {
		t.Fatal(err)
	}
	if got := pageJar(t, manager, page.ID); got != "thread:a" {
		t.Fatalf("the opener came back in %q", got)
	}
}

// With no tab the setting just applies, and setting the jar the thread has is
// no switch at all.
func TestSettingTheJarWithNoTabsOnlyApplies(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
	setJar(t, manager, access, JarProject)
	if len(engine.created()) != 0 || len(manager.CompanionState(access.Caller()).Pages) != 0 {
		t.Fatal("setting the jar with no tab opened something")
	}
	page := openPage(t, manager, access, "https://example.test/")
	setJar(t, manager, access, JarProject)
	if got, ok := findTab(manager, access, page.ID); !ok || got.Suspended {
		t.Fatalf("re-setting the same jar touched the tab: %+v", got)
	}
	if err := manager.SetJar(context.Background(), access, "shared", JarSwitchConfirmed); err == nil {
		t.Fatal("an unknown jar was accepted")
	}
	noProject := manager.Bind(Caller{ThreadID: "b", Workspace: proj.one})
	if err := manager.SetJar(context.Background(), noProject, JarProject, JarSwitchConfirmed); err == nil {
		t.Fatal("a thread with no project chose the project jar")
	}
}

// A second switch before the first one's tabs come back leaves them
// suspended, and they come back in the newest jar; the intermediate jar is
// never created.
func TestASecondSwitchMovesTheTabsToTheNewestJar(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
	page := openPage(t, manager, access, "https://example.test/")
	setJar(t, manager, access, JarThread)
	setJar(t, manager, access, JarProject)
	if err := manager.ActivateCompanionPage(t.Context(), access, PageID(page.ID)); err != nil {
		t.Fatal(err)
	}
	if got := pageJar(t, manager, page.ID); got != "project:P" {
		t.Fatalf("the tab came back in %q, want the newest jar", got)
	}
	if engine.profile("thread:a") != nil {
		t.Fatal("the intermediate jar was created")
	}
}
