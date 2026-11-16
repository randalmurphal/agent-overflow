//go:build windows

package webview2host

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/coder/websocket"
	"golang.org/x/sys/windows"
)

// The pane environment is held for the launcher's lifetime, which costs a
// browser process tree only if WebView2's browser process outlives the last
// controller. This decides it on the real runtime with the app's setup: the
// pane environment's arguments (its --remote-debugging-port included), a
// hidden clip-container controller, and a CDP connection held the way the
// relay holds one. Closing that connection and the profile's controllers, as
// the backend's engine stop does, must end the browser process while the
// environment is still held; the next create must start a new one whose
// page loads and whose debugging endpoint answers.
//
//	AO_NATIVE_BROWSER_SMOKE=1 go test -run TestProcessSmoke ./internal/webview2host
func TestProcessSmokeBrowserExitsWithItsLastController(t *testing.T) {
	if os.Getenv("AO_NATIVE_BROWSER_SMOKE") != "1" {
		t.Skip("set AO_NATIVE_BROWSER_SMOKE=1 to run against the installed WebView2 runtime")
	}
	dataDir, err := os.MkdirTemp("", "ao-process-smoke-")
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
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		host.Close()
		ui.do(host.releaseEnvironment)
	})
	await := func(pageID string, want ReportKind) {
		t.Helper()
		deadline := time.After(60 * time.Second)
		for {
			select {
			case report := <-reports:
				if report.pageID != pageID || report.kind == ReportFindUnavailable {
					continue
				}
				if report.kind != want {
					t.Fatalf("page %s answered %+v, want %s", pageID, report, want)
				}
				return
			case <-deadline:
				t.Fatalf("no %s report for %s", want, pageID)
			}
		}
	}
	viewOf := func(pageID string) *iCoreWebView2 {
		host.mu.Lock()
		defer host.mu.Unlock()
		return host.pages[pageID].view
	}

	host.Apply(Directive{Op: OpCreate, PageID: "page-1", ProfileID: "smoke"})
	await("page-1", ReportCreated)
	var firstPID uint32
	var pidErr error
	ui.do(func() { firstPID, pidErr = smokeBrowserProcessID(viewOf("page-1")) })
	if pidErr != nil {
		t.Fatal(pidErr)
	}
	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, firstPID)
	if err != nil {
		t.Fatalf("open browser process %d: %v", firstPID, err)
	}
	defer windows.CloseHandle(process)
	relay := smokeDialCDP(t, host.CDPPort())

	// The engine stop: the CDP connection goes, then the profile's
	// controllers (close-profile is what hostedProfile.Dispose sends).
	if err := relay.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close the CDP connection: %v", err)
	}
	host.Apply(Directive{Op: OpCloseProfile, ProfileID: "smoke"})
	await("page-1", ReportClosed)
	host.mu.Lock()
	held := host.env10 != nil
	host.mu.Unlock()
	if !held {
		t.Fatal("the environment was released; this test needs it held")
	}
	switch event, err := windows.WaitForSingleObject(process, 30_000); {
	case err != nil:
		t.Fatalf("wait for browser process %d: %v", firstPID, err)
	case event != windows.WAIT_OBJECT_0:
		t.Fatalf("browser process %d still runs 30s after its last controller closed", firstPID)
	}

	host.Apply(Directive{Op: OpCreate, PageID: "page-2", ProfileID: "smoke"})
	await("page-2", ReportCreated)
	var secondPID uint32
	ui.do(func() { secondPID, pidErr = smokeBrowserProcessID(viewOf("page-2")) })
	if pidErr != nil {
		t.Fatal(pidErr)
	}
	if secondPID == firstPID {
		t.Fatalf("the second controller reports the exited browser process %d", firstPID)
	}
	smokeDialCDP(t, host.CDPPort()).CloseNow()
	var navErr error
	ui.do(func() { navErr = smokeNavigateToString(viewOf("page-2"), "<title>ao process smoke</title>") })
	if navErr != nil {
		t.Fatalf("NavigateToString: %v", navErr)
	}
	deadline := time.Now().Add(20 * time.Second)
	for title := ""; title != "ao process smoke"; time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the page never loaded; title %q", title)
		}
		var titleErr error
		ui.do(func() { title, titleErr = smokeDocumentTitle(viewOf("page-2")) })
		if titleErr != nil {
			t.Fatal(titleErr)
		}
	}
}

// smokeBrowserProcessID is ICoreWebView2::get_BrowserProcessId. UI thread.
func smokeBrowserProcessID(view *iCoreWebView2) (uint32, error) {
	var pid uint32
	hr, _, _ := view.vtbl.GetBrowserProcessID.Call(uintptr(unsafe.Pointer(view)), uintptr(unsafe.Pointer(&pid)))
	if err := hresult(hr); err != nil || pid == 0 {
		return 0, fmt.Errorf("get_BrowserProcessId: %v (pid %d)", err, pid)
	}
	return pid, nil
}

// smokeDocumentTitle is ICoreWebView2::get_DocumentTitle. UI thread.
func smokeDocumentTitle(view *iCoreWebView2) (string, error) {
	var title *uint16
	hr, _, _ := view.vtbl.GetDocumentTitle.Call(uintptr(unsafe.Pointer(view)), uintptr(unsafe.Pointer(&title)))
	if err := hresult(hr); err != nil {
		return "", fmt.Errorf("get_DocumentTitle: %w", err)
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(title))
	return windows.UTF16PtrToString(title), nil
}

// smokeDialCDP opens a browser-level CDP connection on the pane
// environment's debugging port and answers one command, as the relay does.
func smokeDialCDP(t *testing.T, port int) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json/version", port), nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("debugging endpoint: %v", err)
	}
	var version struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	err = json.NewDecoder(response.Body).Decode(&version)
	response.Body.Close()
	if err != nil || !strings.HasPrefix(version.WebSocketDebuggerURL, "ws://127.0.0.1:") {
		t.Fatalf("debugging endpoint answered %q: %v", version.WebSocketDebuggerURL, err)
	}
	conn, _, err := websocket.Dial(ctx, version.WebSocketDebuggerURL, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", version.WebSocketDebuggerURL, err)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"id":1,"method":"Target.getTargets"}`)); err != nil {
		t.Fatalf("send a CDP command: %v", err)
	}
	if _, reply, err := conn.Read(ctx); err != nil || !strings.Contains(string(reply), `"id":1`) {
		t.Fatalf("CDP reply %q: %v", reply, err)
	}
	return conn
}
