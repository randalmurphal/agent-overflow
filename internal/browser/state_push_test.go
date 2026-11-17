package browser

import (
	"sync"
	"testing"
)

// statePushes counts the thread state pushes a Manager publishes.
type statePushes struct {
	mu sync.Mutex
	n  int
}

func countStatePushes(m *Manager) *statePushes {
	c := &statePushes{}
	m.SetEventSink(func(event CompanionEvent) {
		if event.Kind == "state" {
			c.mu.Lock()
			c.n++
			c.mu.Unlock()
		}
	})
	return c
}

// take answers the pushes counted since the last take.
func (c *statePushes) take() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.n
	c.n = 0
	return n
}

// A push carries every tab with its favicon, up to about 44 KB each, so a
// tool call that leaves the thread's state as it was pushes nothing; one
// that changes what a tab shows, or gives the thread its active page, pushes
// once.
func TestAToolCallPushesStateOnlyWhenItChangedIt(t *testing.T) {
	manager, _ := newLifecycleManager(t)
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
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
	session := manager.sessions[access.ThreadID]
	session.ActivePageID = ""
	manager.sessions[access.ThreadID] = session
	manager.mu.Unlock()
	wait()
	if n := pushes.take(); n != 1 {
		t.Fatalf("a tool call that gave the thread its active page pushed %d states, want 1", n)
	}
	if got := manager.CompanionState(access).ActivePageID; got != page.ID {
		t.Fatalf("active page %q, want %q", got, page.ID)
	}
}

// An engine reports a page's address and title on every target info change,
// including ones that touched neither; only a change pushes.
func TestAnEngineInfoReportPushesStateOnlyWhenItChangedIt(t *testing.T) {
	manager, _ := newLifecycleManager(t)
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	page := mustOpen(t, manager, access, "https://example.test/")
	p, _, err := manager.lookupOwnedPage(access, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	handle := p.driver.Handle()
	pushes := countStatePushes(manager)

	manager.updatePageInfo(handle, "https://example.test/next", "https://example.test/next", 1)
	manager.updatePageInfo(handle, "https://example.test/next", "Next", 2)
	if n := pushes.take(); n != 2 {
		t.Fatalf("a commit and a title pushed %d states, want 2", n)
	}
	manager.updatePageInfo(handle, "https://example.test/next", "Next", 3)
	manager.updatePageInfo(handle, "https://example.test/next", "Next", 4)
	if n := pushes.take(); n != 0 {
		t.Fatalf("2 reports of the address and title already shown pushed %d states", n)
	}
	if tab, _ := findTab(manager, access, page.ID); tab.URL != "https://example.test/next" || tab.Title != "Next" {
		t.Fatalf("tab shows %q %q", tab.URL, tab.Title)
	}
}

// An engine numbers its info reports where it orders them, and a native
// engine delivers each on its own goroutine, so an older report can reach
// the Manager after a newer one: the tab settles on the newest, and the
// older one pushes nothing.
func TestAnEngineInfoReportOlderThanTheShownOneIsDropped(t *testing.T) {
	manager, _ := newLifecycleManager(t)
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
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
