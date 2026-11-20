//go:build !windows

package browser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/testutil/mockexec"
)

// The headless engine is exercised against a FAKE Chromium: a shell script
// that records its argv, prints the "DevTools listening on" line every
// Chromium prints, and then sleeps until it is asked to exit or is killed.
// The endpoint that line advertises is the same loopback CDP fake the hosted
// engine's tests use. No test over it starts a real browser, and none
// downloads one; the real launch is the manual AO_HEADLESS_CHROMIUM_SMOKE
// gate (TestHeadlessChromiumReal). The scripts are POSIX shell, and the
// headless engine ships on the serve hosts, which are Linux and macOS, so
// the tests over it do not build on Windows.

const headlessTestDeadline = 10 * time.Second

// fakeChromium is one scripted browser: where it wrote its argv, the pid it
// runs under, and the CDP endpoint it advertised.
type fakeChromium struct {
	path     string
	argvFile string
	pidFile  string
	// crashDirFile records the crash handler's directory it was given.
	crashDirFile string
	calls        func() []fakeCDPCall
	// send writes one event to the browser's CDP connection.
	send func(event map[string]any)
	// hangUp drops the CDP connection, the way a browser that died does.
	hangUp func()
	// ignoreClose makes Browser.close a no-op, the way a wedged browser
	// answers it. Otherwise the browser exits on SIGTERM.
	ignoreClose atomic.Bool
	// onClose, when set, runs as Browser.close arrives.
	onClose atomic.Pointer[func()]
	// stallFrameTree leaves every Page.getFrameTree unanswered.
	stallFrameTree atomic.Bool
	// stallBrowserCalls leaves every Target.closeTarget and
	// Browser.cancelDownload unanswered.
	stallBrowserCalls atomic.Bool
}

// writeFakeChromium installs the script and the endpoint behind it.
//
// `exec sleep` rather than a shell loop on purpose: the process the profile
// kills is then the one whose pid the script recorded, so "Dispose killed
// the browser" is provable rather than inferred.
func writeFakeChromium(t *testing.T, name string) *fakeChromium {
	t.Helper()
	return writeAnnouncingFakeChromium(t, name, nil)
}

