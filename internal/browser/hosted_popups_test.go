package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/webview2host"

	"github.com/chromedp/cdproto/target"
)

// A popup becomes a tab of its opener's thread at the address the launcher
// reported, attached at the target the launcher paired with it.
func TestHostedPopupIsAdoptedIntoItsOpenersThread(t *testing.T) {
	l := newPopupLauncher(t)
	access := popupTestAccess(t, "thread")
	_, opener := l.open(access)
	popupID, targetID := l.popup(opener, "https://example.test/popup")
	p := l.awaitTab(access.ThreadID, popupID, true)
	if got := l.tabInfo(p); got.URL != "https://example.test/popup" {
		t.Errorf("the popup's tab is at %q", got.URL)
	}
	if got, ok := l.engine.pageForTarget(targetID); !ok || got != popupID {
		t.Errorf("target %s answers page %q, want %s", targetID, got, popupID)
	}
	l.settledEngine()

	// Its close is the launcher's, as any page's.
	l.report(popupID, webview2host.ReportClosed, "")
	l.awaitTabGone(access.ThreadID, popupID)

	// The browser may report a popup's target before the launcher reports
	// the popup; the tab takes what it said.
	l.engine.targetInfoChanged(target.EventTargetInfoChanged{TargetInfo: &target.Info{
		TargetID: "PT-2", Type: "page", URL: "https://example.test/early", Title: "early",
	}})
	second, _ := l.popup(opener, "https://example.test/popup")
	if got := l.tabInfo(l.awaitTab(access.ThreadID, second, true)); got.Title != "early" || got.URL != "https://example.test/early" {
		t.Errorf("the popup's tab reads %q at %q, want the target's report", got.Title, got.URL)
	}
}

// A popup's own popup is adopted into the same thread.
func TestHostedPopupOfAPopupIsAdopted(t *testing.T) {
	l := newPopupLauncher(t)
	access := popupTestAccess(t, "thread")
	_, opener := l.open(access)
	first, _ := l.popup(opener, "https://example.test/1")
	l.awaitTab(access.ThreadID, first, true)
	second, _ := l.popup(first, "https://example.test/2")
	l.awaitTab(access.ThreadID, second, true)
	if n := len(l.tabs(access.ThreadID)); n != 3 {
		t.Errorf("%d tabs, want 3", n)
	}
	l.settledEngine()
}

// A popup past any of the Manager's caps is closed, as on every engine.
func TestHostedPopupPastACapIsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		// fill opens pages and answers the opener's thread and handle.
		fill func(l *popupLauncher) (Access, string)
	}{
		{"the thread's", func(l *popupLauncher) (Access, string) {
			access := popupTestAccess(l.t, "thread")
			var opener string
			for range maxPagesPerThread {
				_, opener = l.open(access)
			}
			return access, opener
		}},
		// The opener's thread has room in these two; other threads fill the
		// workspace or the process.
		{"the workspace's", func(l *popupLauncher) (Access, string) {
			access := popupTestAccess(l.t, "opener")
			_, opener := l.open(access)
			for i := range maxPagesPerWorkspace - 1 {
				l.open(Access{ThreadID: fmt.Sprintf("thread-%d", i/maxPagesPerThread), Workspace: access.Workspace})
			}
			return access, opener
		}},
		{"the process's", func(l *popupLauncher) (Access, string) {
			access := popupTestAccess(l.t, "opener")
			_, opener := l.open(access)
			var other Access
			for i := range maxPagesTotal - 1 {
				if i%maxPagesPerThread == 0 {
					other = popupTestAccess(l.t, fmt.Sprintf("thread-%d", i))
				}
				l.open(other)
			}
			return access, opener
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newPopupLauncher(t)
			access, opener := tc.fill(l)
			before := len(l.tabs(access.ThreadID))
			popupID, _ := l.popup(opener, "https://example.test/popup")
			l.awaitTab(access.ThreadID, popupID, false)
			if n := len(l.tabs(access.ThreadID)); n != before {
				t.Errorf("%d tabs after a refused popup, want %d", n, before)
			}
			l.settledEngine()
		})
	}
}

