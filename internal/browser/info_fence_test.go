package browser

import (
	"context"
	"sync/atomic"
	"testing"
)

// fencePage opens a page at first and then second, so Back is on, and
// answers its managed page and fake engine page.
func fencePage(t *testing.T, manager *Manager, access Access) (*managedPage, *fakePage) {
	t.Helper()
	page := mustOpen(t, manager, access, "https://example.test/first")
	if _, err := manager.Open(t.Context(), access, "https://example.test/second", OpenOptions{PageID: PageID(page.ID)}); err != nil {
		t.Fatal(err)
	}
	p, _, err := manager.lookupOwnedPage(access, PageID(page.ID))
	if err != nil {
		t.Fatal(err)
	}
	fake := p.driver.(*lifecycleDriver).pageDriver.(*fakePage)
	fake.mu.Lock()
	fake.snapshot = func(context.Context) (Snapshot, error) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return Snapshot{PageInfo: PageInfo{URL: fake.url, Title: fake.title}}, nil
	}
	fake.mu.Unlock()
	return p, fake
}

// engineBack moves the fake page back one entry as the engine would on its
// own, with no tool call and no report yet.
func engineBack(t *testing.T, fake *fakePage) {
	t.Helper()
	fake.mu.Lock()
	fake.index--
	fake.url = fake.history[fake.index]
	fake.title = fakeTitle(fake.url)
	fake.mu.Unlock()
}

func expectFlags(t *testing.T, p *managedPage, back, forward bool, state string) {
	t.Helper()
	if info := p.cachedInfo(); info.CanGoBack != back || info.CanGoForward != forward {
		t.Fatalf("%s: the tab reads back %v forward %v, want %v %v", state, info.CanGoBack, info.CanGoForward, back, forward)
	}
}

// The address and the history state are fenced apart. A history refresh
// that commits while a suspension reads the address does not void that read:
// the suspension saves the address it read.
func TestAHistoryCommitDuringASuspendReadKeepsTheAddressItRead(t *testing.T) {
	manager, _ := newLifecycleManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	p, fake := fencePage(t, manager, access)
	// A report's history refresh is outstanding; the engine then moves to
	// B, whose report is still on its way.
	refresh := p.readStamp()
	const b = "https://example.test/b"
	fake.SameDocumentNavigation(b)
	var calls atomic.Int32
	manager.infoRead = func(*managedPage) {
		if calls.Add(1) == 1 {
			// The suspension read B; the refresh commits before it does.
			manager.refreshHistoryState(p, refresh)
		}
	}
	if err := manager.SuspendThread(t.Context(), access.ThreadID()); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	rec := manager.suspended[p.id]
	manager.mu.Unlock()
	if rec == nil || rec.url != b {
		t.Fatalf("the suspension saved %+v, want the address it read, %q", rec, b)
	}
}

// A report the engine made before a suspension's read, delivered while the
// read commits, does not replace the address the suspension read.
func TestASuspendSavesTheAddressItReadOverALateReport(t *testing.T) {
	manager, _ := newLifecycleManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	p, _ := fencePage(t, manager, access)
	var calls atomic.Int32
	manager.infoRead = func(*managedPage) {
		if calls.Add(1) == 1 {
			p.commitInfo(pageUpdate{Location: &pageLocation{URL: "https://example.test/first", Title: "late"}, Report: 1})
		}
	}
	if err := manager.SuspendThread(t.Context(), access.ThreadID()); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	rec := manager.suspended[p.id]
	manager.mu.Unlock()
	if rec == nil || rec.url != "https://example.test/second" {
		t.Fatalf("the suspension saved %+v, want the address it read", rec)
	}
}

// A snapshot commits the address only: the history refresh that followed
// the engine's report still commits after it.
func TestAHistoryRefreshCommitsAfterASnapshot(t *testing.T) {
	manager, _ := newLifecycleManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	p, fake := fencePage(t, manager, access)
	engineBack(t, fake)
	// The engine reports the back; its report keeps the shown flags and
	// stamps the refresh that will correct them.
	p.commitInfo(pageUpdate{Location: &pageLocation{URL: "https://example.test/first", Title: fakeTitle("https://example.test/first")}, Report: 1})
	refresh := p.readStamp()
	if _, err := manager.Snapshot(t.Context(), access, p.id); err != nil {
		t.Fatal(err)
	}
	expectFlags(t, p, true, false, "the snapshot before the refresh")
	manager.refreshHistoryState(p, refresh)
	expectFlags(t, p, false, true, "the refresh after the snapshot")
}

