package browser

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// basePane and basePaneDark are the pane surface colors the tests report.
const (
	basePane     = "#1e1e2e"
	basePaneDark = "#303446"
)

// baseLog records, in order, every base the engine is told and every show.
type baseLog struct {
	mu     sync.Mutex
	events []string
	last   map[string]string
	bases  map[string]int
}

func (l *baseLog) base(handle, color string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, "base:"+handle+":"+color)
	l.last[handle] = color
	l.bases[handle]++
}

func (l *baseLog) show(handle string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, "show:"+handle)
}

// of answers the handle's last base and how many it was told.
func (l *baseLog) of(handle string) (string, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last[handle], l.bases[handle]
}

func (l *baseLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

// newBaseEngine is a Manager over the recording engine with the base log
// wired before any page exists.
func newBaseEngine(t *testing.T) (*Manager, *recordingPaneHost, *baseLog, Access) {
	t.Helper()
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	log :=&baseLog{last: map[string]string{}, bases: map[string]int{}}
	engine := &recordingPaneHost{fakeEngine: manager.engine.(*fakeEngine), onBase: log.base, onShow: log.show}
	engine.onBounds = func(placement PanePlacement) {
		if placement.Rect.Background != "" {
			t.Errorf("a placement carries the color %q", placement.Rect.Background)
		}
	}
	manager.engine = engine
	return manager, engine, log, Access{ThreadID: "thread", Workspace: t.TempDir()}
}

// newBaseManager opens a document page (first) and a blank new tab
// (second), shows shownID's companion and reports the pane at basePane.
func newBaseManager(t *testing.T, showFirst bool) (*Manager, *baseLog, Access, string, string, string) {
	t.Helper()
	manager, _, log, access := newBaseEngine(t)
	first, err := manager.Open(t.Context(), access, "https://example.test/first", OpenOptions{})
	if err != nil {
		t.Fatalf("open first page: %v", err)
	}
	second, err := manager.NewCompanionPage(t.Context(), access, "")
	if err != nil {
		t.Fatalf("open second page: %v", err)
	}
	shown := second.ID
	if showFirst {
		shown = first.ID
	}
	show := true
	if _, err := manager.Visibility(t.Context(), access, &show, shown); err != nil {
		t.Fatalf("visibility: %v", err)
	}
	mount, err := manager.AttachPane(access)
	if err != nil {
		t.Fatalf("attach pane: %v", err)
	}
	if err := manager.SetPaneRect(mount.ID, baseRect(basePane)); err != nil {
		t.Fatalf("set pane rect: %v", err)
	}
	return manager, log, access, first.ID, second.ID, mount.ID
}

func baseRect(color string) PaneRect {
	return PaneRect{X: 10, Y: 20, Width: 800, Height: 600, ViewportWidth: 1920, ViewportHeight: 1080, Visible: true, Background: color}
}

func waitForBase(t *testing.T, log *baseLog, handle, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := log.of(handle)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("page %s has base %q, want %q", handle, got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func ownedPage(t *testing.T, m *Manager, access Access, pageID string) *managedPage {
	t.Helper()
	p, _, err := m.lookupOwnedPage(access, pageID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A page showing a document has the document base, and a blank new tab the
// pane's own color, whichever is shown.
func TestAPageHasTheBaseItsAddressCallsFor(t *testing.T) {
	manager, log, access, firstID, secondID, _ := newBaseManager(t, false)
	first, second := paneHandle(manager, access, firstID), paneHandle(manager, access, secondID)
	waitForBase(t, log, first, documentBase)
	waitForBase(t, log, second, basePane)
	if err := manager.ActivateCompanionPage(t.Context(), access, firstID); err != nil {
		t.Fatal(err)
	}
	if err := manager.ActivateCompanionPage(t.Context(), access, secondID); err != nil {
		t.Fatal(err)
	}
	if _, n := log.of(first); n != 1 {
		t.Fatalf("the document page was told %d bases, want 1: switching tabs is no base work", n)
	}
	if _, n := log.of(second); n != 1 {
		t.Fatalf("the blank page was told %d bases, want 1", n)
	}
}

// A hidden page navigating between a document and none changes its base at
// once, from the engine's report alone, and places nothing.
func TestAHiddenPageNavigatingChangesItsBase(t *testing.T) {
	manager, log, access, firstID, secondID, _ := newBaseManager(t, true)
	handle := paneHandle(manager, access, secondID)
	waitForBase(t, log, handle, basePane)
	waitForBase(t, log, paneHandle(manager, access, firstID), documentBase)
	p := ownedPage(t, manager, access, secondID)
	engine := manager.engine.(*recordingPaneHost)
	engine.take()
	manager.pageInfoReport(p, "https://example.test/doc", "example.test/doc", 1000)
	waitForBase(t, log, handle, documentBase)
	_, before := log.of(handle)
	manager.pageInfoReport(p, "https://example.test/other", "example.test/other", 1001)
	if _, after := log.of(handle); after != before {
		t.Fatalf("a navigation between documents told the engine %d more bases", after-before)
	}
	manager.pageInfoReport(p, "about:blank", "about:blank", 1002)
	waitForBase(t, log, handle, basePane)
	for _, call := range engine.take() {
		if strings.HasSuffix(call, handle) && !strings.HasPrefix(call, "hide:") {
			t.Fatalf("a hidden page's navigation presented it: %q", call)
		}
	}
}

// A read that finds a hidden page at another kind of address (the engine's
// report was lost or is still on its way) changes the base the same way.
func TestAReadOfAHiddenPagesAddressChangesItsBase(t *testing.T) {
	manager, log, access, _, secondID, _ := newBaseManager(t, true)
	handle := paneHandle(manager, access, secondID)
	waitForBase(t, log, handle, basePane)
	p := ownedPage(t, manager, access, secondID)
	manager.commitRead(p, pageUpdate{Location: &pageLocation{URL: "https://example.test/doc"}}, p.readStamp())
	waitForBase(t, log, handle, documentBase)
}

// A pane color change reaches every blank page of the thread, hidden or
// shown, and no document page.
func TestAPaneColorChangeReachesHiddenBlankPages(t *testing.T) {
	manager, log, access, firstID, secondID, mountID := newBaseManager(t, true)
	first, second := paneHandle(manager, access, firstID), paneHandle(manager, access, secondID)
	waitForBase(t, log, second, basePane)
	_, documentBases := log.of(first)
	_, blankBases := log.of(second)
	if err := manager.SetPaneRect(mountID, baseRect(basePane)); err != nil {
		t.Fatal(err)
	}
	if _, n := log.of(second); n != blankBases {
		t.Fatalf("an unchanged pane color told the engine %d more bases", n-blankBases)
	}
	if err := manager.SetPaneRect(mountID, baseRect(basePaneDark)); err != nil {
		t.Fatal(err)
	}
	waitForBase(t, log, second, basePaneDark)
	if _, n := log.of(first); n != documentBases {
		t.Fatalf("a pane color change told the document page %d bases", n-documentBases)
	}
	// A rect that is no color keeps the last one.
	if err := manager.SetPaneRect(mountID, baseRect("transparent")); err != nil {
		t.Fatal(err)
	}
	if got, _ := log.of(second); got != basePaneDark {
		t.Fatalf("a rect with no color changed the base to %q", got)
	}
}

// Only the presenting pane colors the thread's blank pages: a second mount's
// color counts once the first detaches and it presents.
func TestThePresentingPanesColorIsTheBase(t *testing.T) {
	manager, log, access, _, secondID, mountID := newBaseManager(t, true)
	handle := paneHandle(manager, access, secondID)
	waitForBase(t, log, handle, basePane)
	later, err := manager.AttachPane(access)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetPaneRect(later.ID, baseRect(basePaneDark)); err != nil {
		t.Fatal(err)
	}
	if got, _ := log.of(handle); got != basePane {
		t.Fatalf("a mount that does not present changed the base to %q", got)
	}
	manager.DetachPane(mountID)
	waitForBase(t, log, handle, basePaneDark)
}

// A restored page takes its base before it is shown: the pane color it never
// saw, kept while it was suspended.
func TestARestoredPageTakesItsBaseBeforeItShows(t *testing.T) {
	manager, log, access, _, secondID, mountID := newBaseManager(t, true)
	suspendTab(t, manager, access, secondID)
	if err := manager.SetPaneRect(mountID, baseRect(basePaneDark)); err != nil {
		t.Fatal(err)
	}
	if err := manager.ActivateCompanionPage(t.Context(), access, secondID); err != nil {
		t.Fatal(err)
	}
	handle := paneHandle(manager, access, secondID)
	waitForBase(t, log, handle, basePaneDark)
	events := log.snapshot()
	base := slices.Index(events, "base:"+handle+":"+basePaneDark)
	show := slices.Index(events, "show:"+handle)
	if show >= 0 && show < base {
		t.Fatalf("the restored page showed before its base: %v", events)
	}
}

// A blank page of a thread whose pane never reported a color has no base:
// the engine keeps its own white. A document page has the document base.
func TestABlankPageWithNoPaneColorHasNoBase(t *testing.T) {
	manager, _, log, access := newBaseEngine(t)
	blank, err := manager.NewCompanionPage(t.Context(), access, "")
	if err != nil {
		t.Fatal(err)
	}
	document, err := manager.Open(t.Context(), access, "https://example.test/doc", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitForBase(t, log, paneHandle(manager, access, document.ID), documentBase)
	if got, n := log.of(paneHandle(manager, access, blank.ID)); n != 0 {
		t.Fatalf("a blank page with no pane color was told %q", got)
	}
}

// A page that is not in its thread's tabs (being created, restored or
// closed) is told no base; pageAdded applies it once it is.
func TestAPageNotInItsTabsIsToldNoBase(t *testing.T) {
	manager, log, access, firstID, _, _ := newBaseManager(t, false)
	listed := ownedPage(t, manager, access, firstID)
	waitForBase(t, log, listed.driver.Handle(), documentBase)
	_, before := log.of(listed.driver.Handle())
	unlisted := newManagedPage(access)
	unlisted.attach(listed.driver)
	unlisted.info.URL = "https://example.test/unlisted"
	manager.applyPageBase(unlisted)
	if _, after := log.of(listed.driver.Handle()); after != before {
		t.Fatalf("a page not in its tabs was told %d bases", after-before)
	}
}

// The pane color is the thread's: another thread's pane does not color it,
// and closing the thread forgets it.
func TestAPaneColorIsItsThreads(t *testing.T) {
	manager, log, access, _, secondID, _ := newBaseManager(t, true)
	waitForBase(t, log, paneHandle(manager, access, secondID), basePane)
	other := Access{ThreadID: "other", Workspace: t.TempDir()}
	blank, err := manager.NewCompanionPage(t.Context(), other, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, n := log.of(paneHandle(manager, other, blank.ID)); n != 0 {
		t.Fatalf("another thread's blank page was told %q", got)
	}
	if err := manager.CloseThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	_, kept := manager.paneColors[access.ThreadID]
	manager.mu.Unlock()
	if kept {
		t.Fatal("a closed thread's pane color is kept")
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

func TestPaneColor(t *testing.T) {
	for value, want := range map[string]string{
		"#1E1E2E": "#1e1e2e", "#ffffff": "#ffffff",
		"": "", "#fff": "", "transparent": "", "#12345g": "", "rgb(0,0,0)": "", "#1e1e2e00": "",
	} {
		got, ok := paneColor(value)
		if got != want || ok != (want != "") {
			t.Errorf("paneColor(%q) = %q, %v; want %q", value, got, ok, want)
		}
	}
}
