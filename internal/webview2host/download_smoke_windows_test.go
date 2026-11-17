//go:build windows

package webview2host

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The backend's hosted engine keeps a pane page's downloads in the jar's AO
// directory with Browser.setDownloadBehavior on the page's own browser
// context (internal/browser/hosted_downloads.go). This runs those calls on
// the real runtime the way the engine does, on a named profile's page:
// the context's id from Target.getTargetInfo, allowAndName into a directory
// with events enabled, a download that must land as its GUID there, and a
// Browser.cancelDownload naming the context that must cancel a held one.
//
// AO_WEBVIEW2_DOWNLOAD_SMOKE_DIR names the directory, for example the
// `wslpath -w` view of a WSL directory (\\wsl.localhost\<distro>\...); a
// fresh temporary directory otherwise. Whether WebView2's own download
// flyout opens is visible in the smoke window, which stays up for
// AO_WEBVIEW2_DOWNLOAD_SMOKE_HOLD (a Go duration) after the download.
//
//	AO_NATIVE_BROWSER_SMOKE=1 go test -run TestDownloadSmoke ./internal/webview2host
func TestDownloadSmokeKeepsAProfilesDownloadsInItsDirectory(t *testing.T) {
	if os.Getenv("AO_NATIVE_BROWSER_SMOKE") != "1" {
		t.Skip("set AO_NATIVE_BROWSER_SMOKE=1 to run against the installed WebView2 runtime")
	}
	dataDir, err := os.MkdirTemp("", "ao-download-smoke-")
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
	downloadDir := os.Getenv("AO_WEBVIEW2_DOWNLOAD_SMOKE_DIR")
	if downloadDir == "" {
		downloadDir = t.TempDir()
	}
	var hold time.Duration
	if raw := os.Getenv("AO_WEBVIEW2_DOWNLOAD_SMOKE_HOLD"); raw != "" {
		if hold, err = time.ParseDuration(raw); err != nil {
			t.Fatalf("AO_WEBVIEW2_DOWNLOAD_SMOKE_HOLD: %v", err)
		}
	}

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="`+strings.TrimPrefix(r.URL.Path, "/")+`"`)
		switch r.URL.Path {
		case "/file.bin":
			w.Header().Set("Content-Length", "4096")
			_, _ = w.Write(make([]byte, 4096))
		case "/slow.bin":
			w.Header().Set("Content-Length", "1048576")
			_, _ = w.Write(make([]byte, 1024))
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

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
		ui.do(host.env.release)
	})

	host.Apply(Directive{Op: OpCreate, PageID: "page-1", ProfileID: "smoke"})
	var targetID string
	for targetID == "" {
		select {
		case report := <-reports:
			switch report.kind {
			case ReportCreated:
				targetID = report.detail
			case ReportFindUnavailable:
			default:
				t.Fatalf("create answered %+v", report)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("no created report")
		}
	}
	host.Apply(Directive{Op: OpBounds, PageID: "page-1", X: 0, Y: 0, W: 640, H: 480})
	host.Apply(Directive{Op: OpShow, PageID: "page-1"})

	cdp := &smokeCDP{t: t, conn: smokeDialCDP(t, host.CDPPort()), next: 100}
	t.Cleanup(func() { cdp.conn.CloseNow() })

	var info struct {
		TargetInfo struct {
			BrowserContextID string `json:"browserContextId"`
		} `json:"targetInfo"`
	}
	cdp.call("Target.getTargetInfo", map[string]any{"targetId": targetID}, "", &info)
	contextID := info.TargetInfo.BrowserContextID
	t.Logf("named profile page %s is in browser context %q", targetID, contextID)

	cdp.call("Browser.setDownloadBehavior", map[string]any{
		"behavior": "allowAndName", "browserContextId": contextID,
		"downloadPath": downloadDir, "eventsEnabled": true,
	}, "", nil)

	var attached struct {
		SessionID string `json:"sessionId"`
	}
	cdp.call("Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, "", &attached)

	// A finished download is the GUID-named file in the directory.
	cdp.call("Page.navigate", map[string]any{"url": server.URL + "/file.bin"}, attached.SessionID, nil)
	guid := cdp.downloadBegan("file.bin")
	finished := cdp.downloadSettled(guid)
	if finished.State != "completed" {
		t.Fatalf("file.bin ended %s", finished.State)
	}
	t.Logf("file.bin reported filePath %q", finished.FilePath)
	stat, err := os.Stat(filepath.Join(downloadDir, guid))
	if err != nil {
		t.Fatalf("the download is not its GUID in %s: %v", downloadDir, err)
	}
	if stat.Size() != 4096 {
		t.Fatalf("the download holds %d bytes, want 4096", stat.Size())
	}
	if !strings.EqualFold(filepath.Base(finished.FilePath), guid) {
		t.Fatalf("the reported path %q does not name the GUID %s", finished.FilePath, guid)
	}
	if hold > 0 {
		t.Logf("holding the window %s: check whether WebView2's download flyout opened", hold)
		time.Sleep(hold)
	}

	// A cancel naming the context cancels a download in it.
	cdp.call("Page.navigate", map[string]any{"url": server.URL + "/slow.bin"}, attached.SessionID, nil)
	slow := cdp.downloadBegan("slow.bin")
	cdp.call("Browser.cancelDownload", map[string]any{"guid": slow, "browserContextId": contextID}, "", nil)
	if canceled := cdp.downloadSettled(slow); canceled.State != "canceled" {
		t.Fatalf("slow.bin ended %s after the cancel", canceled.State)
	}
}

// smokeCDP is a browser-level CDP connection read on the test goroutine:
// replies answer calls, and events wait in order for the download reads.
type smokeCDP struct {
	t      *testing.T
	conn   *websocket.Conn
	next   int64
	events []smokeCDPMessage
}

type smokeCDPMessage struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type smokeDownloadProgress struct {
	GUID     string `json:"guid"`
	State    string `json:"state"`
	FilePath string `json:"filePath"`
}

func (c *smokeCDP) read(what string, deadline time.Time) smokeCDPMessage {
	c.t.Helper()
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	_, payload, err := c.conn.Read(ctx)
	if err != nil {
		c.t.Fatalf("waiting for %s: %v", what, err)
	}
	var message smokeCDPMessage
	if err := json.Unmarshal(payload, &message); err != nil {
		c.t.Fatalf("CDP frame %q: %v", payload, err)
	}
	return message
}

// call sends one command and fails the test with the browser's own message
// if it refuses.
func (c *smokeCDP) call(method string, params any, sessionID string, result any) {
	c.t.Helper()
	c.next++
	id := c.next
	frame := map[string]any{"id": id, "method": method, "params": params}
	if sessionID != "" {
		frame["sessionId"] = sessionID
	}
	body, err := json.Marshal(frame)
	if err != nil {
		c.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.conn.Write(ctx, websocket.MessageText, body); err != nil {
		c.t.Fatalf("send %s: %v", method, err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		message := c.read("the reply to "+method, deadline)
		if message.Method != "" {
			c.events = append(c.events, message)
			continue
		}
		if message.ID != id {
			continue
		}
		if message.Error != nil {
			c.t.Fatalf("%s was refused: %s", method, message.Error.Message)
		}
		if result != nil {
			if err := json.Unmarshal(message.Result, result); err != nil {
				c.t.Fatalf("%s result %s: %v", method, message.Result, err)
			}
		}
		return
	}
}

// event answers the first browser-level event match accepts, buffered or
// next to arrive.
func (c *smokeCDP) event(what string, match func(smokeCDPMessage) bool) smokeCDPMessage {
	c.t.Helper()
	for i, message := range c.events {
		if match(message) {
			c.events = append(c.events[:i], c.events[i+1:]...)
			return message
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		message := c.read(what, deadline)
		if message.Method != "" && match(message) {
			return message
		}
		if message.Method != "" {
			c.events = append(c.events, message)
		}
	}
}

// downloadBegan answers the GUID of the download of name.
func (c *smokeCDP) downloadBegan(name string) string {
	c.t.Helper()
	var begin struct {
		GUID              string `json:"guid"`
		SuggestedFilename string `json:"suggestedFilename"`
	}
	c.event(fmt.Sprintf("Browser.downloadWillBegin for %s (events were not enabled for this context)", name), func(message smokeCDPMessage) bool {
		if message.Method != "Browser.downloadWillBegin" {
			return false
		}
		if err := json.Unmarshal(message.Params, &begin); err != nil {
			c.t.Fatal(err)
		}
		return begin.SuggestedFilename == name
	})
	return begin.GUID
}

// downloadSettled answers the terminal progress of guid.
func (c *smokeCDP) downloadSettled(guid string) smokeDownloadProgress {
	c.t.Helper()
	var progress smokeDownloadProgress
	c.event("the end of download "+guid, func(message smokeCDPMessage) bool {
		if message.Method != "Browser.downloadProgress" {
			return false
		}
		if err := json.Unmarshal(message.Params, &progress); err != nil {
			c.t.Fatal(err)
		}
		return progress.GUID == guid && progress.State != "inProgress"
	})
	return progress
}
