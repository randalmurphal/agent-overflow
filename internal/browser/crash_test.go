package browser

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/webview2host"

	"github.com/chromedp/cdproto/target"
)

func livePage(t *testing.T, m *Manager, access Access, pageID string) *managedPage {
	t.Helper()
	p, _, err := m.lookupOwnedPage(access, pageID)
	if err != nil {
		t.Fatalf("page %s is not live: %v", pageID, err)
	}
	return p
}

// A page whose process ended stays a tab, failed with the reason and its
// address, until its Reload brings it back there. Selecting it does not.
func TestCrashedPageStaysAFailedTabUntilItsReload(t *testing.T) {
	m, _, access, firstID, secondID := newPaneHostManager(t)
	events := recordEvents(m)
	p := livePage(t, m, access, firstID)
	handle := p.driver.Handle()

	m.pageCrashed(handle, "render-process-exited")
	tab, ok := findTab(m, access, firstID)
	if !ok {
		t.Fatal("the crashed page left the tab set")
	}
	if !tab.Suspended || !strings.Contains(tab.Error, "render-process-exited") || tab.URL != "https://example.test/first" {
		t.Fatalf("crashed tab = %+v, want suspended with the reason at its address", tab)
	}
	events.waitFor(t, "the crash pushed", func(e CompanionEvent) bool {
		for _, page := range e.Pages {
			if page.ID == firstID && page.Error != "" {
				return true
			}
		}
		return false
	})
	if _, _, err := m.lookupOwnedPage(access, firstID); !errors.Is(err, errPageNotFound) {
		t.Fatalf("the crashed engine page is still live: %v", err)
	}
	if fake := p.driver.(*fakePage); fake.ctx.Err() == nil {
		t.Fatal("the crashed engine page was not closed")
	}
	// A second report of the same death (WebView2's ProcessFailed beside
	// CDP's targetCrashed) changes nothing.
	m.pageCrashed(handle, "crashed")
	if again, _ := findTab(m, access, firstID); again.Error != tab.Error {
		t.Fatalf("a repeated report changed the tab: %+v", again)
	}

	// Selecting it, by click or by step, leaves it failed.
	if err := m.ActivateCompanionPage(t.Context(), access, firstID); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if err := m.StepCompanionPage(t.Context(), access, 1); err != nil {
		t.Fatalf("step: %v", err)
	}
	if err := m.StepCompanionPage(t.Context(), access, -1); err != nil {
		t.Fatalf("step back: %v", err)
	}
	if tab, _ := findTab(m, access, firstID); !tab.Suspended || tab.Error == "" {
		t.Fatalf("selecting the crashed tab restored it: %+v", tab)
	}

	if err := m.ReloadCompanionPage(t.Context(), access, firstID); err != nil {
		t.Fatalf("reload: %v", err)
	}
	tab, _ = findTab(m, access, firstID)
	if tab.Suspended || tab.Error != "" || tab.URL != "https://example.test/first" {
		t.Fatalf("reloaded tab = %+v, want live at its address", tab)
	}
	if ids := tabIDs(m, access); len(ids) != 2 || ids[0] != firstID || ids[1] != secondID {
		t.Fatalf("tabs after the reload = %v, want the crashed tab back in its place", ids)
	}
	// The restore is the reload: the page loaded once.
	if loads := livePage(t, m, access, firstID).docEpoch.Load(); loads != 1 {
		t.Fatalf("the reload loaded the page %d times, want once", loads)
	}
}

// An engine report with no address (WebKitGTK's view as its web process
// ends) leaves the tab's address, which the crash then keeps.
func TestPageInfoWithNoAddressKeepsTheTabsAddress(t *testing.T) {
	m, _, access, firstID, _ := newPaneHostManager(t)
	p := livePage(t, m, access, firstID)
	m.updatePageInfo(p.driver.Handle(), "", "")
	m.pageCrashed(p.driver.Handle(), "terminated")
	if tab, _ := findTab(m, access, firstID); tab.URL != "https://example.test/first" || tab.Title == "" {
		t.Fatalf("tab = %+v, want the page's last address and title", tab)
	}
}

