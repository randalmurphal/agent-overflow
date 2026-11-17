package browser

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/webview2host"
)

const hostedTestContext = "CTX-1"

// hostedPageResponder answers what attaching one launcher-created page
// asks of the browser: WebView2's target TARGET-1 in browser context
// CTX-1. refuse, when set, is the error Browser.setDownloadBehavior gets.
func hostedPageResponder(refuse string) func(call fakeCDPCall) any {
	return func(call fakeCDPCall) any {
		switch call.Method {
		case "Target.attachToTarget":
			return map[string]any{"sessionId": "S-1"}
		case "Runtime.evaluate":
			return map[string]any{"result": map[string]any{"type": "object", "className": "Window"}}
		case "Page.getFrameTree":
			return map[string]any{"frameTree": map[string]any{"frame": map[string]any{
				"id": "TARGET-1", "loaderId": "L-1", "url": "about:blank",
				"securityOrigin": "://", "mimeType": "text/html",
			}}}
		case "DOM.getDocument":
			return map[string]any{"root": map[string]any{
				"nodeId": 1, "backendNodeId": 1, "nodeType": 9,
				"nodeName": "#document", "localName": "", "nodeValue": "",
			}}
		case "Target.getTargetInfo":
			return map[string]any{"targetInfo": map[string]any{
				"targetId": "TARGET-1", "type": "page", "title": "", "url": "about:blank",
				"attached": true, "canAccessOpener": false, "browserContextId": hostedTestContext,
			}}
		case "Browser.setDownloadBehavior":
			if refuse != "" {
				return fakeCDPError{Code: -32000, Message: refuse}
			}
		}
		return nil
	}
}

func browserCalls(calls []fakeCDPCall, method string) []fakeCDPCall {
	var out []fakeCDPCall
	for _, call := range calls {
		if call.Method == method && call.SessionID == "" {
			out = append(out, call)
		}
	}
	return out
}