// A popup whose opener closes while it is attached is closed, not adopted.
func TestHostedPopupOfAnOpenerThatClosesMidAdoptionIsClosed(t *testing.T) {
	l := newPopupLauncher(t)
	access := popupTestAccess(t, "thread")
	_, opener := l.open(access)
	release := l.gate("PT-1")
	defer release()
	popupID, _ := l.popup(opener, "https://example.test/popup")
	l.awaitAttach("PT-1")
	closed := make(chan error, 1)
	go func() { closed <- l.manager.CloseThread(context.Background(), access.ThreadID) }()
	// The opener leaves the tabs before its teardown, which waits on the
	// fake's one connection, held by the attach.
	l.awaitTabGone(access.ThreadID, opener)
	release()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	l.awaitTab(access.ThreadID, popupID, false)
	l.settledEngine()
}

// A popup of a page the engine no longer knows, or one reported after the
// engine stopped, is closed at once.
func TestHostedPopupOfAnUnknownOpenerIsClosed(t *testing.T) {
	l := newPopupLauncher(t)
	access := popupTestAccess(t, "thread")
	_, opener := l.open(access)
	l.reportPopup("popup-t-1", webview2host.PopupReport{Opener: "0123456789abcdef0123456789abcdef", Target: "PT-1"})
	l.awaitTab(access.ThreadID, "popup-t-1", false)
	l.engine.Stop()
	l.reportPopup("popup-t-2", webview2host.PopupReport{Opener: opener, Target: "PT-2"})
	if !l.closed("popup-t-2") {
		t.Error("a popup reported after Stop was not closed")
	}
	l.settledEngine()
}

// A malformed popup report closes the page it names; one naming no usable
// page is dropped, and a repeat changes nothing.
func TestHostedPopupReportValidation(t *testing.T) {
	var opened []enginePopup
	var mu sync.Mutex
	engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{PopupOpened: func(p enginePopup) {
		mu.Lock()
		opened = append(opened, p)
		mu.Unlock()
	}})
	if err := engine.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Stop)
	sink.drain()
	bindTestPage(t, engine, "opener", "T-opener")
	engine.mu.Lock()
	engine.pageProfile["opener"] = "prof"
	engine.mu.Unlock()

	engine.Report("popup-a", webview2host.ReportPopup, `{"opener":"opener"}`)
	if d := sink.next(t); d.Op != webview2host.OpClose || d.PageID != "popup-a" {
		t.Fatalf("a malformed report was answered with %+v, want close", d)
	}
	engine.Report("not a page id", webview2host.ReportPopup, `{"opener":"opener","target":"T"}`)
	engine.Report("popup-b", webview2host.ReportPopup, `{"opener":"opener","target":"T-b"}`)
	engine.Report("popup-b", webview2host.ReportPopup, `{"opener":"opener","target":"T-c"}`)
	if got := sink.drain(); len(got) != 0 {
		t.Fatalf("directives %+v", got)
	}
	deadline := time.Now().Add(hostedTestDeadline)
	for {
		mu.Lock()
		n := len(opened)
		mu.Unlock()
		if n == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(opened) != 1 || opened[0] != (enginePopup{Profile: "prof", Opener: "opener", Handle: "popup-b"}) {
		t.Fatalf("PopupOpened saw %+v", opened)
	}
	if got, _ := engine.pageForTarget("T-b"); got != "popup-b" {
		t.Errorf("the repeat re-paired the popup's target")
	}

	// No more pending popups than the launcher keeps pages.
	engine.mu.Lock()
	for i := len(engine.popups); i < webview2host.MaxHostPages; i++ {
		engine.popups[fmt.Sprintf("filler-%d", i)] = &hostedPopup{}
	}
	engine.mu.Unlock()
	engine.Report("popup-c", webview2host.ReportPopup, `{"opener":"opener","target":"T-d"}`)
	if d := sink.next(t); d.Op != webview2host.OpClose || d.PageID != "popup-c" {
		t.Fatalf("a popup past the bound was answered with %+v, want close", d)
	}
}

