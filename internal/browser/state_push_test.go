package browser

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

// statePushes counts the thread state pushes a Manager publishes and keeps
// the newest by sequence, as a client does.
type statePushes struct {
	mu     sync.Mutex
	n      int
	newest CompanionEvent
}

func countStatePushes(m *Manager) *statePushes {
	c := &statePushes{}
	m.SetEventSink(func(event CompanionEvent) {
		if event.Kind == "state" {
			c.mu.Lock()
			c.n++
			if event.Seq > c.newest.Seq {
				c.newest = event
			}
			c.mu.Unlock()
		}
	})
	return c
}

// shownTitle is the page's title in the newest push.
func (c *statePushes) shownTitle(pageID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, page := range c.newest.Pages {
		if page.ID == pageID {
			return page.Title
		}
	}
	return ""
}

// take answers the pushes counted since the last take.
func (c *statePushes) take() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.n
	c.n = 0
	return n
}

// settle answers the pushes counted since the last take once want have
// arrived, or the deadline passed, and a moment more has brought no other:
// a push can come from a goroutine of the Manager's (refreshHistoryState).
func (c *statePushes) settle(want int) int {
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		c.mu.Lock()
		n := c.n
		c.mu.Unlock()
		if n >= want {
			break
		}
	}
	time.Sleep(50 * time.Millisecond)
	return c.take()
}

