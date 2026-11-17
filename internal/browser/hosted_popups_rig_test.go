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
)

// Popups on the hosted engine against a fake launcher: a directive sink
// that answers creates and closes as the launcher does, and a fake CDP
// endpoint a page or popup attaches through. The Manager is the real one,
// so adoption, its caps and its refusals are the ones every engine gets.

// popupLauncher is the launcher's side: it answers each create with the
// page's target and each close with closed, as reports through the
// Manager, and holds the attach of any target in gated until released.
type popupLauncher struct {
	t        *testing.T
	manager  *Manager
	engine   *hostedEngine
	endpoint *fakeCDPEndpoint

	// adopted hears each adoption the Manager finishes (pageAdopted).
	adopted chan struct{}

	mu      sync.Mutex
	closes  []string
	gated   map[string]chan struct{}
	reached chan string
	seq     int
}

func newPopupLauncher(t *testing.T) *popupLauncher {
	t.Helper()
	l := &popupLauncher{t: t, gated: map[string]chan struct{}{}, reached: make(chan string, 16), adopted: make(chan struct{}, 16)}
	l.endpoint = startFakeCDPEndpoint(t, l.respond, nil)
	l.manager = NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{PaneHost: &PaneHostOptions{
		Directive: l.send,
		Relay:     stubRelay{url: l.endpoint.wsURL},
	}})
	t.Cleanup(func() { _ = l.manager.Close() })
	l.manager.pageAdopted = func() {
		select {
		case l.adopted <- struct{}{}:
		default:
		}
	}
	engine, ok := l.manager.engine.(*hostedEngine)
	if !ok {
		t.Fatalf("engine is %T, want the hosted engine", l.manager.engine)
	}
	l.engine = engine
	engine.logf = t.Logf
	engine.attachTimeout = 5 * time.Second
	engine.windowsPath = testWindowsPath
	engine.backendPath = testBackendPath
	engine.downloadQuiet = time.Hour
	engine.readPageRatio = func(context.Context, *cdpPage) (float64, error) { return 1, nil }
	return l
}

// respond answers a page's attach, holding it while its target is gated.
func (l *popupLauncher) respond(call fakeCDPCall) any {
	switch call.Method {
	case "Target.attachToTarget":
		var params struct {
			TargetID string `json:"targetId"`
		}
		_ = json.Unmarshal(call.Params, &params)
		l.mu.Lock()
		gate := l.gated[params.TargetID]
		l.mu.Unlock()
		if gate != nil {
			l.reached <- params.TargetID
			<-gate
		}
		return map[string]any{"sessionId": "S-" + params.TargetID}
	case "Runtime.evaluate":
		return map[string]any{"result": map[string]any{"type": "object", "className": "Window"}}
	case "Page.getFrameTree":
		return map[string]any{"frameTree": map[string]any{"frame": map[string]any{
			"id": strings.TrimPrefix(call.SessionID, "S-"), "loaderId": "L-1", "url": "about:blank",
			"securityOrigin": "://", "mimeType": "text/html",
		}}}
	case "DOM.getDocument":
		return map[string]any{"root": map[string]any{
			"nodeId": 1, "backendNodeId": 1, "nodeType": 9,
			"nodeName": "#document", "localName": "", "nodeValue": "",
		}}
	}
	return nil
}

func (l *popupLauncher) send(directive webview2host.Directive) {
	switch directive.Op {
	case webview2host.OpCreate:
		go l.report(directive.PageID, webview2host.ReportCreated, "T-"+directive.PageID)
	case webview2host.OpClose:
		l.mu.Lock()
		l.closes = append(l.closes, directive.PageID)
		l.mu.Unlock()
		go l.report(directive.PageID, webview2host.ReportClosed, "")
	}
}

func (l *popupLauncher) report(pageID string, kind webview2host.ReportKind, detail string) {
	if err := l.manager.ReportPaneHost(pageID, kind, detail); err != nil {
		l.t.Errorf("report %s of %s: %v", kind, pageID, err)
	}
}

