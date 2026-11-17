package browser

import (
	"sync"
	"testing"
	"time"
)

// basePane is the pane surface color every test here reports.
const basePane = "#1e1e2e"

// baseRecorder records the base color of every SetPageBounds by handle.
type baseRecorder struct {
	mu   sync.Mutex
	last map[string]string
	n    int
}

func (r *baseRecorder) note(handle, bg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last[handle] = bg
	r.n++
}

func (r *baseRecorder) base(handle string) (string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last[handle], r.n
}

// newBaseManager presents a thread's pane over the recording engine: the
// first page is at a document's address, the second is a blank new tab and
// is the one shown.
func newBaseManager(t *testing.T) (*Manager, *recordingPaneHost, *baseRecorder, Access, string, string) {
	t.Helper()
	manager, engine, access, firstID, secondID := newPaneHostManager(t)
	rec := &baseRecorder{last: map[string]string{}}
	engine.mu.Lock()
	engine.onBoundsHandle = rec.note
	engine.mu.Unlock()
	show := true
	if _, err := manager.Visibility(t.Context(), access, &show, secondID); err != nil {
		t.Fatalf("visibility: %v", err)
	}
	mount, err := manager.AttachPane(access)
	if err != nil {
		t.Fatalf("attach pane: %v", err)
	}
	rect := PaneRect{X: 10, Y: 20, Width: 800, Height: 600, ViewportWidth: 1920, ViewportHeight: 1080, Visible: true, Background: basePane}
	if err := manager.SetPaneRect(mount.ID, rect); err != nil {
		t.Fatalf("set pane rect: %v", err)
	}
	return manager, engine, rec, access, firstID, secondID
}

func waitForBase(t *testing.T, rec *baseRecorder, handle, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := rec.base(handle)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("page %s is painted on %q, want %q", handle, got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A page showing a document is painted on the document base, and a blank
// new tab on the pane's own color, whichever is shown first and however the
// user switches between them.
func TestAPresentedPageIsPaintedOnTheBaseItsAddressCallsFor(t *testing.T) {
	manager, _, rec, access, firstID, secondID := newBaseManager(t)
	first, second := paneHandle(manager, access, firstID), paneHandle(manager, access, secondID)
	waitForBase(t, rec, second, basePane)
	if err := manager.ActivateCompanionPage(t.Context(), access, firstID); err != nil {
		t.Fatal(err)
	}
	waitForBase(t, rec, first, documentBase)
	if err := manager.ActivateCompanionPage(t.Context(), access, secondID); err != nil {
		t.Fatal(err)
	}
	waitForBase(t, rec, second, basePane)
	if err := manager.ActivateCompanionPage(t.Context(), access, firstID); err != nil {
		t.Fatal(err)
	}
	waitForBase(t, rec, first, documentBase)
}

// The page shown navigating between a document and none changes its base at
// once, from the engine's report alone; a navigation that keeps it showing a
// document does no presentation work.
func TestAShownPageNavigatingChangesItsBase(t *testing.T) {
	manager, _, rec, access, _, secondID := newBaseManager(t)
	handle := paneHandle(manager, access, secondID)
	waitForBase(t, rec, handle, basePane)
	p, _, err := manager.lookupOwnedPage(access, secondID)
	if err != nil {
		t.Fatal(err)
	}
	manager.pageInfoReport(p, "https://example.test/doc", "example.test/doc", 1000)
	waitForBase(t, rec, handle, documentBase)
	_, before := rec.base(handle)
	manager.pageInfoReport(p, "https://example.test/other", "example.test/other", 1001)
	if _, after := rec.base(handle); after != before {
		t.Fatalf("a navigation between documents placed the page %d more times", after-before)
	}
	manager.pageInfoReport(p, "about:blank", "about:blank", 1002)
	waitForBase(t, rec, handle, basePane)
}

// A read that finds the shown page at another kind of address (the engine's
// report was lost or is still on its way) changes the base the same way.
func TestAReadOfTheShownPagesAddressChangesItsBase(t *testing.T) {
	manager, _, rec, access, _, secondID := newBaseManager(t)
	handle := paneHandle(manager, access, secondID)
	waitForBase(t, rec, handle, basePane)
	p, _, err := manager.lookupOwnedPage(access, secondID)
	if err != nil {
		t.Fatal(err)
	}
	manager.commitRead(p, pageUpdate{Location: &pageLocation{URL: "https://example.test/doc"}}, p.readStamp())
	waitForBase(t, rec, handle, documentBase)
}

// A hidden page's navigation does no presentation work.
func TestAHiddenPageNavigatingIsNotPlaced(t *testing.T) {
	manager, _, rec, access, firstID, secondID := newBaseManager(t)
	waitForBase(t, rec, paneHandle(manager, access, secondID), basePane)
	p, _, err := manager.lookupOwnedPage(access, firstID)
	if err != nil {
		t.Fatal(err)
	}
	_, before := rec.base(p.driver.Handle())
	manager.pageInfoReport(p, "about:blank", "about:blank", 1000)
	if _, after := rec.base(p.driver.Handle()); after != before {
		t.Fatalf("a hidden page's navigation placed a page %d times", after-before)
	}
}

func TestPageShowsDocument(t *testing.T) {
	for url, want := range map[string]bool{
		"":                        false,
		"about:blank":             false,
		"about:blank#x":           false,
		"about:blank?x":           false,
		"about:blankish":          true,
		"https://example.test/":   true,
		"file:///tmp/a.html":      true,
		"data:text/html,<p>hi":    true,
		"chrome-error://chromewe": true,
	} {
		if got := pageShowsDocument(url); got != want {
			t.Errorf("pageShowsDocument(%q) = %v, want %v", url, got, want)
		}
	}
}
