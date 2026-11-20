//go:build !windows

package browser

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A page knows the frames it had before its attach, which no frame event
// reports: a popup's iframe made before its adoption is the popup's.
func TestCDPPageOwnsTheFramesItHadBeforeItsAttach(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", true)
	page, err := profile.NewPage(context.Background(), testPageHooks())
	if err != nil {
		t.Fatal(err)
	}
	for _, frame := range []string{"F-child", "F-grandchild"} {
		if !page.OwnsFrame(frameID(frame)) {
			t.Errorf("the page does not own its frame %s", frame)
		}
	}
	if page.OwnsFrame("F-stranger") {
		t.Error("the page owns a frame it never had")
	}
}

// Over the connection, a frame whose attach the browser sent before the
// frame tree's reply, which names it too, is the page's until its detach,
// and a detach of the frame tree's own frame ends it as well: the page
// reads its frame events and the tree in the order they came.
func TestCDPPageReadsItsFramesInWireOrder(t *testing.T) {
	browser := writeAnnouncingFakeChromium(t, "chromium", func(call fakeCDPCall) []map[string]any {
		if call.Method != "Page.getFrameTree" {
			return nil
		}
		return []map[string]any{{"method": "Page.frameAttached", "sessionId": call.SessionID, "params": map[string]any{"frameId": "F-child", "parentFrameId": "F-1"}}}
	})
	engine := newTestHeadlessEngine(t, browser.path)
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", true)
	page, err := profile.NewPage(context.Background(), testPageHooks())
	if err != nil {
		t.Fatal(err)
	}
	defer page.Close()
	if !page.OwnsFrame("F-child") || !page.OwnsFrame("F-grandchild") {
		t.Fatal("the page does not own the frames of its tree")
	}
	for _, frame := range []string{"F-child", "F-grandchild"} {
		browser.send(map[string]any{"method": "Page.frameDetached", "sessionId": "S-1", "params": map[string]any{"frameId": frame, "reason": "remove"}})
	}
	eventually(t, "the detached frames to leave the page", func() bool {
		return !page.OwnsFrame("F-child") && !page.OwnsFrame("F-grandchild")
	})
}

// A page owns its main frame, whose id is its target id, before it has
// reported a frame at all: a download can begin in a navigation that never
// commits, and the Manager finds the download's page by its frame.
func TestCDPPageOwnsItsMainFrameBeforeReportingOne(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)
	page, err := profile.NewPage(context.Background(), testPageHooks())
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	defer page.Close()
	if !page.OwnsFrame(frameID(page.Handle())) {
		t.Fatalf("page %s does not own its main frame", page.Handle())
	}
	if page.OwnsFrame("F-elsewhere") {
		t.Fatal("a page owns a frame it never reported")
	}
}

// Every CDP page (the hosted WebView2 engine starts the same one) enables
// Network with explicit caps, so Chromium retains no response bodies or post
// data for it and copies at most one byte of post data into an event.
func TestCDPPageEnablesNetworkWithNoRetainedBodies(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)
	page, err := profile.NewPage(context.Background(), testPageHooks())
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	defer page.Close()
	type caps struct {
		MaxTotalBufferSize    *int64 `json:"maxTotalBufferSize"`
		MaxResourceBufferSize *int64 `json:"maxResourceBufferSize"`
		MaxPostDataSize       *int64 `json:"maxPostDataSize"`
	}
	// chromedp's attach enables Network first with Chromium's defaults, and a
	// later Network.enable replaces the limits, so the page's last one counts.
	var last *caps
	for _, call := range browser.calls() {
		if call.Method != "Network.enable" || call.SessionID == "" {
			continue
		}
		var got caps
		if len(call.Params) > 0 {
			if err := json.Unmarshal(call.Params, &got); err != nil {
				t.Fatalf("decode Network.enable %s: %v", call.Params, err)
			}
		}
		last = &got
	}
	if last == nil {
		t.Fatal("the page never enabled Network")
	}
	if last.MaxTotalBufferSize == nil || *last.MaxTotalBufferSize != 0 ||
		last.MaxResourceBufferSize == nil || *last.MaxResourceBufferSize != 0 ||
		last.MaxPostDataSize == nil || *last.MaxPostDataSize != 1 {
		t.Fatalf("the page's last Network.enable caps total %v, resource %v, post data %v; want 0, 0, 1",
			ptrValue(last.MaxTotalBufferSize), ptrValue(last.MaxResourceBufferSize), ptrValue(last.MaxPostDataSize))
	}
}