// gate holds the attach of target until the answered release.
func (l *popupLauncher) gate(target string) (release func()) {
	ch := make(chan struct{})
	l.mu.Lock()
	l.gated[target] = ch
	l.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

// awaitAttach waits for the attach of a gated target.
func (l *popupLauncher) awaitAttach(target string) {
	l.t.Helper()
	select {
	case got := <-l.reached:
		if got != target {
			l.t.Fatalf("attach of %s, want %s", got, target)
		}
	case <-time.After(hostedTestDeadline):
		l.t.Fatalf("%s was never attached", target)
	}
}

// open opens a blank tab in access's thread and answers its tab id and
// engine page id.
func (l *popupLauncher) open(access Access) (string, string) {
	l.t.Helper()
	info, err := l.manager.NewCompanionPage(l.t.Context(), access, "")
	if err != nil {
		l.t.Fatalf("open a tab: %v", err)
	}
	return info.ID, paneHandle(l.manager, access, info.ID)
}

// popup has the launcher report a popup of opener and answers its page id
// and target.
func (l *popupLauncher) popup(opener, url string) (string, string) {
	l.t.Helper()
	l.mu.Lock()
	l.seq++
	n := l.seq
	l.mu.Unlock()
	pageID := fmt.Sprintf("popup-t-%d", n)
	targetID := fmt.Sprintf("PT-%d", n)
	l.reportPopup(pageID, webview2host.PopupReport{Opener: opener, Target: targetID, URL: url})
	return pageID, targetID
}

func (l *popupLauncher) reportPopup(pageID string, report webview2host.PopupReport) {
	l.t.Helper()
	detail, err := report.Encode()
	if err != nil {
		l.t.Fatal(err)
	}
	l.report(pageID, webview2host.ReportPopup, detail)
}

// tabs answers the thread's tabs by engine page id.
func (l *popupLauncher) tabs(threadID string) map[string]*managedPage {
	out := map[string]*managedPage{}
	for _, p := range l.manager.ownedPages(threadID) {
		out[p.driver.Handle()] = p
	}
	return out
}

// awaitTab waits until the thread has a tab for handle, or, with want
// false, has none and the launcher was asked to close it.
func (l *popupLauncher) awaitTab(threadID, handle string, want bool) *managedPage {
	l.t.Helper()
	deadline := time.Now().Add(hostedTestDeadline)
	for {
		p, has := l.tabs(threadID)[handle]
		switch {
		case want && has:
			return p
		case !want && !has && l.closed(handle):
			return nil
		case time.Now().After(deadline):
			l.t.Fatalf("thread %s tab for %s: has %v, closed %v, want a tab %v", threadID, handle, has, l.closed(handle), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func (l *popupLauncher) closed(pageID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Contains(l.closes, pageID)
}

// settledEngine checks the engine keeps nothing of a popup past its
// adoption or discard.
func (l *popupLauncher) settledEngine() {
	l.t.Helper()
	deadline := time.Now().Add(hostedTestDeadline)
	for {
		l.engine.mu.Lock()
		pending, byTarget := len(l.engine.popups), len(l.engine.popupByTarget)
		l.engine.mu.Unlock()
		if pending == 0 && byTarget == 0 {
			return
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("the engine keeps %d pending popups and %d targets", pending, byTarget)
		}
		time.Sleep(time.Millisecond)
	}
}

func popupTestAccess(t *testing.T, thread string) Access {
	return Access{ThreadID: thread, Workspace: t.TempDir()}
}

// tabInfo answers a tab's info as the Manager holds it.
func (l *popupLauncher) tabInfo(p *managedPage) PageInfo {
	return p.cachedInfo()
}

func (l *popupLauncher) awaitTabGone(threadID, handle string) {
	l.t.Helper()
	deadline := time.Now().Add(hostedTestDeadline)
	for {
		if _, has := l.tabs(threadID)[handle]; !has {
			return
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("the tab for %s stayed", handle)
		}
		time.Sleep(time.Millisecond)
	}
}
