//go:build windows

package webview2host

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// The backend's CDP page learns a document's title from the document
// itself (internal/browser/cdp_page.go), because Target.targetInfoChanged
// reports a navigation with the browser's placeholder title and nothing
// when the document sets its own. This runs that route on the real runtime,
// on a page of a named profile, and proves:
//
//   - Runtime.addBinding with executionContextName and
//     Page.addScriptToEvaluateOnNewDocument with worldName and
//     runImmediately are accepted on the page's session, and the script
//     runs in each new document once the Page domain is enabled (without
//     Page.enable WebView2 creates the world and runs nothing; the CDP page
//     has it enabled by chromedp's attach);
//   - a MutationObserver the script puts on the Document node when it is
//     evaluated, before the document has a root and with no event, sees
//     the parsed <title>, a later assignment by script, and the document
//     document.open and document.write replace it with, each arriving as
//     Runtime.bindingCalled from that world; a page script does not see
//     the binding.
//
// It logs whether any Target.targetInfoChanged carried either title, the
// observation the route exists for (none did, 2026-11-16).
//
//	AO_NATIVE_BROWSER_SMOKE=1 go test -run TestTitleSmoke ./internal/webview2host
func TestTitleSmokeReportsADocumentsTitleFromAnIsolatedWorld(t *testing.T) {
	if os.Getenv("AO_NATIVE_BROWSER_SMOKE") != "1" {
		t.Skip("set AO_NATIVE_BROWSER_SMOKE=1 to run against the installed WebView2 runtime")
	}
	dataDir, err := os.MkdirTemp("", "ao-title-smoke-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := clearUserDataDir(dataDir); err != nil {
			t.Errorf("clear %s: %v", dataDir, err)
		}
		if err := os.Remove(dataDir); err != nil {
			t.Errorf("remove %s: %v", dataDir, err)
		}
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><title>Parsed</title><script>
setTimeout(() => { document.title = typeof __smokeTitle + " by script" }, 300)
setTimeout(() => { document.open(); document.write("<!doctype html><title>Rewritten</title>"); document.close() }, 600)
</script>`))
	}))
	t.Cleanup(server.Close)

	ui := startSmokeUIThread(t)
	reports := make(chan smokeReport, 16)
	host, err := New(Config{
		HostWindow:  func() uintptr { return ui.hwnd },
		UserDataDir: dataDir,
		Logf:        t.Logf,
		Report: func(pageID string, kind ReportKind, detail string) {
			reports <- smokeReport{pageID, kind, detail}
		},
		OnMain: ui.do,
		Post:   ui.post,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		host.Close()
		ui.do(host.env.release)
	})
	host.Apply(Directive{Op: OpCreate, PageID: "page-1", ProfileID: "smoke"})
	var targetID string
	for targetID == "" {
		select {
		case report := <-reports:
			if report.pageID == "page-1" && report.kind == ReportCreated {
				targetID = report.detail
			} else if report.pageID == "page-1" && report.kind == ReportCreateFailed {
				t.Fatalf("create answered %+v", report)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("no created report")
		}
	}

	cdp := newSmokeCDP(t, smokeDialCDP(t, host.CDPPort()))
	cdp.call("Target.setDiscoverTargets", map[string]any{"discover": true}, "", nil)
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	cdp.call("Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, "", &attached)
	session := attached.SessionID
	cdp.call("Runtime.enable", map[string]any{}, session, nil)
	cdp.call("Page.enable", map[string]any{}, session, nil)
	cdp.call("Runtime.addBinding", map[string]any{"name": "__smokeTitle", "executionContextName": "smoke-title"}, session, nil)
	cdp.call("Page.addScriptToEvaluateOnNewDocument", map[string]any{
		"source":         `let last = ""; new MutationObserver(() => { if (document.title && document.title !== last) __smokeTitle(last = document.title) }).observe(document, {subtree: true, childList: true, characterData: true})`,
		"worldName":      "smoke-title",
		"runImmediately": true,
	}, session, nil)
	cdp.call("Page.navigate", map[string]any{"url": server.URL + "/"}, session, nil)

	var bound []string
	infoTitles := map[string]bool{}
	deadline := time.Now().Add(10 * time.Second)
	for len(bound) < 3 && time.Now().Before(deadline) {
		messages := cdp.events
		cdp.events = nil
		messages = append(messages, cdp.collect(250*time.Millisecond)...)
		for _, message := range messages {
			switch message.Method {
			case "Runtime.bindingCalled":
				var call struct{ Name, Payload string }
				_ = json.Unmarshal(message.Params, &call)
				if call.Name == "__smokeTitle" {
					bound = append(bound, call.Payload)
				}
			case "Target.targetInfoChanged":
				var info struct {
					TargetInfo struct{ Title string } `json:"targetInfo"`
				}
				_ = json.Unmarshal(message.Params, &info)
				infoTitles[info.TargetInfo.Title] = true
			}
		}
	}
	t.Logf("titles Target.targetInfoChanged carried: %v", infoTitles)
	if len(bound) != 3 || bound[0] != "Parsed" || bound[1] != "undefined by script" || bound[2] != "Rewritten" {
		t.Fatalf("the isolated world reported %q, want [Parsed, undefined by script, Rewritten]", bound)
	}
}
