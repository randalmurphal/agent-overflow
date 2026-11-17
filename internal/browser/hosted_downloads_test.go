package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/webview2host"
)

// hostedRefusals are the download calls a fake WebView2 refuses, with the
// message real WebView2 gives when it refuses one.
type hostedRefusals struct {
	browserBehavior string
	pageBehavior    string
}

// hostedPageResponder answers what attaching launcher-created pages asks of
// the browser: target TARGET-n attaches as session S-n, whose main frame is
// TARGET-n, as WebView2 numbers a page's main frame by its target.
func hostedPageResponder(refuse hostedRefusals) func(call fakeCDPCall) any {
	return func(call fakeCDPCall) any {
		switch call.Method {
		case "Target.attachToTarget":
			var params struct {
				TargetID string `json:"targetId"`
			}
			_ = json.Unmarshal(call.Params, &params)
			return map[string]any{"sessionId": "S-" + strings.TrimPrefix(params.TargetID, "TARGET-")}
		case "Runtime.evaluate":
			return map[string]any{"result": map[string]any{"type": "object", "className": "Window"}}
		case "Page.getFrameTree":
			return map[string]any{"frameTree": map[string]any{"frame": map[string]any{
				"id": "TARGET-" + strings.TrimPrefix(call.SessionID, "S-"), "loaderId": "L-1", "url": "about:blank",
				"securityOrigin": "://", "mimeType": "text/html",
			}}}
		case "DOM.getDocument":
			return map[string]any{"root": map[string]any{
				"nodeId": 1, "backendNodeId": 1, "nodeType": 9,
				"nodeName": "#document", "localName": "", "nodeValue": "",
			}}
		case "Browser.setDownloadBehavior":
			if refuse.browserBehavior != "" {
				return fakeCDPError{Code: -32000, Message: refuse.browserBehavior}
			}
		case "Page.setDownloadBehavior":
			if refuse.pageBehavior != "" {
				return fakeCDPError{Code: -32000, Message: refuse.pageBehavior}
			}
		}
		return nil
	}
}

func cdpCalls(calls []fakeCDPCall, method, session string) []fakeCDPCall {
	var out []fakeCDPCall
	for _, call := range calls {
		if call.Method == method && call.SessionID == session {
			out = append(out, call)
		}
	}
	return out
}

type downloadBehaviorParams struct {
	Behavior         string  `json:"behavior"`
	BrowserContextID *string `json:"browserContextId"`
	DownloadPath     string  `json:"downloadPath"`
	EventsEnabled    bool    `json:"eventsEnabled"`
}

func behaviorOf(t *testing.T, call fakeCDPCall) downloadBehaviorParams {
	t.Helper()
	var params downloadBehaviorParams
	if err := json.Unmarshal(call.Params, &params); err != nil {
		t.Fatal(err)
	}
	return params
}

// logRecorder keeps what the engine logs.
type logRecorder struct {
	t     *testing.T
	mu    sync.Mutex
	lines []string
}

func (r *logRecorder) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	r.t.Log(line)
	r.mu.Lock()
	r.lines = append(r.lines, line)
	r.mu.Unlock()
}

func (r *logRecorder) contains(text string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range r.lines {
		if strings.Contains(line, text) {
			return true
		}
	}
	return false
}

const testWindowsDownloadDir = `\\wsl.localhost\Test\home\dev\.ao\downloads\jar`

func newHostedDownloadEngine(t *testing.T, refuse hostedRefusals) (*hostedEngine, *directiveSink, *fakeCDPEndpoint, *logRecorder) {
	t.Helper()
	endpoint := startFakeCDPEndpoint(t, hostedPageResponder(refuse), nil)
	engine, sink := newTestHostedEngine(t, stubRelay{url: endpoint.wsURL}, engineEvents{})
	engine.createTimeout, engine.attachTimeout = 5*time.Second, 5*time.Second
	logs := &logRecorder{t: t}
	engine.logf = logs.logf
	t.Cleanup(engine.Interrupt)
	return engine, sink, endpoint, logs
}