// A connection lost while a popup is attached fails its adoption, and the
// popup is closed.
func TestHostedPopupAttachOnALostConnectionIsClosed(t *testing.T) {
	l := newPopupLauncher(t)
	access := popupTestAccess(t, "thread")
	_, opener := l.open(access)
	release := l.gate("PT-1")
	defer release()
	popupID, _ := l.popup(opener, "https://example.test/popup")
	l.awaitAttach("PT-1")
	l.endpoint.hangUp()
	release()
	l.awaitTab(access.ThreadID, popupID, false)
	l.settledEngine()
}

// What a popup reports while it is adopted reaches its tab once it is
// listed: its address and title in order, its downloads, then its close.
func TestHostedPopupReportsMadeDuringAdoptionReachItsTab(t *testing.T) {
	l := newPopupLauncher(t)
	access := popupTestAccess(t, "thread")
	_, opener := l.open(access)
	info := func(targetID, title string) {
		l.engine.targetInfoChanged(target.EventTargetInfoChanged{TargetInfo: &target.Info{
			TargetID: target.ID(targetID), Type: "page", URL: "https://example.test/" + title, Title: title,
		}})
	}
	// Before the launcher's report, by target.
	info("PT-1", "early")
	release := l.gate("PT-1")
	defer release()
	popupID, _ := l.popup(opener, "https://example.test/popup")
	l.awaitAttach("PT-1")
	// During the attach, by page.
	info("PT-1", "late")
	reportDownload(t, l.engine, popupID, snapshot("dl-1", webview2host.DownloadRunning, 10))
	l.report(popupID, webview2host.ReportFindUnavailable, "")
	release()
	p := l.awaitTab(access.ThreadID, popupID, true)
	got := l.tabInfo(p)
	if got.Title != "late" || got.URL != "https://example.test/late" {
		t.Errorf("the popup's tab reads %q at %q, want the newest report", got.Title, got.URL)
	}
	// Delivered once the tab is listed, which awaitTab may see first.
	for deadline := time.Now().Add(hostedTestDeadline); ; {
		downloads := l.manager.CompanionState(access).Downloads
		if len(downloads) == 1 && downloads[0].State == downloadStateRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the thread's downloads are %+v, want the popup's", downloads)
		}
		time.Sleep(time.Millisecond)
	}
	l.engine.mu.Lock()
	noFind := l.engine.noFind[popupID]
	l.engine.mu.Unlock()
	if !noFind {
		t.Error("the popup's runtime has no find, but the engine does not know it")
	}
	l.settledEngine()

	// Its close, held until it is listed, closes the tab.
	release2 := l.gate("PT-2")
	defer release2()
	second, _ := l.popup(opener, "https://example.test/2")
	l.awaitAttach("PT-2")
	l.report(second, webview2host.ReportClosed, "")
	release2()
	// Listed first, then closed.
	l.settledEngine()
	l.awaitTabGone(access.ThreadID, second)
}

// A popup that crashed while it was adopted becomes a crashed tab.
func TestHostedPopupCrashDuringAdoptionReachesItsTab(t *testing.T) {
	l := newPopupLauncher(t)
	access := popupTestAccess(t, "thread")
	_, opener := l.open(access)
	release := l.gate("PT-1")
	defer release()
	popupID, _ := l.popup(opener, "https://example.test/popup")
	l.awaitAttach("PT-1")
	l.report(popupID, webview2host.ReportProcessFailed, "render-process-exited")
	release()
	// The adoption ends once the crash it delivered is handled.
	select {
	case <-l.adopted:
	case <-time.After(hostedTestDeadline):
		t.Fatal("the adoption never ended")
	}
	deadline := time.Now().Add(hostedTestDeadline)
	for {
		state := l.manager.CompanionState(access)
		if len(state.Pages) == 2 && state.Pages[1].Suspended && state.Pages[1].Error != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tabs %+v, want the popup's crashed", state.Pages)
		}
		time.Sleep(time.Millisecond)
	}
	l.settledEngine()
}

