package browser

import "testing"

// documentTitlePage opens a page at a and answers it with a push counter.
func documentTitlePage(t *testing.T, a string) (*Manager, *managedPage, *statePushes) {
	t.Helper()
	manager, _ := newLifecycleManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page := mustOpen(t, manager, access, a)
	p, _, err := manager.lookupOwnedPage(access, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	return manager, p, countStatePushes(manager)
}

// A CDP engine reports a navigation with the browser's placeholder title
// and nothing when the document then sets its own; the document's report
// is what reaches the tab, and it is pushed.
func TestADocumentTitleReachesTheTabAfterTheNavigationReport(t *testing.T) {
	const a = "https://example.test/a"
	manager, p, pushes := documentTitlePage(t, a)
	manager.pageInfoReport(p, a, "example.test/a", 1000)
	manager.pageDocumentTitle(p, a, "Set by script")
	if got := p.cachedInfo().Title; got != "Set by script" {
		t.Fatalf("the tab shows %q, want the document's title", got)
	}
	if got := pushes.shownTitle(p.id); got != "Set by script" {
		t.Fatalf("the newest push shows %q, want the document's title", got)
	}
	// The same title again changes nothing and pushes nothing.
	pushes.take()
	manager.pageDocumentTitle(p, a, "Set by script")
	if n := pushes.take(); n != 0 {
		t.Fatalf("an unchanged title pushed %d times", n)
	}
}

// The document's report and the engine's navigation report arrive on
// separate subscriptions, so the document of b can report before the
// navigation to b does. Its title waits for that address and then wins over
// the report's placeholder; it never moves the tab's address itself.
func TestADocumentTitleBeforeItsNavigationReportWaitsForTheAddress(t *testing.T) {
	const a, b = "https://example.test/a", "https://example.test/b"
	manager, p, pushes := documentTitlePage(t, a)
	before := p.cachedInfo()
	manager.pageDocumentTitle(p, b, "Page b")
	if got := p.cachedInfo(); got.URL != before.URL || got.Title != before.Title {
		t.Fatalf("a title of %s changed the tab at %s to %+v", b, a, got)
	}
	manager.pageInfoReport(p, b, "example.test/b", 1000)
	if got := p.cachedInfo(); got.URL != b || got.Title != "Page b" {
		t.Fatalf("after the navigation report the tab shows %q at %q, want %q at %q", got.Title, got.URL, "Page b", b)
	}
	if got := pushes.shownTitle(p.id); got != "Page b" {
		t.Fatalf("the newest push shows %q, want %q", got, "Page b")
	}
}

// A title the document at a reports after the page left a (its last
// change, delivered late) is not shown on b, and b's own title stands.
func TestALateDocumentTitleOfTheLastAddressChangesNothing(t *testing.T) {
	const a, b = "https://example.test/a", "https://example.test/b"
	manager, p, _ := documentTitlePage(t, a)
	manager.pageInfoReport(p, b, "example.test/b", 1000)
	manager.pageDocumentTitle(p, b, "Page b")
	manager.pageDocumentTitle(p, a, "Late title of a")
	if got := p.cachedInfo(); got.URL != b || got.Title != "Page b" {
		t.Fatalf("the tab shows %q at %q, want %q at %q", got.Title, got.URL, "Page b", b)
	}
}

// A read begun before a document's title report answers the older title,
// and its commit does not undo the report.
func TestAReadBegunBeforeADocumentTitleDoesNotUndoIt(t *testing.T) {
	const a = "https://example.test/a"
	manager, p, _ := documentTitlePage(t, a)
	stamp := p.readStamp()
	manager.pageDocumentTitle(p, a, "Newer")
	manager.commitRead(p, pageUpdate{Location: &pageLocation{URL: a, Title: "Older"}}, stamp)
	if got := p.cachedInfo().Title; got != "Newer" {
		t.Fatalf("the tab shows %q, want the document's newer title", got)
	}
}
