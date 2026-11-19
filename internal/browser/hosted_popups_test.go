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

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/webview2host"

	"github.com/chromedp/cdproto/target"
)

// A popup becomes a tab of its opener's thread at the address the launcher
// reported, attached at the target the launcher paired with it.
func TestHostedPopupIsAdoptedIntoItsOpenersThread(t *testing.T) {
	l := newPopupLauncher(t)
	access := l.access("thread")
	_, opener := l.open(access)
	popupID, targetID := l.popup(opener, "https://example.test/popup")
	p := l.awaitTab(access.ThreadID, popupID, true)
	if got := l.tabInfo(p); got.URL != "https://example.test/popup" {
		t.Errorf("the popup's tab is at %q", got.URL)
	}
	if got, ok := l.engine.pageForTarget(targetID); !ok || got != ids.PageHandle(popupID) {
		t.Errorf("target %s answers page %q, want %s", targetID, got, popupID)
	}
	l.settledEngine()

	// Its close is the launcher's, as any page's.
	l.report(popupID, webview2host.ReportClosed, "")
	l.awaitTabGone(access.ThreadID, popupID)

	// The browser may report a popup's target before the launcher reports
	// the popup; the tab takes what it said.
	l.engine.targetInfoChanged(l.engine.currentTargets(), target.EventTargetInfoChanged{TargetInfo: &target.Info{
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
	access := l.access("thread")
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
			access := l.access("thread")
			var opener string
			for range maxPagesPerThread {
				_, opener = l.open(access)
			}
			return access, opener
		}},
		// The opener's thread has room in these two; other threads fill the
		// workspace or the process.
		{"the workspace's", func(l *popupLauncher) (Access, string) {
			access := l.access("opener")
			_, opener := l.open(access)
			for i := range maxPagesPerWorkspace - 1 {
				l.open(l.manager.Bind(Caller{ThreadID: fmt.Sprintf("thread-%d", i/maxPagesPerThread), Workspace: access.Workspace}))
			}
			return access, opener
		}},
		{"the process's", func(l *popupLauncher) (Access, string) {
			access := l.access("opener")
			_, opener := l.open(access)
			var other Access
			for i := range maxPagesTotal - 1 {
				if i%maxPagesPerThread == 0 {
					other = l.access(fmt.Sprintf("thread-%d", i))
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
	access := l.access("thread")
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
	access := l.access("thread")
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

	engine.Report("popup-a", webview2host.ReportPopup, `{"target":"T-a"}`)
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

	// A report naming a known page id is never the report that closes it,
	// whatever it carries; one naming a target a page or a pending popup
	// already has is refused and the target keeps its page.
	engine.Report("popup-b", webview2host.ReportPopup, `{"target":"T-x"}`)
	engine.Report("opener", webview2host.ReportPopup, `not json`)
	engine.Report("opener", webview2host.ReportPopup, `{"opener":"opener","target":"T-y"}`)
	if got := sink.drain(); len(got) != 0 {
		t.Fatalf("a report for a known page was answered with %+v", got)
	}
	for _, claimed := range []string{"T-b", "T-opener"} {
		engine.Report(ids.PageHandle("popup-"+claimed), webview2host.ReportPopup, `{"opener":"opener","target":"`+claimed+`"}`)
		if d := sink.next(t); d.Op != webview2host.OpClose || d.PageID != "popup-"+claimed {
			t.Fatalf("a popup claiming %s was answered with %+v, want close", claimed, d)
		}
	}
	for target, want := range map[string]string{"T-b": "popup-b", "T-opener": "opener"} {
		if got, _ := engine.pageForTarget(target); got != ids.PageHandle(want) {
			t.Errorf("target %s belongs to %q, want %q", target, got, want)
		}
	}
	engine.mu.Lock()
	for _, id := range []string{"popup-T-b", "popup-T-opener"} {
		if _, ok := engine.pageProfile[ids.PageHandle(id)]; ok || engine.popups[ids.PageHandle(id)] != nil {
			t.Errorf("the refused popup %s was kept", id)
		}
	}
	engine.mu.Unlock()

	// No more pending popups than the launcher keeps pages.
	engine.mu.Lock()
	for i := len(engine.popups); i < webview2host.MaxHostPages; i++ {
		engine.popups[ids.PageHandle(fmt.Sprintf("filler-%d", i))] = &hostedPopup{}
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
	access := l.access("thread")
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

// A popup's target lives on its opener's connection: once that connection
// ends, the popup is never attached, even when another connection is up
// by then; and a popup whose opener has no connection is refused at its
// report.
func TestHostedPopupAttachesOnlyOnItsOpenersConnection(t *testing.T) {
	l := newPopupLauncher(t)
	l.engine.events.PopupOpened = func(enginePopup) {}
	access := l.access("thread")
	_, opener := l.open(access)
	l.engine.mu.Lock()
	profile := &hostedProfile{engine: l.engine, id: l.engine.pageProfile[ids.PageHandle(opener)]}
	first := l.engine.browserCtx
	l.engine.mu.Unlock()
	popupID, _ := l.popup(opener, "https://example.test/popup")

	l.endpoint.hangUp()
	<-first.Done()
	if _, err := l.engine.ensureBrowser(); err != nil {
		t.Fatal(err)
	}
	if driver, err := l.engine.attachPopup(profile, ids.PageHandle(popupID), pageHooks{}); err == nil {
		driver.Close()
		t.Fatal("a popup of a lost connection was attached on its replacement")
	}

	// Openers the engine knows, one whose connection ended and one bound
	// to none (its connection claimed it).
	ended, end := context.WithCancel(context.Background())
	end()
	bindTestPage(t, l.engine, "ended", "T-ended")
	l.engine.mu.Lock()
	l.engine.pageConn["ended"] = ended
	for _, id := range []string{"ended", "unbound"} {
		l.engine.pageProfile[ids.PageHandle(id)] = profile.id
	}
	l.engine.mu.Unlock()
	for _, opener := range []string{"ended", "unbound"} {
		orphan, _ := l.popup(opener, "https://example.test/orphan")
		l.engine.mu.Lock()
		kept := l.engine.popups[ids.PageHandle(orphan)] != nil
		l.engine.mu.Unlock()
		if kept || !l.closed(orphan) {
			t.Fatalf("a popup of %s: kept %v, closed %v", opener, kept, l.closed(orphan))
		}
	}
}

// What a popup reports while it is adopted reaches its tab once it is
// listed: its address and title in order, its downloads, then its close.
func TestHostedPopupReportsMadeDuringAdoptionReachItsTab(t *testing.T) {
	l := newPopupLauncher(t)
	access := l.access("thread")
	_, opener := l.open(access)
	info := func(targetID, title string) {
		l.engine.targetInfoChanged(l.engine.currentTargets(), target.EventTargetInfoChanged{TargetInfo: &target.Info{
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
		downloads := l.manager.CompanionState(access.Caller()).Downloads
		if len(downloads) == 1 && downloads[0].State == downloadStateRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the thread's downloads are %+v, want the popup's", downloads)
		}
		time.Sleep(time.Millisecond)
	}
	l.engine.mu.Lock()
	noFind := l.engine.noFind[ids.PageHandle(popupID)]
	l.engine.mu.Unlock()
	if !noFind {
		t.Error("the popup's runtime has no find, but the engine does not know it")
	}
	l.settledEngine()

	// Its close during the attach ends the attach, and no tab is left.
	release2 := l.gate("PT-2")
	defer release2()
	second, _ := l.popup(opener, "https://example.test/2")
	l.awaitAttach("PT-2")
	l.report(second, webview2host.ReportClosed, "")
	release2()
	l.settledEngine()
	l.awaitTabGone(access.ThreadID, second)
}

// A popup that crashed while it was adopted becomes a crashed tab.
func TestHostedPopupCrashDuringAdoptionReachesItsTab(t *testing.T) {
	l := newPopupLauncher(t)
	access := l.access("thread")
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
		state := l.manager.CompanionState(access.Caller())
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

// A popup that closed after its attach and before its listing leaves no
// tab, crashed or live, whether it crashed too and in either order; one
// that only crashed is a crashed tab. The launcher's reports reach the
// Manager through the engine while the adoption reads the popup's history.
func TestHostedPopupCrashAndCloseDuringAdoptionLeaveNoTab(t *testing.T) {
	for _, tc := range []struct {
		name        string
		reports     []webview2host.ReportKind
		wantCrashed bool
	}{
		{"crashed, then closed", []webview2host.ReportKind{webview2host.ReportProcessFailed, webview2host.ReportClosed}, false},
		{"closed, then crashed", []webview2host.ReportKind{webview2host.ReportClosed, webview2host.ReportProcessFailed}, false},
		{"closed", []webview2host.ReportKind{webview2host.ReportClosed}, false},
		{"crashed", []webview2host.ReportKind{webview2host.ReportProcessFailed}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newPopupLauncher(t)
			access := l.access("thread")
			_, opener := l.open(access)
			release := l.gateHistory("PT-1")
			defer release()
			popupID, _ := l.popup(opener, "https://example.test/popup")
			l.awaitAttach("history PT-1")
			for _, kind := range tc.reports {
				detail := ""
				if kind == webview2host.ReportProcessFailed {
					detail = "render-process-exited"
				}
				l.report(popupID, kind, detail)
			}
			release()
			deadline := time.Now().Add(hostedTestDeadline)
			for {
				state := l.manager.CompanionState(access.Caller())
				crashed := len(state.Pages) == 2 && state.Pages[1].Suspended && state.Pages[1].Error != ""
				gone := len(state.Pages) == 1 && l.closed(popupID)
				if (tc.wantCrashed && crashed) || (!tc.wantCrashed && gone) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("tabs %+v (the popup's page closed: %v), want the popup's crashed: %v", state.Pages, l.closed(popupID), tc.wantCrashed)
				}
				time.Sleep(time.Millisecond)
			}
			l.settledEngine()
		})
	}
}

// policyRecorder stands in for the profiles' policies: each answers
// resolved for owners that name one of its frames, allowing what profile
// "a" asks, and otherwise answers unresolved, allowing a web address alone,
// as a jar does with no workspace's authority (jarAllows). It records the
// profile asked and the owners it was asked with.
type policyRecorder struct {
	mu    sync.Mutex
	asked [][]string
}

func (r *policyRecorder) policy(name string, frames ...string) func([]string, string) (bool, bool) {
	return func(owners []string, rawURL string) (bool, bool) {
		r.mu.Lock()
		r.asked = append(r.asked, append([]string{name}, owners...))
		r.mu.Unlock()
		for _, owner := range owners {
			if slices.Contains(frames, owner) {
				return name == "a", true
			}
		}
		return strings.HasPrefix(rawURL, "https:"), false
	}
}

func (r *policyRecorder) take() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.asked
	r.asked = nil
	return out
}

// newPolicyEngine answers a hosted engine with page-a (target T-a) in
// profile prof-a, whose policy allows, and page-b (T-b) in prof-b, whose
// policy refuses.
func newPolicyEngine(t *testing.T) (*hostedEngine, *policyRecorder) {
	t.Helper()
	engine, _ := newTestHostedEngine(t, stubRelay{}, engineEvents{PopupOpened: func(enginePopup) {}})
	if err := engine.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Stop)
	r := &policyRecorder{}
	engine.mu.Lock()
	engine.policies["prof-a"] = r.policy("a", "T-a", "F-sub")
	engine.policies["prof-b"] = r.policy("b", "T-b")
	engine.pageProfile["page-a"] = "prof-a"
	engine.pageProfile["page-b"] = "prof-b"
	engine.mu.Unlock()
	bindTestPage(t, engine, "page-a", "T-a")
	bindTestPage(t, engine, "page-b", "T-b")
	return engine, r
}

// The browser-wide policy answers a request with the policy of the page its
// frame belongs to, or, for a popup no page answers to yet, of its openers,
// nearest first. With no owner resolved, it answers unresolved with what
// the profiles' policies allow a page with no workspace's authority.
func TestHostedBrowserPolicyAnswersWithTheRequestsOwner(t *testing.T) {
	engine, r := newPolicyEngine(t)
	engine.currentTargets().created(&target.Info{TargetID: "T-popup", Type: "page", OpenerID: "T-a"}, nil)
	engine.currentTargets().created(&target.Info{TargetID: "T-grand", Type: "page", OpenerID: "T-popup"}, nil)

	// want is the policy asked, then the owners it is asked with.
	for _, tc := range []struct {
		frame string
		want  []string
	}{
		{"T-a", []string{"a", "T-a"}},
		{"T-b", []string{"b", "T-b"}},
		{"T-grand", []string{"a", "T-grand", "T-popup", "T-a"}},
	} {
		allowed, resolved := engine.allowRequest(engine.currentTargets(), tc.frame, "https://example.test/")
		asked := r.take()
		switch {
		case !resolved || len(asked) != 1 || !slices.Equal(asked[0], tc.want):
			t.Errorf("%s: resolved %v, asked %v, want %v", tc.frame, resolved, asked, tc.want)
		case allowed != (tc.want[0] == "a"):
			t.Errorf("%s: allowed %v by policy %s", tc.frame, allowed, tc.want[0])
		}
	}
	wantNoAuthority := func(step, frame string) {
		t.Helper()
		for url, want := range map[string]bool{"https://example.test/": true, "file:///w/one.html": false} {
			allowed, resolved := engine.allowRequest(engine.currentTargets(), frame, url)
			if allowed != want || resolved {
				t.Errorf("%s: %s was answered allowed %v, resolved %v; want %v unresolved", step, url, allowed, resolved, want)
			}
			if asked := r.take(); len(asked) == 0 || asked[0][1] != frame {
				t.Errorf("%s: asked %v, want a policy asked for %s", step, asked, frame)
			}
		}
	}
	wantNoAuthority("a frame no page answers for", "T-stranger")
	// A destroyed target's link to its opener goes with it.
	engine.targetDestroyed(engine.currentTargets(), target.EventTargetDestroyed{TargetID: "T-popup"})
	wantNoAuthority("after the destroy, the popup's popup", "T-grand")
	// With no profile's policy, nothing resolves.
	engine.mu.Lock()
	clear(engine.policies)
	engine.mu.Unlock()
	if allowed, resolved := engine.allowRequest(engine.currentTargets(), "T-a", "https://example.test/"); allowed || resolved {
		t.Error("a request with no policy was answered")
	}
}

// A popup whose target names no opener (a click that opened a new tab)
// has the opener the launcher's report names, and so on up its chain.
func TestHostedBrowserPolicyFollowsTheReportedOpener(t *testing.T) {
	engine, r := newPolicyEngine(t)
	engine.currentTargets().created(&target.Info{TargetID: "T-pop", Type: "page"}, nil)
	engine.currentTargets().created(&target.Info{TargetID: "T-pop2", Type: "page"}, nil)
	engine.Report("popup-1", webview2host.ReportPopup, `{"opener":"page-a","target":"T-pop"}`)
	engine.Report("popup-2", webview2host.ReportPopup, `{"opener":"popup-1","target":"T-pop2"}`)
	for frame, want := range map[string][]string{
		"T-pop":  {"a", "T-pop", "T-a"},
		"T-pop2": {"a", "T-pop2", "T-pop", "T-a"},
	} {
		allowed, resolved := engine.allowRequest(engine.currentTargets(), frame, "https://example.test/")
		if asked := r.take(); !allowed || !resolved || len(asked) != 1 || !slices.Equal(asked[0], want) {
			t.Errorf("%s: allowed %v, resolved %v, asked %v, want %v", frame, allowed, resolved, asked, want)
		}
	}
}

// A subframe resolves to the page that reported it.
func TestHostedBrowserPolicyResolvesASubframeToItsPage(t *testing.T) {
	engine, r := newPolicyEngine(t)
	page := &cdpPage{handle: "T-a", frames: newFrameTracker()}
	page.frames.attached("F-sub")
	engine.mu.Lock()
	engine.drivers["page-a"] = page
	engine.mu.Unlock()
	allowed, resolved := engine.allowRequest(engine.currentTargets(), "F-sub", "https://example.test/")
	if asked := r.take(); !allowed || !resolved || len(asked) != 1 || !slices.Equal(asked[0], []string{"a", "F-sub"}) {
		t.Fatalf("a subframe of page-a: allowed %v, resolved %v, asked %v", allowed, resolved, asked)
	}
}

// A request that needs an owner's authority, paused before anything names
// its owner, waits for it, whichever comes second of the pause and its
// target's creation, or of the pause and the launcher's report of its
// popup, and is refused when nothing names one within the bound. One a page
// with no workspace's authority may make is allowed at once.
func TestHostedPausedRequestWaitsForItsOwner(t *testing.T) {
	learn := map[string]func(e *hostedEngine){
		"target created": func(e *hostedEngine) {
			e.currentTargets().created(&target.Info{TargetID: "T-pop", Type: "page", OpenerID: "T-a"}, nil)
		},
		"popup reported": func(e *hostedEngine) {
			e.Report("popup-1", webview2host.ReportPopup, `{"opener":"page-a","target":"T-pop"}`)
		},
	}
	for name, learnOwner := range learn {
		for _, pausedFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, paused first %v", name, pausedFirst), func(t *testing.T) {
				engine, r := newPolicyEngine(t)
				engine.ownerWait = hostedTestDeadline
				if !pausedFirst {
					learnOwner(engine)
				}
				answered := make(chan bool, 1)
				go func() {
					answered <- awaitRequestOwner(t.Context(), engine.ownerWait, func() (bool, bool) {
						return engine.allowRequest(engine.currentTargets(), "T-pop", "file:///w/one.html")
					}, t.Logf)
				}()
				if pausedFirst {
					time.Sleep(20 * time.Millisecond)
					select {
					case got := <-answered:
						t.Fatalf("a request with no owner was answered %v at once", got)
					default:
					}
					learnOwner(engine)
				}
				if !<-answered {
					t.Fatalf("the request was refused: asked %v", r.take())
				}
				if asked := r.take(); len(asked) == 0 || !slices.Equal(asked[len(asked)-1], []string{"a", "T-pop", "T-a"}) {
					t.Fatalf("asked %v", asked)
				}
			})
		}
	}
	t.Run("nothing names one", func(t *testing.T) {
		engine, _ := newPolicyEngine(t)
		start := time.Now()
		if awaitRequestOwner(t.Context(), 50*time.Millisecond, func() (bool, bool) {
			return engine.allowRequest(engine.currentTargets(), "T-stranger", "file:///w/one.html")
		}, t.Logf) {
			t.Fatal("a request no page answers for was allowed")
		}
		if waited := time.Since(start); waited < 50*time.Millisecond {
			t.Fatalf("refused after %v, before the bound", waited)
		}
	})
	// What a page with no workspace's authority may open needs no owner.
	t.Run("none is needed", func(t *testing.T) {
		engine, _ := newPolicyEngine(t)
		start := time.Now()
		if !awaitRequestOwner(t.Context(), hostedTestDeadline, func() (bool, bool) {
			return engine.allowRequest(engine.currentTargets(), "T-stranger", "https://example.test/")
		}, t.Logf) {
			t.Fatal("a web request no page answers for was refused")
		}
		if waited := time.Since(start); waited >= hostedTestDeadline/2 {
			t.Fatalf("allowed after %v, waiting for an owner it does not need", waited)
		}
	})
}

// A page and a popup attached, the hosted engine still pauses a request
// once: the browser-wide policy is the only one.
func TestHostedEnginePausesARequestOnce(t *testing.T) {
	l := newPopupLauncher(t)
	access := l.access("thread")
	_, opener := l.open(access)
	popupID, _ := l.popup(opener, "https://example.test/popup")
	l.awaitTab(access.ThreadID, popupID, true)
	l.settledEngine()
	assertOnePolicyPause(t, l.endpoint.calls, l.endpoint.send)
}

// The browser-wide policy is installed on the connection and answers what
// it pauses there.
func TestHostedBrowserPolicyAnswersPausedRequests(t *testing.T) {
	endpoint := startFakeCDPEndpoint(t, nil, nil)
	engine, _ := newTestHostedEngine(t, stubRelay{url: endpoint.wsURL}, engineEvents{})
	engine.attachTimeout = 5 * time.Second
	engine.policies["prof"] = func(_ []string, rawURL string) (bool, bool) { return !strings.HasPrefix(rawURL, "file:"), true }
	if _, err := engine.ensureBrowser(); err != nil {
		t.Fatal(err)
	}
	defer engine.Interrupt()
	bindTestPage(t, engine, "page", "T-page")
	engine.mu.Lock()
	engine.pageProfile["page"] = "prof"
	engine.mu.Unlock()
	if got := browserCallParams[struct {
		Patterns []json.RawMessage `json:"patterns"`
	}](t, endpoint.calls(), "Fetch.enable"); len(got) != 1 || len(got[0].Patterns) == 0 {
		t.Fatalf("Fetch.enable on the browser session: %+v", got)
	}
	endpoint.send(pausedRequest("R-1", "T-page", "https://example.test/"))
	endpoint.send(pausedRequest("R-2", "T-page", "file:///etc/passwd"))
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
