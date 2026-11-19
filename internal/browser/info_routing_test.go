package browser

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// suspendedURL answers the address the suspension saved for pageID.
func suspendedURL(t *testing.T, m *Manager, pageID string) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.suspended[pageID]
	if rec == nil {
		t.Fatalf("page %s was not suspended", pageID)
	}
	return rec.url
}

// The page navigates itself to B while the suspension reads A, and B's
// report commits first: the suspension reads again and saves B.
func TestASuspendSavesTheNewerAddressReportedDuringItsRead(t *testing.T) {
	manager, _ := newLifecycleManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	p, fake := fencePage(t, manager, access)
	const b = "https://example.test/b"
	var calls atomic.Int32
	manager.infoRead = func(*managedPage) {
		if calls.Add(1) == 1 {
			// The read answered the second entry; the page moves on and the
			// engine reports it before the read commits.
			fake.SameDocumentNavigation(b)
			p.commitInfo(pageUpdate{Location: &pageLocation{URL: b, Title: fakeTitle(b)}, Report: 1})
		}
	}
	if err := manager.SuspendThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	if got := suspendedURL(t, manager, p.id); got != b {
		t.Fatalf("the suspension saved %q, want the address the page reached, %q", got, b)
	}
}

// A page that reports a new address during every read holds the suspension
// for maxSettleReads reads, which then saves the last one.
func TestASuspendOfAPageThatKeepsNavigatingIsBounded(t *testing.T) {
	manager, _ := newLifecycleManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	p, fake := fencePage(t, manager, access)
	var calls atomic.Int32
	address := func(n int32) string { return fmt.Sprintf("https://example.test/loop/%d", n) }
	manager.infoRead = func(*managedPage) {
		n := calls.Add(1)
		fake.SameDocumentNavigation(address(n))
		p.commitInfo(pageUpdate{Location: &pageLocation{URL: address(n), Title: "loop"}, Report: uint64(n)})
	}
	if err := manager.SuspendThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != maxSettleReads {
		t.Fatalf("the suspension read %d times, want %d", n, maxSettleReads)
	}
	// The last read answered the address the page reached before it.
	if got, want := suspendedURL(t, manager, p.id), address(maxSettleReads-1); got != want {
		t.Fatalf("the suspension saved %q, want its last read, %q", got, want)
	}
}

// adoptingProfile attaches a popup as a fake page answering to its handle,
// running onAttach inside AttachPage and onViewport inside the popup's first
// layout.
type adoptingProfile struct {
	*fakeProfile
	onAttach, onViewport func()
}

func (p *adoptingProfile) AttachPage(ctx context.Context, handle string, hooks pageHooks) (pageDriver, error) {
	driver, err := p.NewPage(ctx, hooks)
	if err != nil {
		return nil, err
	}
	page := driver.(*fakePage)
	page.handle = handle
	if p.onViewport != nil {
		var once sync.Once
		page.viewportHook = func(int, int) { once.Do(p.onViewport) }
	}
	if p.onAttach != nil {
		p.onAttach()
	}
	return page, nil
}

// A CDP engine reports a popup by its handle while the Manager adopts it,
// before the popup answers to that handle: the report ends in the registered
// tab, and an older report delivered after the registration is dropped.
func TestAReportDuringAPopupAdoptionReachesItsTab(t *testing.T) {
	const final = "https://example.test/final"
	for _, during := range []string{"AttachPage", "applyViewport"} {
		t.Run(during, func(t *testing.T) {
			manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
			t.Cleanup(func() {
				if err := manager.Close(); err != nil {
					t.Errorf("close manager: %v", err)
				}
			})
			access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
			opener := mustOpen(t, manager, access, "https://example.test/")
			report := func() { manager.updatePageInfo("popup", final, "Final", 2) }
			manager.mu.Lock()
			var profile *adoptingProfile
			for _, scope := range manager.scopes {
				profile = &adoptingProfile{fakeProfile: scope.profile.(*fakeProfile)}
				scope.profile = profile
			}
			manager.mu.Unlock()
			if during == "AttachPage" {
				profile.onAttach = report
			} else {
				profile.onViewport = report
			}
			manager.adoptPopup(enginePopup{Profile: profile.Handle(), Opener: paneHandle(manager, access, opener.ID), Handle: "popup", URL: "https://example.test/initial"})
			manager.mu.Lock()
			popup := manager.pageByHandleLocked("popup")
			manager.mu.Unlock()
			if popup == nil {
				t.Fatal("the popup was not adopted")
			}
			if tab, _ := findTab(manager, access, popup.id); tab.URL != final || tab.Title != "Final" {
				t.Fatalf("the adopted tab shows %q %q, want the report made %s", tab.URL, tab.Title, during)
			}
			manager.updatePageInfo("popup", "https://example.test/older", "Older", 1)
			if tab, _ := findTab(manager, access, popup.id); tab.URL != final {
				t.Fatalf("an older report delivered after the adoption replaced the tab's %q with %q", final, tab.URL)
			}
		})
	}
}