// writeAnnouncingFakeChromium is writeFakeChromium whose endpoint also
// sends the events announce returns before answering a command.
func writeAnnouncingFakeChromium(t *testing.T, name string, announce func(call fakeCDPCall) []map[string]any) *fakeChromium {
	t.Helper()
	dir := t.TempDir()
	browser := &fakeChromium{
		path:         filepath.Join(dir, name),
		argvFile:     filepath.Join(dir, "argv"),
		pidFile:      filepath.Join(dir, "pid"),
		crashDirFile: filepath.Join(dir, "crash-dir"),
	}
	var targets, sessions atomic.Int64
	endpoint := startFakeCDPEndpoint(t, func(call fakeCDPCall) any {
		switch call.Method {
		case "Target.createTarget":
			return map[string]any{"targetId": fmt.Sprintf("T-%d", targets.Add(1))}
		case "Target.attachToTarget":
			return map[string]any{"sessionId": fmt.Sprintf("S-%d", sessions.Add(1))}
		case "Runtime.evaluate":
			// chromedp asks the fresh target what `self` is, to find out
			// whether it attached to a worker. A page answers Window.
			return map[string]any{"result": map[string]any{"type": "object", "className": "Window"}}
		case "Target.closeTarget", "Browser.cancelDownload":
			if browser.stallBrowserCalls.Load() {
				return fakeCDPNoReply{}
			}
		case "Page.getFrameTree":
			if browser.stallFrameTree.Load() {
				return fakeCDPNoReply{}
			}
			// A page whose frames attached before the attach, as a popup's
			// made before its adoption did: a child and its child.
			frame := func(id, parent string) map[string]any {
				return map[string]any{"id": id, "parentId": parent, "loaderId": "L-1", "url": "about:blank", "securityOrigin": "://", "mimeType": "text/html"}
			}
			return map[string]any{"frameTree": map[string]any{
				"frame": frame("F-1", ""),
				"childFrames": []any{map[string]any{
					"frame":       frame("F-child", "F-1"),
					"childFrames": []any{map[string]any{"frame": frame("F-grandchild", "F-child")}},
				}},
			}}
		case "DOM.getDocument":
			// Attaching to a target that already exists, as adopting a
			// popup does, reads its document at once.
			return map[string]any{"root": map[string]any{
				"nodeId": 1, "backendNodeId": 1, "nodeType": 9,
				"nodeName": "#document", "localName": "", "nodeValue": "",
			}}
		case "Browser.close":
			// The endpoint path ends in the pid of the script that
			// advertised it, which is still this test's unreaped child.
			pid, err := strconv.Atoi(filepath.Base(call.Path))
			if err != nil {
				t.Errorf("Browser.close arrived on %q, which names no browser", call.Path)
			} else if !browser.ignoreClose.Load() {
				_ = syscall.Kill(pid, syscall.SIGTERM)
			}
			if onClose := browser.onClose.Load(); onClose != nil {
				(*onClose)()
			}
		}
		return nil
	}, announce)
	browser.calls, browser.send, browser.hangUp = endpoint.calls, endpoint.send, endpoint.hangUp
	wsURL := endpoint.wsURL
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + shellQuote(browser.argvFile) + "\n" +
		"printf '%s' \"$BREAKPAD_DUMP_LOCATION\" > " + shellQuote(browser.crashDirFile) + "\n" +
		"echo $$ > " + shellQuote(browser.pidFile) + "\n" +
		"echo \"DevTools listening on " + wsURL + "/devtools/browser/$$\" >&2\n" +
		"exec sleep 300\n"
	mockexec.Write(t, browser.path, script)
	return browser
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'" }

// argv is what the last launch of this script was given.
func (c *fakeChromium) argv(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(c.argvFile)
	if err != nil {
		t.Fatalf("the fake Chromium recorded no argv (it was never launched): %v", err)
	}
	return strings.Split(strings.TrimRight(string(body), "\n"), "\n")
}

// crashDir is the crash handler's directory the last launch was given.
func (c *fakeChromium) crashDir(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(c.crashDirFile)
	if err != nil {
		t.Fatalf("the fake Chromium recorded no environment (it was never launched): %v", err)
	}
	return string(body)
}

func (c *fakeChromium) pid(t *testing.T) int {
	t.Helper()
	body, err := os.ReadFile(c.pidFile)
	if err != nil {
		t.Fatalf("the fake Chromium recorded no pid (it was never launched): %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatalf("the fake Chromium recorded pid %q: %v", body, err)
	}
	return pid
}

func (c *fakeChromium) methods() []string {
	seen := c.calls()
	names := make([]string, 0, len(seen))
	for _, call := range seen {
		names = append(names, call.Method)
	}
	return names
}

// alive answers whether a recorded pid is still a process. A child that
// exited but was not reaped still is one, so false also proves the reap. A
// killed browser is reaped before Dispose returns, so this needs no polling.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// waitForPID reads a pid the script records before anything else, for a
// launch that is still in flight.
func waitForPID(t *testing.T, pidFile string) int {
	t.Helper()
	var pid int
	eventually(t, "the fake Chromium to record its pid", func() bool {
		body, err := os.ReadFile(pidFile)
		if err != nil || !strings.HasSuffix(string(body), "\n") {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(body)))
		return err == nil
	})
	return pid
}

func newTestHeadlessEngine(t *testing.T, binary string) *headlessEngine {
	t.Helper()
	engine := &headlessEngine{
		configDir: t.TempDir(),
		// The engine's own temp root, so an ephemeral profile lands here
		// and Start's sweep never reads the machine's real one.
		tempRoot: t.TempDir(),
		binary:   binary,
		reaper:   procutiltest.Reaper(),
		events:   engineEvents{},
		logf:     func(format string, args ...any) { t.Logf(format, args...) },
		// Long enough that a fake that exits when asked is never killed
		// instead.
		closeTimeout: headlessTestDeadline,
		profiles:     make(map[*headlessProfile]struct{}),
		pageProfile:  make(map[ids.PageHandle]*headlessProfile),
	}
	t.Cleanup(engine.Stop)
	if err := engine.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	return engine
}

func testHeadlessProfile(t *testing.T, engine *headlessEngine, workspace string, persist bool) *headlessProfile {
	t.Helper()
	profile, err := engine.NewProfile(context.Background(), profileOptions{
		Jar: jarKey(workspace), DownloadDir: filepath.Join(t.TempDir(), "downloads"), Ephemeral: !persist,
		Allow: func([]frameID, string) (bool, bool) { return true, true },
	})
	if err != nil {
		t.Fatalf("new profile for %s: %v", workspace, err)
	}
	return profile.(*headlessProfile)
}

func testPageHooks() pageHooks {
	return pageHooks{
		Console: func(ConsoleLog) {},
		PageURL: func() string { return "" },
		Allow:   func(string) bool { return true },
	}
}

// currentProcess is the Chromium the profile runs now.
func (p *headlessProfile) currentProcess() *chromiumProcess {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.current.process
}

// popupCreated is the targetCreated event Chromium sends for a page one of
// its pages opened.
func popupCreated(handle, opener string) map[string]any {
	return map[string]any{"method": "Target.targetCreated", "params": map[string]any{"targetInfo": map[string]any{
		"targetId": handle, "type": "page", "title": "", "url": "about:blank",
		"attached": false, "canAccessOpener": false, "openerId": opener,
	}}}
}

// downloadWillBegin is the event Chromium sends as a download starts in
// frame.
func downloadWillBegin(guid, frame string) map[string]any {
	return map[string]any{"method": "Browser.downloadWillBegin", "params": map[string]any{
		"frameId": frame, "guid": guid, "url": "https://example.test/file.bin", "suggestedFilename": "file.bin",
	}}
}

// downloadProgressEvent is the event Chromium sends as download guid
// advances or ends in state.
func downloadProgressEvent(guid, state string) map[string]any {
	return map[string]any{"method": "Browser.downloadProgress", "params": map[string]any{
		"guid": guid, "totalBytes": 5, "receivedBytes": 5, "state": state,
	}}
}

func ptrValue(v *int64) any {
	if v == nil {
		return "absent"
	}
	return *v
}

// lostPages records the pages a headless engine reports lost with their
// browser (PageCrashed).
type lostPages struct {
	mu      sync.Mutex
	handles []string
}

func recordLostPages(engine *headlessEngine) *lostPages {
	closed := &lostPages{}
	engine.events.PageCrashed = func(handle ids.PageHandle, _ string) { closed.add(string(handle)) }
	engine.events.PageClosed = func(handle ids.PageHandle) {
		panic("a lost browser's page was reported closed, not crashed: " + handle)
	}
	return closed
}

func (c *lostPages) add(handle string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handles = append(c.handles, handle)
}

func (c *lostPages) count(handle string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, closed := range c.handles {
		if closed == handle {
			n++
		}
	}
	return n
}