// A pane page's browser context keeps its downloads in the jar's AO
// directory, seen from Windows, with the events the Manager accounts with,
// before the Manager has the page and so before it can navigate.
func TestHostedPageKeepsItsDownloadsBeforeItIsHandedOver(t *testing.T) {
	endpoint := startFakeCDPEndpoint(t, hostedPageResponder(""), nil)
	engine, sink := newTestHostedEngine(t, stubRelay{url: endpoint.wsURL}, engineEvents{})
	engine.createTimeout, engine.attachTimeout = 5*time.Second, 5*time.Second
	defer engine.Interrupt()
	profile := testProfile(t, engine)
	answerCreate(t, engine, sink, webview2host.ReportCreated, "TARGET-1")

	page, err := profile.NewPage(context.Background(), pageHooks{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer page.Close()

	calls := endpoint.calls()
	behaviors := browserCalls(calls, "Browser.setDownloadBehavior")
	if len(behaviors) != 1 {
		t.Fatalf("Browser.setDownloadBehavior was called %d times before the page was handed over, want 1", len(behaviors))
	}
	var params struct {
		Behavior         string `json:"behavior"`
		BrowserContextID string `json:"browserContextId"`
		DownloadPath     string `json:"downloadPath"`
		EventsEnabled    bool   `json:"eventsEnabled"`
	}
	if err := json.Unmarshal(behaviors[0].Params, &params); err != nil {
		t.Fatal(err)
	}
	want := `\\wsl.localhost\Test\home\dev\.ao\downloads\jar`
	if params.Behavior != "allowAndName" || params.BrowserContextID != hostedTestContext || params.DownloadPath != want || !params.EventsEnabled {
		t.Fatalf("setDownloadBehavior params %+v, want allowAndName in %s at %s with events", params, hostedTestContext, want)
	}
	for _, call := range calls {
		if call.Method == "Page.navigate" {
			t.Fatal("the page navigated before its downloads were kept")
		}
	}
}

// A browser that refuses the download directory fails the page with its own
// message and closes the controller: WebView2's default, the user's
// Downloads folder with no events, is never kept.
func TestHostedPageFailsWhenTheBrowserRefusesItsDownloadDirectory(t *testing.T) {
	endpoint := startFakeCDPEndpoint(t, hostedPageResponder("Browser context management is not supported."), nil)
	engine, sink := newTestHostedEngine(t, stubRelay{url: endpoint.wsURL}, engineEvents{})
	engine.createTimeout, engine.attachTimeout = 5*time.Second, 5*time.Second
	defer engine.Interrupt()
	profile := testProfile(t, engine)
	answerCreate(t, engine, sink, webview2host.ReportCreated, "TARGET-1")

	page, err := profile.NewPage(context.Background(), pageHooks{})
	if err == nil {
		page.Close()
		t.Fatal("a page whose downloads the browser refused to keep was created")
	}
	if !strings.Contains(err.Error(), "Browser context management is not supported.") || !strings.Contains(err.Error(), "downloads") {
		t.Fatalf("error %v drops the browser's reason", err)
	}
	sink.expectOps(t, webview2host.OpCreate, webview2host.OpClose)
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.targetByPage) != 0 || len(engine.pageDownloads) != 0 {
		t.Fatalf("the refused page left bookkeeping: %v %v", engine.targetByPage, engine.pageDownloads)
	}
}

// A profile whose download directory has no Windows view is refused before
// any page is asked for.
func TestHostedProfileNeedsAReachableDownloadDirectory(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	if _, err := engine.NewProfile(context.Background(), profileOptions{Jar: "/home/dev/repo"}); err == nil {
		t.Fatal("a profile with no download directory was created")
	}
	engine.windowsPath = func(context.Context, string) (string, error) { return "", errors.New("wslpath: exit status 1") }
	_, err := engine.NewProfile(context.Background(), profileOptions{Jar: "/home/dev/repo", DownloadDir: testDownloadDir})
	if err == nil || !strings.Contains(err.Error(), "wslpath: exit status 1") {
		t.Fatalf("got %v, want the mapping's failure", err)
	}
	sink.expectOps(t)
}

func TestHostedDownloadLandedAcceptsOnlyTheGUIDFileInTheDirectory(t *testing.T) {
	dir := `\\wsl.localhost\Ubuntu\home\dev\.ao\downloads\jar`
	for _, tc := range []struct {
		path string
		want bool
	}{
		{dir + `\G-1`, true},
		{`\\wsl$\Ubuntu\home\dev\.ao\downloads\jar\G-1`, true},
		{`//wsl.localhost/Ubuntu/home/dev/.ao/downloads/jar/G-1`, true},
		{dir + `\G-2`, false},
		{dir + `\sub\G-1`, false},
		{`C:\Users\dev\Downloads\G-1`, false},
	} {
		if got := hostedDownloadLanded(dir, "G-1", tc.path); got != tc.want {
			t.Errorf("hostedDownloadLanded(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// newHostedDownloadManager is a Manager on the hosted engine against the
// fake endpoint, with one pane page open and every create answered.
func newHostedDownloadManager(t *testing.T) (*Manager, *fakeCDPEndpoint, Access, string) {
	t.Helper()
	endpoint := startFakeCDPEndpoint(t, hostedPageResponder(""), nil)
	sink := newDirectiveSink()
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{PaneHost: &PaneHostOptions{
		Directive: sink.send,
		Relay:     stubRelay{url: endpoint.wsURL},
	}})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	engine := manager.engine.(*hostedEngine)
	engine.windowsPath = testWindowsPath
	engine.logf = t.Logf
	engine.attachTimeout = 5 * time.Second
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			select {
			case directive := <-sink.feed:
				if directive.Op == webview2host.OpCreate {
					if err := manager.ReportPaneHost(directive.PageID, webview2host.ReportCreated, "TARGET-1"); err != nil {
						t.Errorf("report created: %v", err)
					}
				}
			case <-done:
				return
			}
		}
	}()
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	if _, err := manager.NewCompanionPage(t.Context(), access, ""); err != nil {
		t.Fatalf("open a pane page: %v", err)
	}
	manager.mu.Lock()
	var jarDir string
	for _, scope := range manager.scopes {
		jarDir = scope.downloadDir
	}
	manager.mu.Unlock()
	return manager, endpoint, access, jarDir
}

func waitForDownload(t *testing.T, manager *Manager, access Access, name, state string) ThreadDownload {
	t.Helper()
	deadline := time.Now().Add(hostedTestDeadline)
	for {
		for _, item := range manager.CompanionState(access).Downloads {
			if item.Name == name && item.State == state {
				return item
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never reached %s; the thread lists %+v", name, state, manager.CompanionState(access).Downloads)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func windowsViewOf(t *testing.T, path string) string {
	t.Helper()
	view, err := testWindowsPath(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return view
}

// A download the Windows-side browser reports lands in the thread's list,
// its GUID-named file moves into the workspace's directory, and a cancel
// names the browser context the download runs in.
func TestAHostedDownloadLandsInTheThreadList(t *testing.T) {
	manager, endpoint, access, jarDir := newHostedDownloadManager(t)

	endpoint.send(map[string]any{"method": "Browser.downloadWillBegin", "params": map[string]any{
		"frameId": "TARGET-1", "guid": "G-1", "url": "https://example.test/report.pdf", "suggestedFilename": "report.pdf",
	}})
	waitForDownload(t, manager, access, "report.pdf", downloadStateRunning)
	if err := os.WriteFile(filepath.Join(jarDir, "G-1"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	endpoint.send(map[string]any{"method": "Browser.downloadProgress", "params": map[string]any{
		"guid": "G-1", "totalBytes": 5, "receivedBytes": 5, "state": "completed",
		"filePath": windowsViewOf(t, filepath.Join(jarDir, "G-1")),
	}})
	done := waitForDownload(t, manager, access, "report.pdf", downloadStateCompleted)
	item, err := manager.threadDownload(access.ThreadID, done.ID)
	if err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(item.path); err != nil || string(body) != "hello" {
		t.Fatalf("the finished file at %s reads %q, %v", item.path, body, err)
	}

	endpoint.send(map[string]any{"method": "Browser.downloadWillBegin", "params": map[string]any{
		"frameId": "TARGET-1", "guid": "G-2", "url": "https://example.test/big.bin", "suggestedFilename": "big.bin",
	}})
	running := waitForDownload(t, manager, access, "big.bin", downloadStateRunning)
	if err := manager.CancelThreadDownload(access, running.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	cancels := browserCalls(endpoint.calls(), "Browser.cancelDownload")
	if len(cancels) != 1 || !strings.Contains(string(cancels[0].Params), `"guid":"G-2"`) || !strings.Contains(string(cancels[0].Params), `"browserContextId":"`+hostedTestContext+`"`) {
		t.Fatalf("cancelDownload calls %+v, want G-2 in %s", cancels, hostedTestContext)
	}
}

// A browser that wrote a download anywhere but the jar's directory (its
// own Downloads folder) fails the download rather than counting it.
func TestAHostedDownloadWrittenElsewhereFails(t *testing.T) {
	manager, endpoint, access, _ := newHostedDownloadManager(t)

	endpoint.send(map[string]any{"method": "Browser.downloadWillBegin", "params": map[string]any{
		"frameId": "TARGET-1", "guid": "G-3", "url": "https://example.test/stray.bin", "suggestedFilename": "stray.bin",
	}})
	waitForDownload(t, manager, access, "stray.bin", downloadStateRunning)
	endpoint.send(map[string]any{"method": "Browser.downloadProgress", "params": map[string]any{
		"guid": "G-3", "totalBytes": 5, "receivedBytes": 5, "state": "completed",
		"filePath": `C:\Users\dev\Downloads\G-3`,
	}})
	failed := waitForDownload(t, manager, access, "stray.bin", downloadStateFailed)
	if !strings.Contains(failed.Error, "outside its artifact directory") {
		t.Fatalf("the stray download failed with %q", failed.Error)
	}
}