// A push carries every tab with its favicon, up to about 44 KB each, so a
// tool call that leaves the thread's state as it was pushes nothing; one
// that changes what a tab shows, or gives the thread its active page, pushes
// once.
func TestAToolCallPushesStateOnlyWhenItChangedIt(t *testing.T) {
	manager, _ := newLifecycleManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page := mustOpen(t, manager, access, "https://example.test/")
	p, _, err := manager.lookupOwnedPage(access, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	pushes := countStatePushes(manager)
	wait := func() {
		t.Helper()
		if _, err := manager.Wait(t.Context(), access, page.ID, "", 1); err != nil {
			t.Fatal(err)
		}
	}

	for range 3 {
		wait()
	}
	if n := pushes.take(); n != 0 {
		t.Fatalf("3 tool calls that changed nothing pushed %d states", n)
	}

	fake := p.driver.(*lifecycleDriver).pageDriver.(*fakePage)
	fake.mu.Lock()
	fake.title = "Retitled by the page"
	fake.mu.Unlock()
	wait()
	if n := pushes.take(); n != 1 {
		t.Fatalf("a tool call after the page retitled itself pushed %d states, want 1", n)
	}
	if tab, _ := findTab(manager, access, page.ID); tab.Title != "Retitled by the page" {
		t.Fatalf("tab title %q", tab.Title)
	}

	manager.mu.Lock()
	access.tb.session.ActivePageID = ""
	manager.mu.Unlock()
	wait()
	if n := pushes.take(); n != 1 {
		t.Fatalf("a tool call that gave the thread its active page pushed %d states, want 1", n)
	}
	if got := manager.CompanionState(access.Caller()).ActivePageID; got != page.ID {
		t.Fatalf("active page %q, want %q", got, page.ID)
	}
}

// An engine reports a page's address and title on every target info change,
// including ones that touched neither; only a change pushes.
func TestAnEngineInfoReportPushesStateOnlyWhenItChangedIt(t *testing.T) {
	manager, _ := newLifecycleManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page := mustOpen(t, manager, access, "https://example.test/")
	p, _, err := manager.lookupOwnedPage(access, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	handle := p.driver.Handle()
	pushes := countStatePushes(manager)

	manager.updatePageInfo(handle, "https://example.test/next", "https://example.test/next", 1)
	manager.updatePageInfo(handle, "https://example.test/next", "Next", 2)
	if n := pushes.settle(2); n != 2 {
		t.Fatalf("a commit and a title pushed %d states, want 2", n)
	}
	manager.updatePageInfo(handle, "https://example.test/next", "Next", 3)
	manager.updatePageInfo(handle, "https://example.test/next", "Next", 4)
	if n := pushes.settle(0); n != 0 {
		t.Fatalf("2 reports of the address and title already shown pushed %d states", n)
	}
	if tab, _ := findTab(manager, access, page.ID); tab.URL != "https://example.test/next" || tab.Title != "Next" {
		t.Fatalf("tab shows %q %q", tab.URL, tab.Title)
	}
}

// Two publishers of one write push it once: the second runs between the
// first's decision to push and its push, and finds nothing unpushed,
// because the decision took the write's record.
func TestTwoPublishersOfOneWritePushItOnce(t *testing.T) {
	manager, _ := newLifecycleManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page := mustOpen(t, manager, access, "https://example.test/")
	p, _, err := manager.lookupOwnedPage(access, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	pushes := countStatePushes(manager)
	p.commitInfo(pageUpdate{Location: &pageLocation{URL: "https://example.test/next", Title: "Next"}})
	first := true
	manager.publishDecided = func() {
		if first {
			first = false
			manager.publishPages(access.ThreadID)
		}
	}
	manager.publishPages(access.ThreadID)
	if n := pushes.take(); n != 1 {
		t.Fatalf("one write published twice pushed %d states, want 1", n)
	}
}

// retitle changes the fake page's title as the page would, with the engine's
// report of it still to come.
func retitle(t *testing.T, m *Manager, access Access, pageID, title string) *managedPage {
	t.Helper()
	p, _, err := m.lookupOwnedPage(access, pageID)
	if err != nil {
		t.Fatal(err)
	}
	fake := p.driver.(*lifecycleDriver).pageDriver.(*fakePage)
	fake.mu.Lock()
	fake.title = title
	fake.mu.Unlock()
	return p
}

// infoReaders are the calls that read a page's info only to store it.
var infoReaders = []struct {
	name string
	read func(*testing.T, *Manager, Access, string)
}{
	{"SelectPage", func(t *testing.T, m *Manager, access Access, pageID string) {
		if _, err := m.SelectPage(t.Context(), access, pageID); err != nil {
			t.Error(err)
		}
	}},
	{"Pages", func(t *testing.T, m *Manager, access Access, _ string) {
		if _, err := m.Pages(t.Context(), access); err != nil {
			t.Error(err)
		}
	}},
}

// A call that reads a page's info and stores it (SelectPage, Pages) pushes
// what it stored, so the engine's later report of the same title, which
// changes nothing, never leaves the tabs showing the old one.
func TestAReadInfoWriteReachesThePushBeforeItsEngineReport(t *testing.T) {
	for _, tc := range infoReaders {
		t.Run(tc.name, func(t *testing.T) {
			manager, _ := newLifecycleManager(t)
			access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
			page := mustOpen(t, manager, access, "https://example.test/")
			pushes := countStatePushes(manager)
			manager.emitThreadState(access.ThreadID)
			old := pushes.shownTitle(page.ID)

			p := retitle(t, manager, access, page.ID, "Retitled")
			tc.read(t, manager, access, page.ID)
			if got := pushes.shownTitle(page.ID); got != "Retitled" {
				t.Fatalf("after the read the tabs show %q (before: %q), want what it stored", got, old)
			}
			manager.updatePageInfo(p.driver.Handle(), page.URL, "Retitled", 1)
			if _, err := manager.Wait(t.Context(), access, page.ID, "", 1); err != nil {
				t.Fatal(err)
			}
			if got := pushes.shownTitle(page.ID); got != "Retitled" {
				t.Fatalf("the tabs show %q (before: %q), want the title the page and the tools have", got, old)
			}
		})
	}
}

// browser_pages answers which page is selected and when each was last
// opened, which no tab shows: listing pages and then an operation that
// changes nothing push nothing.
func TestListingPagesPushesNothingWhenNothingChanged(t *testing.T) {
	manager, _ := newLifecycleManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page := mustOpen(t, manager, access, "https://example.test/")
	pushes := countStatePushes(manager)
	for range 2 {
		if _, err := manager.Pages(t.Context(), access); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Wait(t.Context(), access, page.ID, "", 1); err != nil {
			t.Fatal(err)
		}
	}
	if n := pushes.take(); n != 0 {
		t.Fatalf("listing pages and an unchanged operation pushed %d states", n)
	}
	pages, err := manager.Pages(t.Context(), access)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 || !pages[0].Selected || pages[0].LastOpened == "" {
		t.Fatalf("browser_pages answers %+v", pages)
	}
	// The listing stays in the cache until the next read: browser_label_page
	// answers the page as the last browser_pages listed it.
	labeled, err := manager.LabelPage(t.Context(), access, page.ID, "docs")
	if err != nil {
		t.Fatal(err)
	}
	if !labeled.Selected || labeled.LastOpened != pages[0].LastOpened {
		t.Fatalf("browser_label_page after browser_pages answers %+v", labeled)
	}
	if _, err := manager.Wait(t.Context(), access, page.ID, "", 1); err != nil {
		t.Fatal(err)
	}
	relabeled, err := manager.LabelPage(t.Context(), access, page.ID, "docs")
	if err != nil {
		t.Fatal(err)
	}
	if relabeled.Selected || relabeled.LastOpened != "" {
		t.Fatalf("browser_label_page after another read answers %+v", relabeled)
	}
}

// A write that lands while a push builds the state, after the push copied
// the page, is not in that push; its record stays set, so its writer's own
// publish pushes it.
func TestAWriteDuringAPushBuildIsPushed(t *testing.T) {
	manager, _ := newLifecycleManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page := mustOpen(t, manager, access, "https://example.test/")
	p, _, err := manager.lookupOwnedPage(access, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	pushes := countStatePushes(manager)
	written := false
	manager.pushCopied = func() {
		if !written {
			written = true
			p.commitInfo(pageUpdate{Location: &pageLocation{URL: page.URL, Title: "Written mid-push"}})
		}
	}
	manager.emitThreadState(access.ThreadID)
	if !written {
		t.Fatal("no push copied the page")
	}
	if got := pushes.shownTitle(page.ID); got != page.Title {
		t.Fatalf("the push the write raced shows %q, want the title it copied, %q", got, page.Title)
	}
	// The writer publishes after its write, as every writer does.
	manager.publishPages(access.ThreadID)
	if got := pushes.shownTitle(page.ID); got != "Written mid-push" {
		t.Fatalf("after the writer's publish the newest push shows %q, want the write", got)
	}
}

// A restore loads the page before it joins the thread's tabs, so the push
// that registers it carries what the load read.
func TestARestoredPageIsPushedWithWhatItsLoadRead(t *testing.T) {
	manager, _ := newRestoreManager(t, t.TempDir(), ManagerOptions{})
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page := mustOpen(t, manager, access, "https://example.test/one")
	if err := manager.SuspendThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	pushes := countStatePushes(manager)
	if err := manager.ActivateCompanionPage(t.Context(), access, page.ID); err != nil {
		t.Fatal(err)
	}
	pushes.mu.Lock()
	newest := pushes.newest
	pushes.mu.Unlock()
	want, _ := findTab(manager, access, page.ID)
	if want.Suspended || want.Restoring || want.URL != page.URL || want.Title != page.Title {
		t.Fatalf("the restored tab reads %+v", want)
	}
	if len(newest.Pages) != 1 || newest.Pages[0] != want {
		t.Fatalf("the newest push carries %+v, want %+v", newest.Pages, want)
	}
}

// One client's read of the state (an attach, a call's answer) reaches only
// that client, so a write it carried is still pushed to every client.
func TestAClientReadLeavesAWriteForThePush(t *testing.T) {
	manager, _ := newLifecycleManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page := mustOpen(t, manager, access, "https://example.test/")
	p, _, err := manager.lookupOwnedPage(access, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	pushes := countStatePushes(manager)
	p.commitInfo(pageUpdate{Location: &pageLocation{URL: page.URL, Title: "Read by one client"}})
	if tab, _ := findTab(manager, access, page.ID); tab.Title != "Read by one client" {
		t.Fatalf("the read answers %q", tab.Title)
	}
	manager.publishPages(access.ThreadID)
	if got := pushes.shownTitle(page.ID); got != "Read by one client" {
		t.Fatalf("the newest push shows %q, want the write one client's read carried", got)
	}
}

// Writes racing pushes: whichever push takes a page's record, every write
// reaches a push, so the newest push shows the last title written. The
// push that takes the last write's record can be a history refresh's, on a
// goroutine of its own (refreshHistoryState), so it is waited for.
func TestAWriteRacingAPushIsPushed(t *testing.T) {
	manager, _ := newLifecycleManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page := mustOpen(t, manager, access, "https://example.test/")
	p, _, err := manager.lookupOwnedPage(access, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	handle := p.driver.Handle()
	pushes := countStatePushes(manager)
	const writes = 300
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range writes {
			manager.updatePageInfo(handle, page.URL, "title "+strconv.Itoa(i), uint64(i+1))
		}
	}()
	go func() {
		defer wg.Done()
		for range writes {
			manager.emitThreadState(access.ThreadID)
		}
	}()
	wg.Wait()
	want := "title " + strconv.Itoa(writes-1)
	for deadline := time.Now().Add(3 * time.Second); pushes.shownTitle(page.ID) != want; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the newest push shows %q, want %q", pushes.shownTitle(page.ID), want)
		}
	}
}

// An engine numbers its info reports where it orders them, and a native
// engine delivers each on its own goroutine, so an older report can reach
// the Manager after a newer one: the tab settles on the newest, and the
// older one pushes nothing.
func TestAnEngineInfoReportOlderThanTheShownOneIsDropped(t *testing.T) {
	manager, _ := newLifecycleManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page := mustOpen(t, manager, access, "https://example.test/")
	p, _, err := manager.lookupOwnedPage(access, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	handle := p.driver.Handle()
	pushes := countStatePushes(manager)

	// The commit's report, before the title was parsed, lands after the
	// title's.
	manager.updatePageInfo(handle, "https://example.test/next", "Next", 2)
	manager.updatePageInfo(handle, "https://example.test/next", "", 1)
	if n := pushes.take(); n != 1 {
		t.Fatalf("a newer report and a late older one pushed %d states, want 1", n)
	}
	if tab, _ := findTab(manager, access, page.ID); tab.URL != "https://example.test/next" || tab.Title != "Next" {
		t.Fatalf("tab shows %q %q, want the newer report", tab.URL, tab.Title)
	}
	// A repeat of the newest order is the same report.
	manager.updatePageInfo(handle, "https://example.test/other", "Other", 2)
	if tab, _ := findTab(manager, access, page.ID); tab.Title != "Next" {
		t.Fatalf("a repeated order replaced the title with %q", tab.Title)
	}
	manager.updatePageInfo(handle, "https://example.test/third", "Third", 3)
	if tab, _ := findTab(manager, access, page.ID); tab.URL != "https://example.test/third" || tab.Title != "Third" {
		t.Fatalf("tab shows %q %q after a newer report", tab.URL, tab.Title)
	}
}