// Every CDP page leaves the CSS domain that chromedp's attach enabled turned
// off: no driver reads its events, and on the hosted engine each would cross
// the launcher's relay (one per style sheet a document adds).
func TestCDPPageLeavesTheUnreadCSSDomainDisabled(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)
	page, err := profile.NewPage(context.Background(), testPageHooks())
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	defer page.Close()
	last := ""
	for _, call := range browser.calls() {
		if call.SessionID != "" && (call.Method == "CSS.enable" || call.Method == "CSS.disable") {
			last = call.Method
		}
	}
	if last != "CSS.disable" {
		t.Fatalf("the page's last CSS domain call is %q, want CSS.disable", last)
	}
}

// A CDP page reports a new document when its main frame navigates
// (Page.frameNavigated with no parent); a child frame's navigation and a
// same-document one (Page.navigatedWithinDocument) are not one. The hosted
// WebView2 engine drives the same page.
func TestCDPPageReportsOnlyMainFrameDocumentCommits(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)
	var commits atomic.Int64
	hooks := testPageHooks()
	hooks.DocumentCommitted = func() func() {
		commits.Add(1)
		return nil
	}
	page, err := profile.NewPage(context.Background(), hooks)
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	defer page.Close()
	frame := func(id, parent string) map[string]any {
		f := map[string]any{"id": id, "loaderId": "L-2", "url": "https://example.test/", "securityOrigin": "https://example.test", "mimeType": "text/html"}
		if parent != "" {
			f["parentId"] = parent
		}
		return f
	}
	send := func(method string, params map[string]any) {
		browser.send(map[string]any{"method": method, "sessionId": "S-1", "params": params})
	}
	send("Page.navigatedWithinDocument", map[string]any{"frameId": "F-1", "url": "https://example.test/#part", "navigationType": "fragment"})
	send("Page.frameNavigated", map[string]any{"frame": frame("F-child", "F-1"), "type": "Navigation"})
	send("Page.frameNavigated", map[string]any{"frame": frame("F-1", ""), "type": "Navigation"})
	eventually(t, "the main frame's commit", func() bool { return commits.Load() >= 1 })
	time.Sleep(50 * time.Millisecond)
	if got := commits.Load(); got != 1 {
		t.Fatalf("document commits = %d, want 1 (the main frame's only)", got)
	}
}

// Chromium reports no event when a document sets its title, so a CDP page
// installs the title watcher in an isolated world, for documents to come and
// the current one, behind a binding only that world sees, and hands each
// report of that binding to DocumentTitle. Another binding's calls are not
// title reports. The hosted WebView2 engine drives the same page.
func TestCDPPageWatchesItsDocumentTitle(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)
	type report struct{ url, title string }
	reports := make(chan report, 4)
	hooks := testPageHooks()
	hooks.DocumentTitle = func(url, title string) { reports <- report{url, title} }
	page, err := profile.NewPage(context.Background(), hooks)
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	defer page.Close()
	var binding, script bool
	for _, call := range browser.calls() {
		if call.SessionID == "" {
			continue
		}
		var params map[string]any
		_ = json.Unmarshal(call.Params, &params)
		switch call.Method {
		case "Runtime.addBinding":
			binding = params["name"] == documentTitleBinding && params["executionContextName"] == documentTitleWorld
		case "Page.addScriptToEvaluateOnNewDocument":
			script = params["worldName"] == documentTitleWorld && params["runImmediately"] == true &&
				strings.Contains(params["source"].(string), documentTitleBinding)
		}
	}
	if !binding || !script {
		t.Fatalf("binding in the watcher's world: %v, watcher script in that world on the current document too: %v", binding, script)
	}
	send := func(name, payload string) {
		browser.send(map[string]any{"method": "Runtime.bindingCalled", "sessionId": "S-1", "params": map[string]any{
			"name": name, "payload": payload, "executionContextId": 7,
		}})
	}
	send("pageOwnBinding", `["https://example.test/","Not a title"]`)
	send(documentTitleBinding, `["https://example.test/","Set by script"]`)
	select {
	case got := <-reports:
		if got != (report{"https://example.test/", "Set by script"}) {
			t.Fatalf("DocumentTitle got %+v", got)
		}
	case <-time.After(headlessTestDeadline):
		t.Fatal("the title report never reached DocumentTitle")
	}
	select {
	case got := <-reports:
		t.Fatalf("a second report %+v; another binding's call is not a title", got)
	case <-time.After(50 * time.Millisecond):
	}
}