// The reverse: a history refresh that commits while a snapshot reads does
// not void the snapshot's address.
func TestASnapshotCommitsAfterAHistoryRefresh(t *testing.T) {
	manager, _ := newLifecycleManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	p, fake := fencePage(t, manager, access)
	refresh := p.readStamp()
	engineBack(t, fake)
	var calls atomic.Int32
	manager.infoRead = func(*managedPage) {
		if calls.Add(1) == 1 {
			// The snapshot read the first entry; the refresh commits first.
			manager.refreshHistoryState(p, refresh)
		}
	}
	if _, err := manager.Snapshot(t.Context(), access, p.id); err != nil {
		t.Fatal(err)
	}
	if got := p.cachedInfo().URL; got != "https://example.test/first" {
		t.Fatalf("the tab shows %q after the snapshot, want the address it read", got)
	}
	expectFlags(t, p, false, true, "the refresh and the snapshot")
}

// A history refresh that read before a later read began loses to that read
// whichever commits first, and an address commit does not void it.
func TestAStaleHistoryRefreshLosesToALaterRead(t *testing.T) {
	manager, _ := newLifecycleManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	p, fake := fencePage(t, manager, access)
	refresh := p.readStamp()
	held, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	manager.infoRead = func(*managedPage) {
		if calls.Add(1) == 1 {
			// The refresh read Back on, Forward off, and waits to commit.
			close(held)
			<-release
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.refreshHistoryState(p, refresh)
	}()
	<-held
	engineBack(t, fake)
	// An engine report of the move commits the address meanwhile.
	p.commitInfo(pageUpdate{Location: &pageLocation{URL: "https://example.test/first", Title: "first"}, Report: 1})
	if _, err := manager.SelectPage(t.Context(), access, p.id); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	expectFlags(t, p, false, true, "the later read and then the stale refresh")
}

// A report that reaches the Manager while its page is still being created
// moves the page's report order fence, so an older report delivered after
// the page joined its tabs does not replace it.
func TestAReportDuringCreationMovesTheOrderFence(t *testing.T) {
	manager, engine := newLifecycleManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	var hooks pageHooks
	engine.mu.Lock()
	engine.onNewPage = func(h pageHooks) {
		hooks = h
		// The newest report, delivered before the page is in its tabs.
		h.Info("about:blank", "", 2)
	}
	engine.mu.Unlock()
	page, err := manager.NewPage(t.Context(), access)
	if err != nil {
		t.Fatal(err)
	}
	// An older report, delayed on its goroutine.
	hooks.Info("https://example.test/intermediate", "Intermediate", 1)
	if tab, _ := findTab(manager, access, page.ID); tab.URL != "about:blank" || tab.Title == "Intermediate" {
		t.Fatalf("the tab shows %q %q, want the newest report's page", tab.URL, tab.Title)
	}
}

// The same during a restore: the final report arrives while the page loads
// unlisted, the load's read commits, and the intermediate report delivered
// after the page joined its tabs is dropped.
func TestAReportDuringRestoreMovesTheOrderFence(t *testing.T) {
	manager, engine := newRestoreManager(t, t.TempDir(), ManagerOptions{})
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page := mustOpen(t, manager, access, "https://example.test/one")
	if err := manager.SuspendThread(t.Context(), access.ThreadID()); err != nil {
		t.Fatal(err)
	}
	release := engine.hold()
	restored := make(chan error, 1)
	go func() { restored <- manager.ActivateCompanionPage(context.Background(), access, PageID(page.ID)) }()
	<-engine.navigating
	engine.mu.Lock()
	loading := engine.lastPage
	engine.mu.Unlock()
	hooks := loading.fake().hooks
	hooks.Info(page.URL, page.Title, 2)
	release()
	if err := <-restored; err != nil {
		t.Fatal(err)
	}
	hooks.Info("https://example.test/intermediate", "Intermediate", 1)
	if tab, _ := findTab(manager, access, page.ID); tab.URL != page.URL || tab.Title != page.Title {
		t.Fatalf("the restored tab shows %q %q, want %q %q", tab.URL, tab.Title, page.URL, page.Title)
	}
}

// A report for a page that left its thread's tabs changes nothing a tab or
// a push shows.
func TestAReportAfterThePageClosedIsANoOp(t *testing.T) {
	manager, _ := newLifecycleManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page := mustOpen(t, manager, access, "https://example.test/")
	p, _, err := manager.lookupOwnedPage(access, PageID(page.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ClosePage(t.Context(), access, PageID(page.ID)); err != nil {
		t.Fatal(err)
	}
	pushes := countStatePushes(manager)
	manager.pageInfoReport(p, "https://example.test/late", "Late", 1)
	if n := pushes.take(); n != 0 {
		t.Fatalf("a report for a closed page pushed %d states", n)
	}
	if _, ok := findTab(manager, access, page.ID); ok {
		t.Fatal("a report for a closed page brought its tab back")
	}
}
