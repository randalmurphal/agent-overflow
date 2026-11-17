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
// directory (internal/browser/hosted_downloads.go). This runs the engine's
// protocol on the real runtime, on pages of a named profile, and proves:
//
//   - Browser.setDownloadBehavior with no browser context and behavior
//     "default" is accepted and turns on Browser.download* events on the
//     connection, for downloads in every profile;
//   - Page.setDownloadBehavior allowAndName on a page's own session is
//     accepted and holds for its whole profile; set again from page 1
//     before page 2, which set it last, closes, it still holds after the
//     close (without that WebView2 falls back to the user's Downloads
//     folder, observed 2026-11-16, which is why the engine hands it over);
//   - a download lands as its GUID in that directory (a \\wsl.localhost
//     path when AO_WEBVIEW2_DOWNLOAD_SMOKE_DIR names one), never in the
//     user's Downloads folder, and Browser.downloadProgress reports that
//     path; two downloads with one suggested name are two files;
//   - Browser.cancelDownload with no browser context cancels a download of
//     the named profile and leaves no file behind.
//
// Browser.setDownloadBehavior naming the profile's browser context is
// refused ("Failed to find browser context"), so the engine never sends it.
// Whether WebView2's own download flyout opens has no CDP signal; the smoke
// window stays up for AO_WEBVIEW2_DOWNLOAD_SMOKE_HOLD (a Go duration) after
// the first download for a look.
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
	downloadDir := t.TempDir()
	if base := os.Getenv("AO_WEBVIEW2_DOWNLOAD_SMOKE_DIR"); base != "" {
		downloadDir = base + `\` + fmt.Sprint(time.Now().UnixNano())
		if err := os.Mkdir(downloadDir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var hold time.Duration
	if raw := os.Getenv("AO_WEBVIEW2_DOWNLOAD_SMOKE_HOLD"); raw != "" {
		if hold, err = time.ParseDuration(raw); err != nil {
			t.Fatalf("AO_WEBVIEW2_DOWNLOAD_SMOKE_HOLD: %v", err)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	userDownloads := filepath.Join(home, "Downloads")
	stamp := fmt.Sprint(time.Now().UnixNano())

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		if strings.HasPrefix(name, "slow") {
			w.Header().Set("Content-Length", "1048576")
			_, _ = w.Write(make([]byte, 1024))
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write(make([]byte, 4096))
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
	create := func(pageID string) string {
		t.Helper()
		host.Apply(Directive{Op: OpCreate, PageID: pageID, ProfileID: "smoke"})
		for {
			select {
			case report := <-reports:
				if report.pageID != pageID || report.kind == ReportFindUnavailable {
					continue
				}
				if report.kind != ReportCreated {
					t.Fatalf("create %s answered %+v", pageID, report)
				}
				return report.detail
			case <-time.After(60 * time.Second):
				t.Fatalf("no created report for %s", pageID)
			}
		}
	}
	target1 := create("page-1")
	host.Apply(Directive{Op: OpBounds, PageID: "page-1", X: 0, Y: 0, W: 640, H: 480})
	host.Apply(Directive{Op: OpShow, PageID: "page-1"})
	target2 := create("page-2")

	cdp := newSmokeCDP(t, smokeDialCDP(t, host.CDPPort()))
	cdp.call("Browser.setDownloadBehavior", map[string]any{"behavior": "default", "eventsEnabled": true}, "", nil)
	attach := func(targetID string) string {
		var attached struct {
			SessionID string `json:"sessionId"`
		}
		cdp.call("Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, "", &attached)
		cdp.call("Page.setDownloadBehavior", map[string]any{"behavior": "allowAndName", "downloadPath": downloadDir}, attached.SessionID, nil)
		return attached.SessionID
	}
	session1 := attach(target1)
	download := func(name string) smokeDownloadProgress {
		t.Helper()
		cdp.call("Page.navigate", map[string]any{"url": server.URL + "/" + name}, session1, nil)
		guid := cdp.downloadBegan(name)
		finished := cdp.downloadSettled(guid)
		if finished.State != "completed" {
			t.Fatalf("%s ended %s", name, finished.State)
		}
		want := downloadDir + `\` + guid
		if _, err := os.Stat(filepath.Join(userDownloads, name)); err == nil {
			// Removed before failing, so a failed run leaves nothing there.
			_ = os.Remove(filepath.Join(userDownloads, name))
			t.Fatalf("%s was saved to %s", name, userDownloads)
		}
		if !strings.EqualFold(finished.FilePath, want) {
			t.Fatalf("%s reported %q, want %q", name, finished.FilePath, want)
		}
		stat, err := os.Stat(want)
		if err != nil || stat.Size() != 4096 {
			t.Fatalf("%s is not 4096 bytes at %s: %v", name, want, err)
		}
		t.Logf("%s landed at %s", name, finished.FilePath)
		return finished
	}
	download("solo-" + stamp + ".bin")
	// Page 2 holds the setting now. Closing it would put the profile back on
	// WebView2's own behavior, so page 1 takes the setting first, as the
	// engine's hand-over does.
	attach(target2)
	cdp.call("Page.setDownloadBehavior", map[string]any{"behavior": "allowAndName", "downloadPath": downloadDir}, session1, nil)
	host.Apply(Directive{Op: OpClose, PageID: "page-2"})
	for closed := false; !closed; {
		select {
		case report := <-reports:
			closed = report.pageID == "page-2" && report.kind == ReportClosed
		case <-time.After(30 * time.Second):
			t.Fatal("page-2 never closed")
		}
	}
	download("afterclose-" + stamp + ".bin")
	first := download("same-" + stamp + ".bin")
	if hold > 0 {
		t.Logf("holding the window %s: look for WebView2's download flyout", hold)
		time.Sleep(hold)
	}
	if second := download("same-" + stamp + ".bin"); second.GUID == first.GUID {
		t.Fatal("two downloads share one GUID")
	}
	// The backend removes this stream file before moving the download.
	if _, err := os.Stat(downloadDir + `\` + first.GUID + ":Zone.Identifier"); err == nil {
		t.Logf("WebView2 wrote a Zone.Identifier stream beside %s", first.GUID)
	}

	cdp.call("Page.navigate", map[string]any{"url": server.URL + "/slow-" + stamp + ".bin"}, session1, nil)
	slow := cdp.downloadBegan("slow-" + stamp + ".bin")
	cdp.call("Browser.cancelDownload", map[string]any{"guid": slow}, "", nil)
	if canceled := cdp.downloadSettled(slow); canceled.State != "canceled" {
		t.Fatalf("the held download ended %s after the cancel", canceled.State)
	}
	if _, err := os.Stat(downloadDir + `\` + slow); err == nil {
		t.Fatalf("the canceled download left %s behind", slow)
	}
}

// smokeCDP is a browser-level CDP connection. One goroutine reads it, since
// a read that times out closes a coder/websocket connection; replies answer
// calls, and events wait in order for the download reads.
type smokeCDP struct {
	t      *testing.T
	conn   *websocket.Conn
	next   int64
	feed   chan smokeCDPMessage
	events []smokeCDPMessage
}

type smokeCDPMessage struct {
	ID        int64           `json:"id"`
	Method    string          `json:"method"`
	SessionID string          `json:"sessionId"`
	Params    json.RawMessage `json:"params"`
	Result    json.RawMessage `json:"result"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type smokeDownloadProgress struct {
	GUID     string `json:"guid"`
	State    string `json:"state"`
	FilePath string `json:"filePath"`
}

func newSmokeCDP(t *testing.T, conn *websocket.Conn) *smokeCDP {
	c := &smokeCDP{t: t, conn: conn, next: 100, feed: make(chan smokeCDPMessage, 256)}
	t.Cleanup(func() { conn.CloseNow() })
	go func() {
		defer close(c.feed)
		for {
			_, payload, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var message smokeCDPMessage
			if err := json.Unmarshal(payload, &message); err != nil {
				t.Errorf("CDP frame %q: %v", payload, err)
				return
			}
			c.feed <- message
		}
	}()
	return c
}

func (c *smokeCDP) read(what string, deadline time.Time) smokeCDPMessage {
	c.t.Helper()
	select {
	case message, ok := <-c.feed:
		if !ok {
			c.t.Fatalf("the CDP connection closed waiting for %s", what)
		}
		return message
	case <-time.After(time.Until(deadline)):
		c.t.Fatalf("no %s within the deadline", what)
	}
	return smokeCDPMessage{}
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
func (c *smokeCDP) event(what string, match func(smokeCDPMessage) bool) {
	c.t.Helper()
	for i, message := range c.events {
		if message.SessionID == "" && match(message) {
			c.events = append(c.events[:i], c.events[i+1:]...)
			return
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		message := c.read(what, deadline)
		if message.Method == "" {
			continue
		}
		if message.SessionID == "" && match(message) {
			return
		}
		c.events = append(c.events, message)
	}
}

// downloadBegan answers the GUID of the next browser-level download of name.
func (c *smokeCDP) downloadBegan(name string) string {
	c.t.Helper()
	var begin struct {
		GUID              string `json:"guid"`
		SuggestedFilename string `json:"suggestedFilename"`
	}
	c.event("Browser.downloadWillBegin for "+name, func(message smokeCDPMessage) bool {
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

// downloadSettled answers the terminal browser-level progress of guid.
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