// The crash cancels an operation holding the page, which gives the page up
// for the crash to close it.
func TestCrashEndsTheOperationHoldingThePage(t *testing.T) {
	m, _, access, firstID, _ := newPaneHostManager(t)
	p := livePage(t, m, access, firstID)
	fake := p.driver.(*fakePage)
	entered := make(chan struct{})
	fake.mu.Lock()
	fake.navigateHook = func(ctx context.Context, _ string) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	fake.mu.Unlock()
	done := async(func() error {
		_, err := m.Open(context.Background(), access, "https://example.test/slow", OpenOptions{PageID: firstID})
		return err
	})
	<-entered
	crashed := async(func() struct{} { m.pageCrashed(p.driver.Handle(), "crashed"); return struct{}{} })
	if err := await(t, done, "the held navigation"); err == nil {
		t.Fatal("the navigation of a crashed page succeeded")
	}
	await(t, crashed, "the crash")
	if tab, _ := findTab(m, access, firstID); !tab.Suspended || tab.Error == "" {
		t.Fatalf("tab = %+v, want crashed", tab)
	}
}

// A tool call on a crashed page restores it, as on any suspended page.
func TestToolCallRestoresACrashedPage(t *testing.T) {
	m, _, access, firstID, _ := newPaneHostManager(t)
	m.pageCrashed(livePage(t, m, access, firstID).driver.Handle(), "crashed")
	if _, err := m.History(t.Context(), access, firstID, "reload"); err != nil {
		t.Fatalf("history reload on the crashed page: %v", err)
	}
	if tab, _ := findTab(m, access, firstID); tab.Suspended || tab.Error != "" {
		t.Fatalf("tab = %+v, want restored", tab)
	}
}

// The failure is not saved: after a restart the tab is an ordinary
// suspended one, which presenting restores.
func TestCrashedTabReturnsAfterARestartAsASuspendedOne(t *testing.T) {
	dir := t.TempDir()
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	first := NewManager(dir, Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	page, err := first.Open(t.Context(), access, "https://example.test/a", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	first.pageCrashed(livePage(t, first, access, page.ID).driver.Handle(), "crashed")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := NewManager(dir, Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() { _ = second.Close() })
	tab, ok := findTab(second, access, page.ID)
	if !ok || !tab.Suspended || tab.Error != "" || tab.URL != "https://example.test/a" {
		t.Fatalf("tab after restart = %+v (found %v), want an ordinary suspended tab", tab, ok)
	}
}

// Each engine reports its own page's death and nothing else.
func TestHostedEngineReportsAProcessFailureByKind(t *testing.T) {
	for _, tc := range []struct {
		detail  string
		crashes bool
	}{
		{"render-process-exited", true},
		{"browser-process-exited", true},
		{"render-process-unresponsive", false},
		{"kind-6", false},
	} {
		t.Run(tc.detail, func(t *testing.T) {
			var crashed, closed []string
			engine, _ := newTestHostedEngine(t, stubRelay{}, engineEvents{
				PageCrashed: func(handle, reason string) { crashed = append(crashed, handle+" "+reason) },
				PageClosed:  func(handle string) { closed = append(closed, handle) },
			})
			engine.bind("page1", "TARGET-1")
			engine.Report("page1", webview2host.ReportProcessFailed, tc.detail)
			if len(closed) != 0 {
				t.Fatalf("a process failure retired the page: %v", closed)
			}
			want := 0
			if tc.crashes {
				want = 1
			}
			if len(crashed) != want || (want == 1 && crashed[0] != "page1 "+tc.detail) {
				t.Fatalf("PageCrashed saw %v, want %d report", crashed, want)
			}
		})
	}
}

func TestHeadlessProfileReportsItsOwnCrashedTarget(t *testing.T) {
	crashed := make(chan string, 4)
	p := newTargetTestProfile(t, engineEvents{PageCrashed: func(handle, reason string) { crashed <- handle + " " + reason }})
	other := &headlessProfile{engine: p.engine, handle: "other"}
	p.engine.bindPage("T-own", p)
	p.engine.bindPage("T-other", other)

	p.targetCrashed(target.EventTargetCrashed{TargetID: "T-unknown", Status: "crashed"})
	p.targetCrashed(target.EventTargetCrashed{TargetID: "T-other", Status: "crashed"})
	p.targetCrashed(target.EventTargetCrashed{TargetID: "T-own", Status: "oom", ErrorCode: 9})
	select {
	case got := <-crashed:
		if got != "T-own oom, code 9" {
			t.Fatalf("PageCrashed saw %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the profile's crashed target was never reported")
	}
	select {
	case got := <-crashed:
		t.Fatalf("a target the profile does not own was reported: %q", got)
	case <-time.After(50 * time.Millisecond):
	}
}