// The connection reports downloads without naming a profile's browser
// context, which WebView2 refuses, and the page's own session keeps its
// profile's downloads in the jar's AO directory, seen from Windows, before
// the Manager has the page and so before it can navigate.
func TestHostedPageKeepsItsDownloadsBeforeItIsHandedOver(t *testing.T) {
	engine, sink, endpoint, _ := newHostedDownloadEngine(t, hostedRefusals{})
	profile := testProfile(t, engine)
	answerCreate(t, engine, sink, webview2host.ReportCreated, "TARGET-1")

	page, err := profile.NewPage(context.Background(), pageHooks{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer page.Close()

	calls := endpoint.calls()
	browserLevel := cdpCalls(calls, "Browser.setDownloadBehavior", "")
	if len(browserLevel) != 1 {
		t.Fatalf("Browser.setDownloadBehavior was called %d times, want once for the connection", len(browserLevel))
	}
	if got := behaviorOf(t, browserLevel[0]); got.Behavior != "default" || !got.EventsEnabled || got.BrowserContextID != nil || got.DownloadPath != "" {
		t.Fatalf("Browser.setDownloadBehavior %+v, want default with events and no browser context", got)
	}
	pageLevel := cdpCalls(calls, "Page.setDownloadBehavior", "S-1")
	if len(pageLevel) != 1 {
		t.Fatalf("Page.setDownloadBehavior was called %d times on the page before it was handed over, want 1", len(pageLevel))
	}
	if got := behaviorOf(t, pageLevel[0]); got.Behavior != "allowAndName" || got.DownloadPath != testWindowsDownloadDir {
		t.Fatalf("Page.setDownloadBehavior %+v, want allowAndName into %s", got, testWindowsDownloadDir)
	}
	if len(cdpCalls(calls, "Page.navigate", "S-1")) != 0 {
		t.Fatal("the page navigated before its downloads were kept")
	}
	if reason := page.(*hostedPage).downloadsUnmanaged(); reason != "" {
		t.Fatalf("a kept profile reports its downloads unmanaged: %s", reason)
	}
}

// A browser that refuses the download directory still opens the page; the
// refusal is logged with the browser's message and the page reports its
// downloads unmanaged.
func TestHostedPageOpensWhenTheBrowserRefusesItsDownloadDirectory(t *testing.T) {
	const refusal = "Failed to find browser context for id 4CA7DE3AB5D51E01A053A34468D63FD7"
	engine, sink, _, logs := newHostedDownloadEngine(t, hostedRefusals{pageBehavior: refusal})
	profile := testProfile(t, engine)
	answerCreate(t, engine, sink, webview2host.ReportCreated, "TARGET-1")

	page, err := profile.NewPage(context.Background(), pageHooks{})
	if err != nil {
		t.Fatalf("a refused download directory failed the page: %v", err)
	}
	defer page.Close()
	if reason := page.(*hostedPage).downloadsUnmanaged(); !strings.Contains(reason, refusal) {
		t.Fatalf("the page reports %q, want the browser's refusal", reason)
	}
	if !logs.contains(refusal) {
		t.Fatal("the refusal was not logged")
	}
	sink.expectOps(t, webview2host.OpCreate)
}

// Without download events nothing could be accounted for, so pages turn
// downloads off instead of letting WebView2 save them to its own folder.
func TestHostedPagesTurnDownloadsOffWithoutDownloadEvents(t *testing.T) {
	const refusal = "Browser.setDownloadBehavior is not allowed"
	engine, sink, endpoint, logs := newHostedDownloadEngine(t, hostedRefusals{browserBehavior: refusal})
	profile := testProfile(t, engine)
	answerCreate(t, engine, sink, webview2host.ReportCreated, "TARGET-1")

	page, err := profile.NewPage(context.Background(), pageHooks{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer page.Close()
	pageLevel := cdpCalls(endpoint.calls(), "Page.setDownloadBehavior", "S-1")
	if len(pageLevel) != 1 || behaviorOf(t, pageLevel[0]).Behavior != "deny" {
		t.Fatalf("Page.setDownloadBehavior calls %+v, want one deny", pageLevel)
	}
	if reason := page.(*hostedPage).downloadsUnmanaged(); !strings.Contains(reason, refusal) {
		t.Fatalf("the page reports %q, want the browser's refusal", reason)
	}
	if !logs.contains(refusal) {
		t.Fatal("the refusal was not logged")
	}
}

// WebView2 drops a profile's download setting when the session that set it
// last goes, so a closing page hands it to another page of the profile
// before its own session detaches.
func TestHostedDownloadSettingMovesToAnotherPageBeforeItsHolderDetaches(t *testing.T) {
	engine, sink, endpoint, _ := newHostedDownloadEngine(t, hostedRefusals{})
	profile := testProfile(t, engine)
	answerCreate(t, engine, sink, webview2host.ReportCreated, "TARGET-1")
	first, err := profile.NewPage(context.Background(), pageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	answerCreate(t, engine, sink, webview2host.ReportCreated, "TARGET-2")
	second, err := profile.NewPage(context.Background(), pageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(cdpCalls(endpoint.calls(), "Page.setDownloadBehavior", "S-1")); n != 1 {
		t.Fatalf("the first page set the behavior %d times before the second closed, want 1", n)
	}

	second.Close()
	deadline := time.Now().Add(hostedTestDeadline)
	for {
		calls := endpoint.calls()
		// The first page's second setting is the hand-over.
		handover, detach, settings := -1, -1, 0
		for i, call := range calls {
			if call.Method == "Page.setDownloadBehavior" && call.SessionID == "S-1" {
				if settings++; settings == 2 {
					handover = i
				}
			}
			if call.Method == "Target.detachFromTarget" && strings.Contains(string(call.Params), `"S-2"`) && detach < 0 {
				detach = i
			}
		}
		if handover >= 0 && detach >= 0 {
			if handover > detach {
				t.Fatalf("the setting moved to the first page (call %d) after the second's session detached (call %d)", handover, detach)
			}
			if got := behaviorOf(t, calls[handover]); got.Behavior != "allowAndName" || got.DownloadPath != testWindowsDownloadDir {
				t.Fatalf("the hand-over set %+v", got)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("hand-over call %d, detach call %d", handover, detach)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A page that did not set the profile's behavior last closes without
// touching it.
func TestHostedDownloadSettingStaysWhenAnotherPageCloses(t *testing.T) {
	engine, sink, endpoint, _ := newHostedDownloadEngine(t, hostedRefusals{})
	profile := testProfile(t, engine)
	answerCreate(t, engine, sink, webview2host.ReportCreated, "TARGET-1")
	first, err := profile.NewPage(context.Background(), pageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	answerCreate(t, engine, sink, webview2host.ReportCreated, "TARGET-2")
	second, err := profile.NewPage(context.Background(), pageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	first.Close()
	if n := len(cdpCalls(endpoint.calls(), "Page.setDownloadBehavior", "S-2")); n != 1 {
		t.Fatalf("the holder set the behavior %d times, want 1", n)
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
// fake endpoint, every create answered with the next target, and one pane
// page open.
func newHostedDownloadManager(t *testing.T, refuse hostedRefusals) (*Manager, *fakeCDPEndpoint, *downloadEventLog, Access, string) {
	t.Helper()
	endpoint := startFakeCDPEndpoint(t, hostedPageResponder(refuse), nil)
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
	log := &downloadEventLog{}
	manager.SetEventSink(log.sink)
	engine := manager.engine.(*hostedEngine)
	engine.windowsPath = testWindowsPath
	engine.logf = t.Logf
	engine.attachTimeout = 5 * time.Second
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	var targets atomic.Int64
	go func() {
		for {
			select {
			case directive := <-sink.feed:
				if directive.Op == webview2host.OpCreate {
					target := fmt.Sprintf("TARGET-%d", targets.Add(1))
					if err := manager.ReportPaneHost(directive.PageID, webview2host.ReportCreated, target); err != nil {
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
	return manager, endpoint, log, access, jarDir
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

func beginDownload(endpoint *fakeCDPEndpoint, guid, name string) {
	endpoint.send(map[string]any{"method": "Browser.downloadWillBegin", "params": map[string]any{
		"frameId": "TARGET-1", "guid": guid, "url": "https://example.test/" + name, "suggestedFilename": name,
	}})
}

// A download the Windows-side browser reports lands in the thread's list,
// its GUID-named file moves into the workspace's directory, the
// Zone.Identifier stream file WebView2 writes beside it is removed, and a
// cancel names no browser context, which is the form WebView2 accepts.
func TestAHostedDownloadLandsInTheThreadList(t *testing.T) {
	manager, endpoint, _, access, jarDir := newHostedDownloadManager(t, hostedRefusals{})

	beginDownload(endpoint, "G-1", "report.pdf")
	waitForDownload(t, manager, access, "report.pdf", downloadStateRunning)
	file := filepath.Join(jarDir, "G-1")
	if err := os.WriteFile(file, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file+":Zone.Identifier", []byte("[ZoneTransfer]\nZoneId=3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	endpoint.send(map[string]any{"method": "Browser.downloadProgress", "params": map[string]any{
		"guid": "G-1", "totalBytes": 5, "receivedBytes": 5, "state": "completed",
		"filePath": windowsViewOf(t, file),
	}})
	done := waitForDownload(t, manager, access, "report.pdf", downloadStateCompleted)
	item, err := manager.threadDownload(access.ThreadID, done.ID)
	if err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(item.path); err != nil || string(body) != "hello" {
		t.Fatalf("the finished file at %s reads %q, %v", item.path, body, err)
	}
	if _, err := os.Stat(file + ":Zone.Identifier"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the Zone.Identifier stream file is still in the jar directory: %v", err)
	}

	beginDownload(endpoint, "G-2", "big.bin")
	running := waitForDownload(t, manager, access, "big.bin", downloadStateRunning)
	if err := manager.CancelThreadDownload(access, running.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	cancels := cdpCalls(endpoint.calls(), "Browser.cancelDownload", "")
	if len(cancels) != 1 || string(cancels[0].Params) != `{"guid":"G-2"}` {
		t.Fatalf("cancelDownload calls %+v, want G-2 alone", cancels)
	}
	// Chromium accepts the cancel of a download that has just finished and
	// reports nothing more (observed on WebView2), so the finish it already
	// sent settles it and the file stays.
	if got := waitForDownload(t, manager, access, "big.bin", downloadStateRunning); got.ID != running.ID {
		t.Fatalf("the canceled download lists as %+v", got)
	}
	big := filepath.Join(jarDir, "G-2")
	if err := os.WriteFile(big, []byte("all of it"), 0o600); err != nil {
		t.Fatal(err)
	}
	endpoint.send(map[string]any{"method": "Browser.downloadProgress", "params": map[string]any{
		"guid": "G-2", "totalBytes": 9, "receivedBytes": 9, "state": "completed", "filePath": windowsViewOf(t, big),
	}})
	finished := waitForDownload(t, manager, access, "big.bin", downloadStateCompleted)
	if item, err := manager.finishedDownloadPath(access, finished.ID); err != nil {
		t.Fatalf("the finish that beat the cancel lost its file: %v", err)
	} else if body, err := os.ReadFile(item.path); err != nil || string(body) != "all of it" {
		t.Fatalf("the finished file reads %q, %v", body, err)
	}

	// A cancel the engine took ends with the engine's cancel report.
	beginDownload(endpoint, "G-5", "stopped.bin")
	stopping := waitForDownload(t, manager, access, "stopped.bin", downloadStateRunning)
	if err := manager.CancelThreadDownload(access, stopping.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	endpoint.send(map[string]any{"method": "Browser.downloadProgress", "params": map[string]any{
		"guid": "G-5", "totalBytes": 9, "receivedBytes": 3, "state": "canceled",
	}})
	if got := waitForDownload(t, manager, access, "stopped.bin", downloadStateCanceled); got.Error != "" {
		t.Fatalf("the user's cancel lists as %+v", got)
	}
}

// A browser that wrote a download anywhere but the jar's directory (its
// own Downloads folder) fails the download rather than counting it.
func TestAHostedDownloadWrittenElsewhereFails(t *testing.T) {
	manager, endpoint, _, access, _ := newHostedDownloadManager(t, hostedRefusals{})

	beginDownload(endpoint, "G-3", "stray.bin")
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

// When the browser refuses AO's download directory the pages still open,
// the user is told once for the profile, and a download that starts in it
// is cancelled and listed as failed with the reason.
func TestUnmanagedHostedDownloadsAreToldOnceAndStopped(t *testing.T) {
	const refusal = "Failed to find browser context"
	manager, endpoint, log, access, _ := newHostedDownloadManager(t, hostedRefusals{pageBehavior: refusal})
	if _, err := manager.NewCompanionPage(t.Context(), access, ""); err != nil {
		t.Fatalf("a second page of the profile: %v", err)
	}
	var warnings []CompanionEvent
	for _, event := range log.take() {
		if event.Kind == "downloads-unmanaged" {
			warnings = append(warnings, event)
		}
	}
	if len(warnings) != 1 || warnings[0].ThreadID != access.ThreadID || !strings.Contains(warnings[0].Error, refusal) {
		t.Fatalf("warnings %+v, want one for the thread with the refusal", warnings)
	}

	beginDownload(endpoint, "G-4", "off.bin")
	failed := waitForDownload(t, manager, access, "off.bin", downloadStateFailed)
	if !strings.Contains(failed.Error, "downloads are off in this browser profile") || !strings.Contains(failed.Error, refusal) {
		t.Fatalf("the download failed with %q", failed.Error)
	}
	deadline := time.Now().Add(hostedTestDeadline)
	for len(cdpCalls(endpoint.calls(), "Browser.cancelDownload", "")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the unmanaged download was never cancelled")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