// The browser-wide policy answers a request with the policy of the page its
// frame belongs to, or, for a popup no page answers to yet, of its openers,
// nearest first; with no owner resolved, with no page's authority.
func TestHostedBrowserPolicyAnswersWithTheRequestsOwner(t *testing.T) {
	engine, _ := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	var asked [][]string
	policy := func(name string) func([]string, string) bool {
		return func(owners []string, _ string) bool {
			asked = append(asked, append([]string{name}, owners...))
			return name == "a"
		}
	}
	engine.policies["prof-a"] = policy("a")
	engine.policies["prof-b"] = policy("b")
	bindTestPage(t, engine, "page-a", "T-a")
	bindTestPage(t, engine, "page-b", "T-b")
	engine.pageProfile["page-a"] = "prof-a"
	engine.pageProfile["page-b"] = "prof-b"
	engine.targetCreated(target.EventTargetCreated{TargetInfo: &target.Info{TargetID: "T-popup", OpenerID: "T-a"}})
	engine.targetCreated(target.EventTargetCreated{TargetInfo: &target.Info{TargetID: "T-grand", OpenerID: "T-popup"}})

	// want is the policy asked, empty for any, then the owners it is asked
	// with.
	for _, tc := range []struct {
		frame string
		want  []string
	}{
		{"T-a", []string{"a", "T-a"}},
		{"T-b", []string{"b", "T-b"}},
		{"T-grand", []string{"a", "T-grand", "T-popup", "T-a"}},
		{"T-stranger", []string{"", "T-stranger"}},
	} {
		asked = nil
		allowed := engine.allowRequest(tc.frame, "https://example.test/")
		if len(asked) != 1 || tc.want[0] != "" && asked[0][0] != tc.want[0] || !slices.Equal(asked[0][1:], tc.want[1:]) {
			t.Errorf("%s: asked %v, want %v", tc.frame, asked, tc.want)
		} else if allowed != (asked[0][0] == "a") {
			t.Errorf("%s: allowed %v by policy %s", tc.frame, allowed, asked[0][0])
		}
	}
	// A destroyed target's link to its opener goes with it.
	engine.targetDestroyed(target.EventTargetDestroyed{TargetID: "T-popup"})
	asked = nil
	engine.allowRequest("T-grand", "https://example.test/")
	if len(asked) != 1 || !slices.Equal(asked[0][1:], []string{"T-grand", "T-popup"}) {
		t.Errorf("after the destroy: asked %v", asked)
	}
	// With no profile's policy, nothing is allowed.
	clear(engine.policies)
	if engine.allowRequest("T-a", "https://example.test/") {
		t.Error("a request with no policy was allowed")
	}
}

// The browser-wide policy is installed on the connection and answers what
// it pauses there.
func TestHostedBrowserPolicyAnswersPausedRequests(t *testing.T) {
	endpoint := startFakeCDPEndpoint(t, nil, nil)
	engine, _ := newTestHostedEngine(t, stubRelay{url: endpoint.wsURL}, engineEvents{})
	engine.attachTimeout = 5 * time.Second
	engine.policies["prof"] = func(_ []string, rawURL string) bool { return !strings.HasPrefix(rawURL, "file:") }
	if _, err := engine.ensureBrowser(); err != nil {
		t.Fatal(err)
	}
	defer engine.Interrupt()
	if got := browserCallParams[struct {
		Patterns []json.RawMessage `json:"patterns"`
	}](t, endpoint.calls(), "Fetch.enable"); len(got) != 1 || len(got[0].Patterns) == 0 {
		t.Fatalf("Fetch.enable on the browser session: %+v", got)
	}
	endpoint.send(pausedRequest("R-1", "T-popup", "https://example.test/"))
	endpoint.send(pausedRequest("R-2", "T-popup", "file:///etc/passwd"))
	deadline := time.Now().Add(hostedTestDeadline)
	for {
		continued := browserCallParams[requestAnswer](t, endpoint.calls(), "Fetch.continueRequest")
		failed := browserCallParams[requestAnswer](t, endpoint.calls(), "Fetch.failRequest")
		if len(continued) == 1 && len(failed) == 1 {
			if continued[0].RequestID != "R-1" || failed[0].RequestID != "R-2" {
				t.Fatalf("continued %+v, failed %+v", continued, failed)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("continued %+v, failed %+v", continued, failed)
		}
		time.Sleep(time.Millisecond)
	}
}