// A CDP engine's report of a page whose engine page the Manager is still
// creating, before it is attached, moves that page's order fence.
func TestAReportBeforeACreatedPageIsAttachedMovesItsFence(t *testing.T) {
	manager, engine := newLifecycleManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	const final = "https://example.test/final"
	engine.mu.Lock()
	engine.onNewFakePage = func(page *fakePage) {
		page.mu.Lock()
		page.url, page.title = final, "Final"
		handle := page.handle
		page.mu.Unlock()
		manager.updatePageInfo(handle, final, "Final", 2)
	}
	engine.mu.Unlock()
	page, err := manager.NewPage(t.Context(), access)
	if err != nil {
		t.Fatal(err)
	}
	manager.updatePageInfo(paneHandle(manager, access, page.ID), "https://example.test/older", "Older", 1)
	if tab, _ := findTab(manager, access, page.ID); tab.URL != final {
		t.Fatalf("the tab shows %q, want %q: an older report replaced the one made before the attach", tab.URL, final)
	}
}

// The same through the hosted engine's browser connection: its target info
// changes for a page the Manager has not attached are held for that page.
func TestHostedReportsBeforeTheAttachReachThePage(t *testing.T) {
	manager, engine := newLifecycleManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	endpoint := startFakeCDPEndpoint(t, nil, nil)
	hosted, _ := newTestHostedEngine(t, stubRelay{url: endpoint.wsURL}, engineEvents{
		PageInfoChanged: manager.updatePageInfo,
		PageClosed:      func(string) {},
		PageCrashed:     func(string, string) {},
	})
	hosted.attachTimeout = 5 * time.Second
	if err := hosted.bind("page1", "TARGET-1", context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := hosted.ensureBrowser(); err != nil {
		t.Fatalf("ensureBrowser against the fake endpoint: %v", err)
	}
	defer hosted.Interrupt()
	const final = "https://example.test/final"
	for _, address := range []string{"https://example.test/intermediate", final} {
		endpoint.send(map[string]any{"method": "Target.targetInfoChanged", "params": map[string]any{"targetInfo": map[string]any{
			"targetId": "TARGET-1", "type": "page", "title": "t", "url": address, "attached": true, "canAccessOpener": false,
		}}})
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		manager.mu.Lock()
		held, ok := manager.heldInfo.get("page1")
		manager.mu.Unlock()
		if ok && held.url == final {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Manager holds %+v for the unattached page, want the final report", held)
		}
		time.Sleep(5 * time.Millisecond)
	}
	engine.mu.Lock()
	engine.onNewFakePage = func(page *fakePage) {
		page.mu.Lock()
		page.handle, page.url, page.title = "page1", final, "t"
		page.mu.Unlock()
	}
	engine.mu.Unlock()
	page, err := manager.NewPage(t.Context(), access)
	if err != nil {
		t.Fatal(err)
	}
	// The intermediate report, overtaken on its way.
	manager.updatePageInfo("page1", "https://example.test/intermediate", "t", 1)
	if tab, _ := findTab(manager, access, page.ID); tab.URL != final {
		t.Fatalf("the tab shows %q, want %q", tab.URL, final)
	}
}
