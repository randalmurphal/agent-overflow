//go:build !windows

package browser

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/testutil/mockexec"
)

// The headless engine is exercised against a FAKE Chromium: a shell script
// that records its argv, prints the "DevTools listening on" line every
// Chromium prints, and then sleeps until it is asked to exit or is killed.
// The endpoint that line advertises is the same loopback CDP fake the hosted
// engine's tests use. No test here starts a real browser, and none
// downloads one. The real launch is the manual AO_HEADLESS_CHROMIUM_SMOKE
// gate at the bottom of this file. The scripts are POSIX shell, and the
// headless engine ships on the serve hosts, which are Linux and macOS, so
// this file does not build on Windows.

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
		pageProfile:  make(map[string]*headlessProfile),
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
		Jar: workspace, DownloadDir: filepath.Join(t.TempDir(), "downloads"), Ephemeral: !persist,
		Allow: func([]string, string) (bool, bool) { return true, true },
	})
	if err != nil {
		t.Fatalf("new profile for %s: %v", workspace, err)
	}
	return profile.(*headlessProfile)
}

// A serve host can lose its browser to a package upgrade while it is up, so
// the engine re-checks at start rather than trusting what selection found —
// and the error names the setting, because a backend with no window has no
// Settings screen in front of the person reading its journal.
func TestHeadlessEngineStartRefusesAMissingBinaryByName(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "chromium")
	engine := &headlessEngine{binary: missing, logf: t.Logf}
	err := engine.Start(context.Background())
	if err == nil {
		t.Fatal("an engine with no browser behind it started")
	}
	if !strings.Contains(err.Error(), chromiumSettingKey) || !strings.Contains(err.Error(), missing) {
		t.Fatalf("error %v names neither the file nor the setting", err)
	}
	if engine.Running() {
		t.Fatal("a refused start still reports running")
	}
}

// One Chromium PER PROFILE is what isolates two workspaces' logins on a
// deployment with no per-view network session to do it with. Two profiles
// must therefore be two processes, each on its own user-data directory
// under the profile tree Clear site data deletes.
func TestHeadlessEngineLaunchesOneChromiumPerProfile(t *testing.T) {
	first := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, first.path)
	one := testHeadlessProfile(t, engine, "/home/dev/one", true)
	if _, err := one.ensureBrowser(); err != nil {
		t.Fatalf("launch the first profile: %v", err)
	}
	firstPID := first.pid(t)

	// A second profile relaunches the same script, so its argv and pid are
	// read from a second copy of it.
	second := writeFakeChromium(t, "chromium")
	engine.binary = second.path
	two := testHeadlessProfile(t, engine, "/home/dev/two", true)
	if _, err := two.ensureBrowser(); err != nil {
		t.Fatalf("launch the second profile: %v", err)
	}
	if secondPID := second.pid(t); secondPID == firstPID {
		t.Fatal("two workspace profiles shared one Chromium process")
	}

	oneDir := filepath.Join(engine.configDir, browserProfileDir, one.handle, "chromium")
	twoDir := filepath.Join(engine.configDir, browserProfileDir, two.handle, "chromium")
	if oneDir == twoDir {
		t.Fatal("two workspaces resolved to one user-data directory")
	}
	assertChromiumArgv(t, first.argv(t), oneDir)
	assertChromiumArgv(t, second.argv(t), twoDir)
}

// Chromium's crash handler writes its database even when nothing crashes,
// and it writes into the directory the launch names, the profile's own, so
// nothing of a workspace's browser lands in the user's Chrome directory.
func TestHeadlessProfileKeepsCrashReportsInItsDirectory(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)
	if _, err := profile.ensureBrowser(); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if got, want := browser.crashDir(t), filepath.Join(profile.userDataDir, "Crash Reports"); got != want {
		t.Fatalf("the crash handler writes to %q, want %q", got, want)
	}
}

// wantChromiumArgv is the whole launch line, in order. --no-sandbox is the
// one flag that must never appear: it is a whole security boundary, and
// nothing in the launch depends on the user it runs as.
func wantChromiumArgv(userDataDir string) []string {
	return []string{
		"--headless=new",
		"--user-data-dir=" + userDataDir,
		"--disable-gpu",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-popup-blocking",
		"--remote-debugging-port=0",
		"about:blank",
	}
}

// assertChromiumArgv checks what the process was actually given.
func assertChromiumArgv(t *testing.T, argv []string, wantUserDataDir string) {
	t.Helper()
	if want := wantChromiumArgv(wantUserDataDir); !slices.Equal(argv, want) {
		t.Fatalf("the fake Chromium was launched with %q, want exactly %q", argv, want)
	}
}

// The same line without a process: exactly these arguments in this order,
// the DevTools port the launch reads its endpoint from, and no --no-sandbox
// for any user.
func TestChromiumArgsAreTheWholeLaunchLine(t *testing.T) {
	got := chromiumArgs("/data/chromium")
	for _, arg := range got {
		if arg == "--no-sandbox" || strings.HasPrefix(arg, "--no-sandbox=") {
			t.Fatalf("the launch disabled the renderer sandbox: %q", got)
		}
	}
	if want := wantChromiumArgv("/data/chromium"); !slices.Equal(got, want) {
		t.Fatalf("chromiumArgs = %q, want exactly %q", got, want)
	}
}

// Downloads land ONLY in the AO artifact directory, and allowAndName is
// what the Manager's own bookkeeping reads back. Events are what make the
// download reports arrive at all.
func TestHeadlessProfilePinsDownloadsToTheArtifactDirectory(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", true)
	if _, err := profile.ensureBrowser(); err != nil {
		t.Fatalf("launch: %v", err)
	}

	var params struct {
		Behavior      string `json:"behavior"`
		DownloadPath  string `json:"downloadPath"`
		EventsEnabled bool   `json:"eventsEnabled"`
	}
	var found bool
	for _, call := range browser.calls() {
		if call.Method != "Browser.setDownloadBehavior" {
			continue
		}
		if err := json.Unmarshal(call.Params, &params); err != nil {
			t.Fatalf("decode setDownloadBehavior params: %v", err)
		}
		found = true
	}
	if !found {
		t.Fatalf("the launch never pinned downloads: %v", browser.methods())
	}
	if params.DownloadPath != profile.downloadDir {
		t.Fatalf("downloads land in %q, want the artifact directory %q", params.DownloadPath, profile.downloadDir)
	}
	if params.Behavior != "allowAndName" {
		t.Fatalf("download behavior is %q; the Manager renames from the GUID-named file allowAndName writes", params.Behavior)
	}
	if !params.EventsEnabled {
		t.Fatal("download events are off, so no download would ever be reported or capped")
	}
}

// The first page launches the browser; every later one joins it. A process
// per page would multiply a workspace's memory by its tab count.
func TestHeadlessProfileReusesItsBrowserForEveryPage(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", true)

	first, err := profile.NewPage(context.Background(), testPageHooks())
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	launchedPID := browser.pid(t)
	second, err := profile.NewPage(context.Background(), testPageHooks())
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if browser.pid(t) != launchedPID {
		t.Fatal("a second page launched a second Chromium")
	}
	if first.Handle() == second.Handle() {
		t.Fatalf("both pages report handle %q", first.Handle())
	}
	// The handle IS the CDP target id on this engine, and both pages are
	// bound to the profile that made them.
	for _, driver := range []pageDriver{first, second} {
		owner, ok := engine.profileForPage(driver.Handle())
		if !ok || owner != profile {
			t.Fatalf("page %q is bound to %v", driver.Handle(), owner)
		}
	}
}

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
		if !page.OwnsFrame(frame) {
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

// Two pages attached, the headless engine still pauses a request once: the
// browser-wide policy is the only one.
func TestHeadlessEnginePausesARequestOnce(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", true)
	for range 2 {
		if _, err := profile.NewPage(context.Background(), testPageHooks()); err != nil {
			t.Fatalf("new page: %v", err)
		}
	}
	assertOnePolicyPause(t, browser.calls, browser.send)
}

func testPageHooks() pageHooks {
	return pageHooks{
		Console: func(ConsoleLog) {},
		PageURL: func() string { return "" },
		Allow:   func(string) bool { return true },
	}
}

// Dispose is what stops a workspace's browser, and the Manager calls it
// when the workspace's last page closes. An ephemeral profile's directory
// goes with it, because "site data is not persisted" is a promise about
// disk and Chromium has no in-memory profile to keep it with. A persisted
// profile's browser is asked to exit, so it writes what it holds in memory,
// and an ephemeral one's is killed.
func TestHeadlessProfileDisposeStopsTheBrowserAndHonoursPersistence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		persist    bool
		wantOnDisk bool
		// wantExit is how the fake ended: it exits on SIGTERM when asked.
		wantExit string
	}{
		{name: "an ephemeral profile leaves nothing behind", persist: false, wantExit: "signal: killed"},
		{name: "a persisted profile keeps its logins", persist: true, wantOnDisk: true, wantExit: "signal: terminated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			browser := writeFakeChromium(t, "chromium")
			engine := newTestHeadlessEngine(t, browser.path)
			profile := testHeadlessProfile(t, engine, "/home/dev/repo", tc.persist)
			browserCtx, err := profile.ensureBrowser()
			if err != nil {
				t.Fatalf("launch: %v", err)
			}
			// The profile's event handlers run on browserCtx, so a context that
			// has ended when Browser.close arrives is one nothing the exit
			// reports can reach.
			var listening atomic.Bool
			recordListening := func() { listening.Store(browserCtx.Err() == nil) }
			browser.onClose.Store(&recordListening)
			process := profile.currentProcess()
			pid := browser.pid(t)
			if !alive(pid) {
				t.Fatalf("the fake Chromium %d died on its own", pid)
			}

			if err := profile.Dispose(context.Background()); err != nil {
				t.Fatalf("dispose: %v", err)
			}
			if alive(pid) {
				t.Fatalf("Chromium %d outlived its profile", pid)
			}
			if got := process.exitStatus(); got != tc.wantExit {
				t.Fatalf("the browser ended with %q, want %q", got, tc.wantExit)
			}
			if asked := slices.Contains(browser.methods(), "Browser.close"); asked != tc.persist {
				t.Fatalf("Browser.close sent = %v, want %v", asked, tc.persist)
			}
			if listening.Load() {
				t.Fatal("the profile was still listening to the browser it asked to exit")
			}
			if _, err := os.Stat(profile.userDataDir); os.IsNotExist(err) == tc.wantOnDisk {
				t.Fatalf("user data directory %q on disk = %v, want %v", profile.userDataDir, !os.IsNotExist(err), tc.wantOnDisk)
			}
			// Twice is a no-op: the Manager disposes on the last page
			// close and again on shutdown.
			if err := profile.Dispose(context.Background()); err != nil {
				t.Fatalf("second dispose: %v", err)
			}
		})
	}
}

// currentProcess is the Chromium the profile runs now.
func (p *headlessProfile) currentProcess() *chromiumProcess {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.current.process
}

// A persisted profile's browser that does not exit when asked is killed once
// the close timeout passes, and the log says so. Dispose still returns with
// the browser gone.
func TestHeadlessProfileKillsABrowserThatDoesNotExitWhenAsked(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	browser.ignoreClose.Store(true)
	engine := newTestHeadlessEngine(t, browser.path)
	engine.closeTimeout = 50 * time.Millisecond
	var logMu sync.Mutex
	var logged []string
	engine.logf = func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", true)
	if _, err := profile.ensureBrowser(); err != nil {
		t.Fatalf("launch: %v", err)
	}
	process := profile.currentProcess()

	disposed := make(chan error, 1)
	go func() { disposed <- profile.Dispose(context.Background()) }()
	select {
	case err := <-disposed:
		if err != nil {
			t.Fatalf("dispose: %v", err)
		}
	case <-time.After(headlessTestDeadline):
		t.Fatal("Dispose waited on a browser that ignored Browser.close")
	}
	if got := process.exitStatus(); got != "signal: killed" {
		t.Fatalf("the browser ended with %q, want it killed", got)
	}
	if !slices.Contains(browser.methods(), "Browser.close") {
		t.Fatal("the browser was never asked to exit")
	}
	logMu.Lock()
	defer logMu.Unlock()
	if !slices.ContainsFunc(logged, func(line string) bool { return strings.Contains(line, "did not exit when asked") }) {
		t.Fatalf("nothing logged the kill; the log is %q", logged)
	}
}

// A profile created ephemerally is marked, so the run that follows a crash
// can attribute it. Without this the sweep has nothing to act on and the
// whole mechanism is inert.
func TestAnEphemeralProfileMarksItsRoot(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)

	if profile.ephemeralRoot == "" {
		t.Fatal("an ephemeral profile got no root of its own")
	}
	if filepath.Dir(profile.ephemeralRoot) != engine.tempRoot {
		t.Fatalf("the ephemeral root %q is not under the engine's temp root %q", profile.ephemeralRoot, engine.tempRoot)
	}
	pid, ok := readEphemeralOwner(profile.ephemeralRoot)
	if !ok || pid != os.Getpid() {
		t.Fatalf("the root's owner marker reads %d/%v, want this process %d", pid, ok, os.Getpid())
	}
}

// Stop is the idle close's landing point (the Manager stops the engine
// idleBrowserDelay after the last profile goes) and shutdown's. Either way
// no browser may survive it.
func TestHeadlessEngineStopStopsEveryProfile(t *testing.T) {
	first := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, first.path)
	one := testHeadlessProfile(t, engine, "/home/dev/one", true)
	if _, err := one.ensureBrowser(); err != nil {
		t.Fatalf("launch one: %v", err)
	}
	second := writeFakeChromium(t, "chromium")
	engine.binary = second.path
	two := testHeadlessProfile(t, engine, "/home/dev/two", false)
	if _, err := two.ensureBrowser(); err != nil {
		t.Fatalf("launch two: %v", err)
	}

	engine.Stop()
	for _, pid := range []int{first.pid(t), second.pid(t)} {
		if alive(pid) {
			t.Fatalf("Chromium %d outlived the engine", pid)
		}
	}
	if engine.Running() {
		t.Fatal("a stopped engine reports running")
	}
	if _, err := os.Stat(two.ephemeralRoot); !os.IsNotExist(err) {
		t.Fatalf("the ephemeral profile's directory survives Stop: %v", err)
	}
}

// A browser that refuses to start says why — the sandbox refusal, the
// missing library — and the operator only ever sees what the engine
// carries out of it.
//
// The script exits as soon as it has printed, so the reason survives only
// if nothing closes the output before it is read to the end. The refused
// process is reaped before the launch returns.
func TestHeadlessProfileSurfacesWhyTheBrowserRefusedToStart(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "chromium")
	pidFile := filepath.Join(dir, "pid")
	script := "#!/bin/sh\n" +
		"echo $$ > " + shellQuote(pidFile) + "\n" +
		"echo 'Failed to move to new namespace: Operation not permitted' >&2\n" +
		"exit 1\n"
	mockexec.Write(t, binary, script)
	engine := newTestHeadlessEngine(t, binary)
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", true)

	started := time.Now()
	_, err := profile.ensureBrowser()
	if err == nil {
		t.Fatal("a browser that exited 1 reported a live connection")
	}
	if !strings.Contains(err.Error(), "Failed to move to new namespace") {
		t.Fatalf("error %v drops what the browser said; a sandbox refusal would be unreadable", err)
	}
	if !strings.Contains(err.Error(), binary) {
		t.Fatalf("error %v does not name the binary that failed", err)
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("error %v drops how the browser exited", err)
	}
	if elapsed := time.Since(started); elapsed > headlessTestDeadline {
		t.Fatalf("the launch took %s: the wait is not bounded", elapsed)
	}
	if pid := waitForPID(t, pidFile); alive(pid) {
		t.Fatalf("the refused browser %d was never reaped", pid)
	}
}

// popupCreated is the targetCreated event Chromium sends for a page one of
// its pages opened.
func popupCreated(handle, opener string) map[string]any {
	return map[string]any{"method": "Target.targetCreated", "params": map[string]any{"targetInfo": map[string]any{
		"targetId": handle, "type": "page", "title": "", "url": "about:blank",
		"attached": false, "canAccessOpener": false, "openerId": opener,
	}}}
}

// Popups reach the Manager only through the browser-level event handlers,
// and the Manager adopts one with CDP commands on the same connection. The
// subscriptions are made before discovery is enabled, so a popup announced
// during the handshake is reported, and adopting a popup does not wait on
// the handler that reported it.
//
// The handshake popup is announced once, before the reply, as Chromium
// reports the targets that exist when discovery is enabled. A subscription
// made after that reply never hears of it.
func TestHeadlessProfileReportsPopupsAndCanAdoptThem(t *testing.T) {
	var handshake sync.Once
	browser := writeAnnouncingFakeChromium(t, "chromium", func(call fakeCDPCall) []map[string]any {
		switch call.Method {
		case "Target.setDiscoverTargets":
			var events []map[string]any
			handshake.Do(func() { events = []map[string]any{popupCreated("P-handshake", "T-elsewhere")} })
			return events
		case "Target.createTarget":
			return []map[string]any{popupCreated("P-page", "T-1")}
		}
		return nil
	})
	engine := newTestHeadlessEngine(t, browser.path)
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", true)

	var mu sync.Mutex
	reported := make(map[string]enginePopup)
	adopted := make(chan error, 1)
	engine.events.PopupOpened = func(popup enginePopup) {
		mu.Lock()
		reported[popup.Handle] = popup
		mu.Unlock()
		if popup.Handle != "P-page" {
			return
		}
		// What Manager.popupOpened does with a popup it keeps: the report
		// returns at once and the adoption runs on its own goroutine.
		go func() {
			driver, err := profile.AttachPage(context.Background(), popup.Handle, testPageHooks())
			if err == nil {
				driver.Close()
			}
			adopted <- err
		}()
	}
	created := make(chan error, 1)
	go func() {
		_, err := profile.NewPage(context.Background(), testPageHooks())
		created <- err
	}()

	select {
	case err := <-adopted:
		if err != nil {
			t.Fatalf("adopt the popup: %v", err)
		}
	case <-time.After(headlessTestDeadline):
		t.Fatal("adopting the popup never finished")
	}
	select {
	case err := <-created:
		if err != nil {
			t.Fatalf("the page that opened the popup: %v", err)
		}
	case <-time.After(headlessTestDeadline):
		t.Fatal("creating the page that opened the popup never finished")
	}
	mu.Lock()
	defer mu.Unlock()
	for handle, opener := range map[string]string{"P-handshake": "T-elsewhere", "P-page": "T-1"} {
		got, ok := reported[handle]
		if !ok || got.Opener != opener || got.Profile != profile.handle {
			t.Fatalf("popup %s was reported as %+v (reported: %v)", handle, got, ok)
		}
		if owner, bound := engine.profileForPage(handle); !bound || owner != profile {
			t.Fatalf("popup %s is not bound to the profile that saw it", handle)
		}
	}
}

// The profile enables the jar's policy on the browser session at launch, so
// it sees a page Chromium opened by itself before the Manager adopts it as
// well as every page's requests, and answers what it pauses there with the
// Manager's policy for the jar. In a jar two worktrees share, a request has
// the authority of the page its frame belongs to, or of its popup's openers.
// A request whose owner is not resolved (a popup's request before Chromium
// reports the popup, a subframe no page reported, a popup whose opener
// closed) is allowed at once what a page with no workspace's authority may
// open; any other waits for one within the owner bound, then is refused.
func TestHeadlessJarPolicyAnswersWithTheRequestsOwner(t *testing.T) {
	manager, jars := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	files := map[string]string{}
	for name, dir := range map[string]string{"one": proj.one, "two": proj.two} {
		path := filepath.Join(dir, name+".html")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		files[name] = "file://" + canonical(t, path)
	}
	a, b := proj.access(manager, "a", proj.one), proj.access(manager, "b", proj.two)
	setJar(t, manager, a, JarProject)
	setJar(t, manager, b, JarProject)
	pageA := openPage(t, manager, a, "https://example.test/a")
	openPage(t, manager, b, "https://example.test/b")
	openerA := paneHandle(manager, a, pageA.ID)
	jars.ownFrame("F-of-A", openerA)

	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	engine.events.PopupOpened = func(enginePopup) {}
	engine.ownerWait = 100 * time.Millisecond
	profile, err := engine.NewProfile(context.Background(), profileOptions{
		Jar: "project:P", DownloadDir: filepath.Join(t.TempDir(), "downloads"), Ephemeral: true,
		Allow: jars.profile("project:P").allow,
	})
	if err != nil {
		t.Fatalf("new profile: %v", err)
	}
	if _, err := profile.(*headlessProfile).ensureBrowser(); err != nil {
		t.Fatalf("launch: %v", err)
	}

	type pattern struct {
		URLPattern   string `json:"urlPattern"`
		ResourceType string `json:"resourceType"`
		RequestStage string `json:"requestStage"`
	}
	var want []pattern
	for _, p := range navigationPolicyPatterns {
		want = append(want, pattern{p.URLPattern, string(p.ResourceType), string(p.RequestStage)})
	}
	enabled := browserCallParams[struct{ Patterns []pattern }](t, browser.calls(), "Fetch.enable")
	if len(enabled) != 1 || !slices.Equal(enabled[0].Patterns, want) {
		t.Fatalf("Fetch.enable on the browser session: %+v, want once with %+v", enabled, want)
	}

	// expect sends each request and waits until every one is answered, so a
	// later event cannot be handled before them.
	expect := func(step string, requests map[string][3]string) {
		t.Helper()
		for id, r := range requests {
			browser.send(pausedRequest(id, r[0], r[1]))
		}
		answers := map[string]bool{}
		eventually(t, step+": the paused requests to be answered", func() bool {
			calls := browser.calls()
			for _, answer := range browserCallParams[requestAnswer](t, calls, "Fetch.continueRequest") {
				answers[answer.RequestID] = true
			}
			for _, answer := range browserCallParams[requestAnswer](t, calls, "Fetch.failRequest") {
				answers[answer.RequestID] = false
			}
			for id := range requests {
				if _, ok := answers[id]; !ok {
					return false
				}
			}
			return true
		})
		for id, r := range requests {
			if got := answers[id]; got != (r[2] == "allow") {
				t.Errorf("%s: frame %s requesting %s was allowed=%v, want %s", step, r[0], r[1], got, r[2])
			}
		}
	}

	expect("before the popup is reported", map[string][3]string{
		"R-early":         {"P-popup", files["one"], "refuse"},
		"R-early-web":     {"P-popup", "https://example.test/", "allow"},
		"R-unknown-frame": {"F-unknown", files["one"], "refuse"},
		"R-frame":         {"F-of-A", files["one"], "allow"},
		"R-frame-other":   {"F-of-A", files["two"], "refuse"},
	})

	// A request paused before its popup's creation is handled waits for it.
	browser.send(pausedRequest("R-waits", "P-popup", files["one"]))
	time.Sleep(20 * time.Millisecond)
	browser.send(popupCreated("P-popup", openerA))
	browser.send(popupCreated("P-nested", "P-popup"))
	expect("a request paused before its popup was known", map[string][3]string{})
	eventually(t, "the waiting request to be allowed", func() bool {
		for _, answer := range browserCallParams[requestAnswer](t, browser.calls(), "Fetch.continueRequest") {
			if answer.RequestID == "R-waits" {
				return true
			}
		}
		return false
	})
	eventually(t, "the popups to be known", func() bool {
		_, popup := engine.profileForPage("P-popup")
		_, nested := engine.profileForPage("P-nested")
		return popup && nested
	})
	expect("after the popups are reported", map[string][3]string{
		"R-popup":       {"P-popup", files["one"], "allow"},
		"R-popup-other": {"P-popup", files["two"], "refuse"},
		"R-nested":      {"P-nested", files["one"], "allow"},
	})

	if err := manager.ClosePage(t.Context(), a, pageA.ID); err != nil {
		t.Fatal(err)
	}
	expect("after the opener closed", map[string][3]string{
		"R-orphan":       {"P-popup", files["one"], "refuse"},
		"R-orphan-other": {"P-popup", files["two"], "refuse"},
	})
}

// A profile answers every request it pauses, so one without a policy to ask
// is refused rather than created.
func TestHeadlessProfileRequiresANavigationPolicy(t *testing.T) {
	engine := newTestHeadlessEngine(t, writeFakeChromium(t, "chromium").path)
	if _, err := engine.NewProfile(context.Background(), profileOptions{
		Jar: "/home/dev/repo", DownloadDir: filepath.Join(t.TempDir(), "downloads"), Ephemeral: true,
	}); err == nil {
		t.Fatal("a profile with no navigation policy was created")
	}
}

// downloadWillBegin is the event Chromium sends as a download starts in
// frame.
func downloadWillBegin(guid, frame string) map[string]any {
	return map[string]any{"method": "Browser.downloadWillBegin", "params": map[string]any{
		"frameId": frame, "guid": guid, "url": "https://example.test/file.bin", "suggestedFilename": "file.bin",
	}}
}

// A download the Manager refuses outright, over quota, is cancelled by the
// engine at once, with no wait for a popup to own it, and the file Chromium
// finished for it before the cancel goes; one it keeps is left running.
func TestHeadlessProfileCancelsADownloadTheManagerRefuses(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	var mu sync.Mutex
	var started []string
	engine.events.DownloadStarted = func(start downloadStart) downloadAdmission {
		mu.Lock()
		defer mu.Unlock()
		started = append(started, start.ID)
		if start.ID == "G-kept" {
			return downloadAdmitted
		}
		return downloadRefused
	}
	// An unowned start would wait this long for a popup to own it.
	engine.ownerWait = time.Hour
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)
	if _, err := profile.ensureBrowser(); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if err := os.MkdirAll(profile.downloadDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// What Chromium wrote for a download that finished before its cancel.
	finished := filepath.Join(profile.downloadDir, "G-refused")
	if err := os.WriteFile(finished, []byte("whole"), 0o600); err != nil {
		t.Fatal(err)
	}

	browser.send(downloadWillBegin("G-refused", "F-1"))
	browser.send(downloadWillBegin("G-kept", "F-1"))
	type cancel struct {
		GUID string `json:"guid"`
	}
	eventually(t, "the refused download to be cancelled", func() bool {
		return len(browserCallParams[cancel](t, browser.calls(), "Browser.cancelDownload")) > 0
	})
	eventually(t, "both downloads to be reported", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(started) == 2
	})
	if got := browserCallParams[cancel](t, browser.calls(), "Browser.cancelDownload"); len(got) != 1 || got[0].GUID != "G-refused" {
		t.Fatalf("cancelled %+v, want only G-refused", got)
	}
	eventually(t, "the refused download's file to go", func() bool {
		_, err := os.Stat(finished)
		return errors.Is(err, fs.ErrNotExist)
	})
}

// A discarded popup's close and a download's cancel end when a connected
// Chromium stops answering them: the discard within its caller's deadline,
// the cancel within the close timeout. Meanwhile another page opens, the
// target the discard could not close stays bound to its profile, and a
// later discard, answered, closes it.
func TestHeadlessProfileBoundsItsBrowserCalls(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	engine.closeTimeout = 50 * time.Millisecond
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)
	if _, err := profile.ensureBrowser(); err != nil {
		t.Fatalf("launch: %v", err)
	}
	engine.bindPage("P-1", profile)
	browser.stallBrowserCalls.Store(true)
	discarded := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		discarded <- engine.DiscardPage(ctx, "P-1")
	}()
	opened, err := profile.NewPage(t.Context(), testPageHooks())
	if err != nil {
		t.Fatalf("another page while the discard is unanswered: %v", err)
	}
	opened.Close()
	select {
	case err := <-discarded:
		if err == nil {
			t.Fatal("a discard Chromium never answered succeeded")
		}
	case <-time.After(headlessTestDeadline):
		t.Fatal("a discard Chromium never answers did not end")
	}
	if owner, bound := engine.profileForPage("P-1"); !bound || owner != profile {
		t.Fatal("the target the discard could not close has no owner")
	}
	canceled := make(chan error, 1)
	go func() { canceled <- profile.CancelDownload("G-1") }()
	select {
	case err := <-canceled:
		if err == nil {
			t.Fatal("a cancel Chromium never answered succeeded")
		}
	case <-time.After(headlessTestDeadline):
		t.Fatal("a cancel Chromium never answers did not end")
	}
	browser.stallBrowserCalls.Store(false)
	if err := engine.DiscardPage(t.Context(), "P-1"); err != nil {
		t.Fatalf("a discard again, answered: %v", err)
	}
	if _, bound := engine.profileForPage("P-1"); bound {
		t.Fatal("the discarded page is still bound")
	}
}

// A download in a popup's main frame names the popup, its openers as the
// browser reported them, and its profile, so the Manager lists it whatever
// becomes of the popup; one in any other frame names no popup.
func TestHeadlessProfileNamesThePopupADownloadBeganIn(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	starts := make(chan downloadStart, 4)
	engine.events.DownloadStarted = func(start downloadStart) downloadAdmission {
		starts <- start
		return downloadAdmitted
	}
	engine.events.PopupOpened = func(enginePopup) {}
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)
	if _, err := profile.ensureBrowser(); err != nil {
		t.Fatalf("launch: %v", err)
	}
	browser.send(popupCreated("P-1", "T-1"))
	browser.send(popupCreated("P-2", "P-1"))
	eventually(t, "the popups to be recorded", func() bool {
		_, first := engine.profileForPage("P-1")
		_, second := engine.profileForPage("P-2")
		return first && second
	})
	browser.send(downloadWillBegin("G-popup", "P-2"))
	browser.send(downloadWillBegin("G-page", "F-1"))
	for range 2 {
		var start downloadStart
		select {
		case start = <-starts:
		case <-time.After(headlessTestDeadline):
			t.Fatal("a download was never reported")
		}
		switch start.ID {
		case "G-popup":
			if start.Frame != "P-2" || start.Popup != "P-2" || !slices.Equal(start.Openers, []string{"P-1", "T-1"}) || start.Profile != profile.Handle() {
				t.Errorf("the popup's download started as %+v", start)
			}
		case "G-page":
			if start.Popup != "" || start.Openers != nil || start.Profile != "" {
				t.Errorf("the page's download started as %+v", start)
			}
		}
	}
}

// A download whose start Chromium sends after its popup's destroy was
// handled still names the popup, its openers and its profile: the event
// subscriptions run in no fixed order, and the Manager lists it by the
// popup's fate.
func TestHeadlessProfileNamesAGonePopupADownloadBeganIn(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	starts := make(chan downloadStart, 1)
	engine.events.DownloadStarted = func(start downloadStart) downloadAdmission {
		starts <- start
		return downloadAdmitted
	}
	engine.events.PopupOpened = func(enginePopup) {}
	engine.events.PageClosed = func(string) {}
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)
	if _, err := profile.ensureBrowser(); err != nil {
		t.Fatalf("launch: %v", err)
	}
	browser.send(popupCreated("P-1", "T-1"))
	browser.send(popupCreated("P-2", "P-1"))
	eventually(t, "the popups to be recorded", func() bool {
		_, first := engine.profileForPage("P-1")
		_, second := engine.profileForPage("P-2")
		return first && second
	})
	browser.send(map[string]any{"method": "Target.targetDestroyed", "params": map[string]any{"targetId": "P-1"}})
	browser.send(map[string]any{"method": "Target.targetDestroyed", "params": map[string]any{"targetId": "P-2"}})
	eventually(t, "the popups' destroys to be handled", func() bool {
		_, first := engine.profileForPage("P-1")
		_, second := engine.profileForPage("P-2")
		return !first && !second
	})
	browser.send(downloadWillBegin("G-gone", "P-2"))
	select {
	case start := <-starts:
		if start.Popup != "P-2" || !slices.Equal(start.Openers, []string{"P-1", "T-1"}) || start.Profile != profile.Handle() {
			t.Fatalf("the gone popup's download started as %+v", start)
		}
	case <-time.After(headlessTestDeadline):
		t.Fatal("the download was never reported")
	}
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
	if !page.OwnsFrame(page.Handle()) {
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

func ptrValue(v *int64) any {
	if v == nil {
		return "absent"
	}
	return *v
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

// A browser whose connection is lost is gone: the browser context is
// cancelled, and the profile's watcher disposes the profile, which kills and
// reaps the process and removes its ephemeral site data.
func TestHeadlessProfileEndsWhenItsConnectionIsLost(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)
	if _, err := profile.ensureBrowser(); err != nil {
		t.Fatalf("launch: %v", err)
	}
	pid := browser.pid(t)

	browser.hangUp()
	eventually(t, "the profile to report its browser gone", func() bool {
		_, ok := profile.browser()
		return !ok
	})
	eventually(t, "the process to be killed and reaped", func() bool { return !alive(pid) })
	eventually(t, "the ephemeral site data to be removed", func() bool {
		_, err := os.Stat(profile.ephemeralRoot)
		return os.IsNotExist(err)
	})
	if len(engine.liveProfiles()) != 0 {
		t.Fatal("the engine still holds a profile whose browser is gone")
	}
}

// lostPages records the pages a headless engine reports lost with their
// browser (PageCrashed).
type lostPages struct {
	mu      sync.Mutex
	handles []string
}

func recordLostPages(engine *headlessEngine) *lostPages {
	closed := &lostPages{}
	engine.events.PageCrashed = func(handle, _ string) { closed.add(handle) }
	engine.events.PageClosed = func(handle string) { panic("a lost browser's page was reported closed, not crashed: " + handle) }
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

// A lost connection cancels the browser context, and the watcher and a
// relaunch both wake on it, in either order. When the relaunch runs first,
// it reports the lost browser's pages closed and reaps its process, and the
// watcher, finding the browser already replaced, reports nothing again. The
// watcher is run by hand after the relaunch here, the order a scheduler can
// pick.
func TestHeadlessProfileRelaunchReportsThePagesOfTheBrowserItLost(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	closed := recordLostPages(engine)
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)

	// Published as adopt publishes a browser, without starting its watcher.
	lost, err := profile.launch(testLaunchContext(t, headlessTestDeadline))
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	profile.mu.Lock()
	profile.current = lost
	profile.mu.Unlock()
	lostPID := browser.pid(t)
	page, err := profile.NewPage(context.Background(), testPageHooks())
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	handle := page.Handle()

	browser.hangUp()
	select {
	case <-lost.ctx.Done():
	case <-time.After(headlessTestDeadline):
		t.Fatal("losing the connection never cancelled the browser context")
	}
	if _, err := profile.ensureBrowser(); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if alive(lostPID) {
		t.Fatalf("the lost Chromium %d is still running or unreaped after the relaunch", lostPID)
	}
	eventually(t, "the lost browser's page to be reported lost", func() bool { return closed.count(handle) > 0 })

	profile.watchBrowser(lost.ctx)
	if n := closed.count(handle); n != 1 {
		t.Fatalf("page %s was reported lost %d times, want once", handle, n)
	}
	if _, ok := profile.browser(); !ok {
		t.Fatal("the lost browser's watcher ended the relaunched browser")
	}
	if pid := browser.pid(t); pid == lostPID || !alive(pid) {
		t.Fatalf("the relaunched Chromium %d is not running", pid)
	}
}

// When the watcher runs first, it takes the lost browser's pages and ends
// the profile in one step, and reports them once the profile is torn down.
// A page the Manager creates while the report is delivered, which is when a
// relaunch would host it, is refused rather than created on a profile the
// watcher is ending, where nothing would ever report it closed.
func TestHeadlessProfileRefusesAPageOnceItsBrowserIsLost(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)

	// Installed before the first page starts the watcher that calls it.
	type created struct {
		handle string
		err    error
	}
	closed := &lostPages{}
	during := make(chan created, 1)
	var lostMu sync.Mutex
	var handle string
	var lostPID int
	var reportedBeforeTheReap, reportedBeforeTheRemoval atomic.Bool
	engine.events.PageCrashed = func(closedHandle, _ string) {
		closed.add(closedHandle)
		lostMu.Lock()
		lost, pid := handle, lostPID
		lostMu.Unlock()
		if closedHandle != lost {
			return
		}
		reportedBeforeTheReap.Store(alive(pid))
		_, err := os.Stat(profile.ephemeralRoot)
		reportedBeforeTheRemoval.Store(err == nil)
		next, err := profile.NewPage(context.Background(), testPageHooks())
		if err != nil {
			during <- created{err: err}
			return
		}
		during <- created{handle: next.Handle()}
	}

	page, err := profile.NewPage(context.Background(), testPageHooks())
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	launchedPID := browser.pid(t)
	lostMu.Lock()
	handle, lostPID = page.Handle(), launchedPID
	lostMu.Unlock()

	browser.hangUp()
	var next created
	select {
	case next = <-during:
	case <-time.After(headlessTestDeadline):
		t.Fatal("the lost browser's page was never reported lost")
	}
	if next.err == nil {
		// A page that exists must be reported lost when its profile ends.
		eventually(t, "the page created during the report to be reported lost", func() bool {
			return closed.count(next.handle) > 0
		})
		t.Fatalf("a page was created on a profile whose browser was lost: %s", next.handle)
	}
	eventually(t, "the profile to end", func() bool {
		profile.mu.Lock()
		defer profile.mu.Unlock()
		return profile.disposed
	})
	if n := closed.count(handle); n != 1 {
		t.Fatalf("page %s was reported lost %d times, want once", handle, n)
	}
	if reportedBeforeTheReap.Load() || reportedBeforeTheRemoval.Load() {
		t.Fatal("the page was reported lost before the profile was torn down; a Dispose the report triggers would return early")
	}
	if pid := browser.pid(t); pid != lostPID {
		t.Fatalf("a Chromium %d was launched for a profile whose browser was lost", pid)
	}
}

// The same race under real scheduling: the relaunch starts the moment the
// loss is visible, which is when the watcher wakes too. Whichever runs
// first, each page of the lost browser is reported lost exactly once.
func TestHeadlessProfileReportsEveryPageOfALostBrowserOnce(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	closed := recordLostPages(engine)

	var lostPages []string
	outcomes := make(map[string]int)
	for i := range 20 {
		profile := testHeadlessProfile(t, engine, fmt.Sprintf("/home/dev/repo-%d", i), false)
		page, err := profile.NewPage(context.Background(), testPageHooks())
		if err != nil {
			t.Fatalf("new page: %v", err)
		}
		lostPages = append(lostPages, page.Handle())

		browser.hangUp()
		deadline := time.Now().Add(headlessTestDeadline)
		for {
			if _, ok := profile.browser(); !ok {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("losing the connection never reached the profile")
			}
			runtime.Gosched()
		}
		if _, err := profile.NewPage(context.Background(), testPageHooks()); err == nil {
			outcomes["relaunch first"]++
		} else {
			outcomes["watcher first"]++
		}
		eventually(t, "page "+page.Handle()+" to be reported lost", func() bool {
			return closed.count(page.Handle()) > 0
		})
		if err := profile.Dispose(context.Background()); err != nil {
			t.Fatalf("dispose: %v", err)
		}
	}
	// A second report of any page would come from a watcher running late.
	time.Sleep(50 * time.Millisecond)
	for _, handle := range lostPages {
		if n := closed.count(handle); n != 1 {
			t.Errorf("page %s was reported lost %d times, want once", handle, n)
		}
	}
	t.Logf("outcomes: %v", outcomes)
}

// Dispose during a launch cannot reach the process through the profile's
// browser, which does not exist yet. It still returns only once that
// process is reaped, because the ephemeral directory is removed next.
func TestHeadlessProfileDisposeReapsALaunchInFlight(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "chromium")
	pidFile := filepath.Join(dir, "pid")
	script := "#!/bin/sh\n" +
		"echo $$ > " + shellQuote(pidFile) + "\n" +
		"exec sleep 300\n"
	mockexec.Write(t, binary, script)
	engine := newTestHeadlessEngine(t, binary)
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)
	launched := make(chan error, 1)
	go func() {
		_, err := profile.ensureBrowser()
		launched <- err
	}()
	pid := waitForPID(t, pidFile)

	if err := profile.Dispose(context.Background()); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	if alive(pid) {
		t.Fatalf("Dispose returned while the Chromium it was launching, %d, was still running", pid)
	}
	if _, err := os.Stat(profile.ephemeralRoot); !os.IsNotExist(err) {
		t.Fatalf("the ephemeral site data survived Dispose: %v", err)
	}
	select {
	case err := <-launched:
		if err == nil {
			t.Fatal("a launch cancelled by Dispose reported a browser")
		}
	case <-time.After(headlessTestDeadline):
		t.Fatal("the cancelled launch never returned")
	}
}

// Selection is a POSITIVE option. The windowless rule
// (TestSelectEngineWithoutAWindowHasNoEngine) and this one are the same
// rule from both sides: no window never means a browser, and a serve boot
// that asked for one and has none gets no engine rather than a broken one.
func TestSelectEngineTakesTheHeadlessEngineOnlyWhenAsked(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := selectEngine(t.TempDir(), ManagerOptions{
		HeadlessChromium: &HeadlessChromiumOptions{Binary: browser.path, Reaper: procutiltest.Reaper()},
	}, engineEvents{})
	if _, ok := engine.(*headlessEngine); !ok {
		t.Fatalf("an asked-for headless engine selected %T", engine)
	}

	// Without a reaper to end its browsers with the backend there is no
	// engine.
	unreaped := selectEngine(t.TempDir(), ManagerOptions{
		HeadlessChromium: &HeadlessChromiumOptions{Binary: browser.path},
	}, engineEvents{})
	if _, ok := unreaped.(unavailableEngine); !ok {
		t.Fatalf("a headless engine with no reaper selected %T, want no engine", unreaped)
	}

	missing := filepath.Join(t.TempDir(), "chromium")
	none := selectEngine(t.TempDir(), ManagerOptions{
		HeadlessChromium: &HeadlessChromiumOptions{Binary: missing, Reaper: procutiltest.Reaper()},
	}, engineEvents{})
	if _, ok := none.(unavailableEngine); !ok {
		t.Fatalf("a serve host with no browser selected %T, want no engine", none)
	}
}

// ---------------------------------------------------------------------
// The manual real-browser gate
// ---------------------------------------------------------------------

// TestHeadlessChromiumReal is the ONLY test that starts a real browser, and
// it runs only when asked: `AO_HEADLESS_CHROMIUM_SMOKE=1 go test
// ./internal/browser -run TestHeadlessChromiumReal -count=1`. It is
// documented beside `make provider-smoke` in
// docs/architecture/development.md and is on no automatic target.
//
// What it proves that nothing above can: that this machine's Chromium
// accepts the command line chromiumArgs builds, sandbox and all, with only
// the mock keychain added on macOS, and hands out a target the shared CDP
// driver can attach to and navigate; that Dispose leaves no process of it running and an ephemeral
// profile nowhere on disk; that a persisted profile keeps a cookie set
// just before Dispose; that a popup's first request passes the
// workspace's navigation policy; and that the crash handler writes
// nothing into HOME. Run it after a Chromium major upgrade and before
// shipping a change to the launch flags or to how a browser is stopped.
func TestHeadlessChromiumReal(t *testing.T) {
	if os.Getenv("AO_HEADLESS_CHROMIUM_SMOKE") != "1" {
		t.Skip("set AO_HEADLESS_CHROMIUM_SMOKE=1 to launch this machine's real Chromium")
	}
	// Chromium inherits this HOME, so nothing the smoke starts writes into
	// the developer's own.
	home := t.TempDir()
	t.Setenv("HOME", home)
	engine, err := newHeadlessChromiumEngine(t.TempDir(), HeadlessChromiumOptions{Reaper: procutiltest.Reaper()}, engineEvents{
		PopupOpened:      func(enginePopup) {},
		PageClosed:       func(string) {},
		PageCrashed:      func(string, string) {},
		PageInfoChanged:  func(string, string, string, uint64) {},
		DownloadStarted:  func(downloadStart) downloadAdmission { return downloadAdmitted },
		DownloadProgress: func(downloadProgress) {},
	})
	if err != nil {
		t.Fatalf("find a Chromium: %v", err)
	}
	t.Logf("launching %s", engine.binary)
	if runtime.GOOS == "darwin" {
		// macOS finds the keychain through HOME, and this one has none:
		// Chrome would ask for one in a dialog at every launch and keep no
		// cookie it cannot encrypt. The mock keychain keeps the smoke off
		// the keychain; a serve backend runs with the user's own HOME.
		wrapper := filepath.Join(t.TempDir(), "chromium")
		script := "#!/bin/sh\nexec '" + strings.ReplaceAll(engine.binary, "'", `'\''`) + "' --use-mock-keychain \"$@\"\n"
		if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		engine.binary = wrapper
	}
	engine.logf = func(format string, args ...any) { t.Logf(format, args...) }
	if err := engine.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer engine.Stop()

	profile := testHeadlessProfile(t, engine, t.TempDir(), false)
	page, err := profile.NewPage(t.Context(), testPageHooks())
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	// Operations run on a context derived from the page's own lifetime,
	// which is the Manager's contract (operationContext): a bare caller
	// context carries no chromedp target and chromedp refuses it.
	opCtx, cancel := operationContext(t.Context(), page.Lifetime(), operationTimeout)
	defer cancel()
	if err := page.Navigate(opCtx, "about:blank"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	url, _, err := page.Info(opCtx)
	if err != nil {
		t.Fatalf("read page state: %v", err)
	}
	if url != "about:blank" {
		t.Fatalf("page URL = %q, want about:blank", url)
	}
	group := profile.currentProcess().cmd.Process.Pid
	page.Close()
	if err := profile.Dispose(t.Context()); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	// With no process of Chromium's left, nothing can recreate the
	// directory, so its absence now is final.
	if left := chromiumLeftovers(t, group, profile.ephemeralRoot); len(left) > 0 {
		t.Fatalf("Chromium processes outlived Dispose: %v", left)
	}
	if _, err := os.Stat(profile.ephemeralRoot); !os.IsNotExist(err) {
		t.Fatalf("the ephemeral profile %s survives Dispose: %v", profile.ephemeralRoot, err)
	}

	workspace := t.TempDir()
	cookie := setRealCookie(t, testHeadlessProfile(t, engine, workspace, true))
	if got := readRealCookie(t, testHeadlessProfile(t, engine, workspace, true), cookie.Name); got != cookie.Value {
		t.Fatalf("the cookie set before Dispose reads %q after it, want %q", got, cookie.Value)
	}

	assertRealPopupsPassThePolicy(t, engine)
	assertRealDocumentTitles(t, engine)
	assertRealDocumentTitleChanges(t, engine)
	assertRealEvaluateSemantics(t, engine)
	assertRealPageOperations(t, engine)
	if err := filepath.WalkDir(home, func(path string, _ os.DirEntry, err error) error {
		if err == nil && filepath.Base(path) == "Crash Reports" {
			t.Errorf("the crash handler wrote %s outside the profile", path)
		}
		return err
	}); err != nil {
		t.Fatalf("read HOME: %v", err)
	}
}

// assertRealEvaluateSemantics holds the CDP driver to the evaluate contract
// on a page whose script policy forbids eval, as AO's own UI does.
func assertRealEvaluateSemantics(t *testing.T, engine *headlessEngine) {
	t.Helper()
	workspace := t.TempDir()
	index := filepath.Join(workspace, "evaluate.html")
	if err := os.WriteFile(index, []byte(evaluateSemanticsPage), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := testHeadlessProfile(t, engine, workspace, false)
	defer func() {
		if err := profile.Dispose(context.Background()); err != nil {
			t.Errorf("dispose: %v", err)
		}
	}()
	page, err := profile.NewPage(t.Context(), testPageHooks())
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	opCtx, cancel := operationContext(t.Context(), page.Lifetime(), operationTimeout)
	defer cancel()
	if err := page.Navigate(opCtx, "file://"+index); err != nil {
		t.Fatalf("navigate to the evaluate page: %v", err)
	}
	assertEvaluateSemantics(t, evaluateEngineCDP, driverEvaluator(opCtx, page, false), driverEvaluator(opCtx, page, true))
}

// assertRealDocumentTitles proves the title watcher on a real Chromium: the
// parsed <title> and a later assignment by script both reach DocumentTitle,
// which no Target.targetInfoChanged reports, and a page script cannot see
// the binding.
func assertRealDocumentTitles(t *testing.T, engine *headlessEngine) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/frame" {
			_, _ = w.Write([]byte(`<!doctype html><title>Frame</title>`))
			return
		}
		_, _ = w.Write([]byte(`<!doctype html><title>Parsed</title><iframe src="/frame"></iframe><script>
setTimeout(() => { document.title = typeof __aoDocumentTitle + " by script" }, 300)
</script>`))
	}))
	defer server.Close()
	created, err := engine.NewProfile(t.Context(), profileOptions{
		Jar: t.TempDir(), DownloadDir: filepath.Join(t.TempDir(), "downloads"), Ephemeral: true, Allow: func([]string, string) (bool, bool) { return true, true },
	})
	if err != nil {
		t.Fatalf("new profile: %v", err)
	}
	profile := created.(*headlessProfile)
	defer func() {
		if err := profile.Dispose(context.Background()); err != nil {
			t.Errorf("dispose: %v", err)
		}
	}()
	titles := make(chan string, 16)
	hooks := testPageHooks()
	framed := make(chan string, 16)
	hooks.DocumentTitle = func(url, title string) {
		switch url {
		case server.URL + "/":
			titles <- title
		case "about:blank":
			// The tab's own first document, before the navigation.
		default:
			framed <- url
		}
	}
	page, err := profile.NewPage(t.Context(), hooks)
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	defer page.Close()
	opCtx, cancel := operationContext(t.Context(), page.Lifetime(), operationTimeout)
	defer cancel()
	if err := page.Navigate(opCtx, server.URL+"/"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	for _, want := range []string{"Parsed", "undefined by script"} {
		select {
		case got := <-titles:
			if got != want {
				t.Fatalf("the document reported %q, want %q", got, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the document never reported %q", want)
		}
	}
	// The frame loaded before the script's title change; only the tab's own
	// document reports.
	select {
	case url := <-framed:
		t.Fatalf("a document that is not the tab's reported its title: %s", url)
	default:
	}
}

// assertRealPopupsPassThePolicy opens an outside-workspace file in a popup
// from a workspace page, as a page script can, with and without an opener.
// Each popup runs before AttachPage installs its own policy, so what keeps
// the file out of the adopted page is the workspace's policy.
func assertRealPopupsPassThePolicy(t *testing.T, engine *headlessEngine) {
	t.Helper()
	workspace, outside := t.TempDir(), t.TempDir()
	index := filepath.Join(workspace, "index.html")
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(index, []byte("<html><body>workspace</body></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("outside-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	inWorkspace := func(url string) bool {
		return !strings.HasPrefix(url, "file:") || strings.HasPrefix(url, "file://"+workspace+"/")
	}
	popups := make(chan enginePopup, 2)
	engine.events.PopupOpened = func(popup enginePopup) { popups <- popup }
	created, err := engine.NewProfile(t.Context(), profileOptions{
		Jar: workspace, DownloadDir: filepath.Join(t.TempDir(), "downloads"), Ephemeral: true, Allow: func(_ []string, url string) (bool, bool) { return inWorkspace(url), true },
	})
	if err != nil {
		t.Fatalf("new profile: %v", err)
	}
	profile := created.(*headlessProfile)
	defer func() {
		if err := profile.Dispose(context.Background()); err != nil {
			t.Errorf("dispose: %v", err)
		}
	}()
	hooks := testPageHooks()
	hooks.Allow = inWorkspace
	page, err := profile.NewPage(t.Context(), hooks)
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	opCtx, cancel := operationContext(t.Context(), page.Lifetime(), operationTimeout)
	defer cancel()
	if err := page.Navigate(opCtx, "file://"+index); err != nil {
		t.Fatalf("navigate to the workspace page: %v", err)
	}
	for _, features := range []string{"", "noopener"} {
		open := fmt.Sprintf(`window.open(%q, "_blank", %q); 1`, "file://"+secret, features)
		if _, err := chromedp.Call(opCtx, cdpruntime.Evaluate, cdpruntime.EvaluateParams{Expression: open, UserGesture: new(true)}); err != nil {
			t.Fatalf("open a popup with features %q: %v", features, err)
		}
		var popup enginePopup
		select {
		case popup = <-popups:
		case <-time.After(headlessTestDeadline):
			t.Fatalf("the popup with features %q was never reported", features)
		}
		driver, err := profile.AttachPage(t.Context(), popup.Handle, hooks)
		if err != nil {
			t.Fatalf("adopt the popup with features %q: %v", features, err)
		}
		popupCtx, popupCancel := operationContext(t.Context(), driver.Lifetime(), operationTimeout)
		status, _ := driver.PageStatus(popupCtx)
		for status.Ready != "complete" || status.URL == "about:blank" {
			if popupCtx.Err() != nil {
				t.Fatalf("the popup with features %q never finished loading: %+v", features, status)
			}
			time.Sleep(20 * time.Millisecond)
			status, _ = driver.PageStatus(popupCtx)
		}
		body, _, err := driverEvaluator(popupCtx, driver, false)(`document.body ? document.body.innerText : ""`, nil)
		popupCancel()
		driver.Close()
		if err != nil {
			t.Fatalf("read the popup with features %q: %v", features, err)
		}
		if strings.Contains(fmt.Sprint(body), "outside-secret") {
			t.Fatalf("the popup with features %q loaded the outside file at %s", features, status.URL)
		}
	}
	page.Close()
}

// setRealCookie sets a persistent cookie from a page of profile and
// disposes the profile, checking that its browser exited when asked.
func setRealCookie(t *testing.T, profile *headlessProfile) *network.Cookie {
	t.Helper()
	page, err := profile.NewPage(t.Context(), testPageHooks())
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	cookie := &network.Cookie{Name: "ao-smoke", Value: strconv.FormatInt(time.Now().UnixNano(), 36)}
	expires := cdp.TimeSinceEpoch(time.Now().Add(time.Hour).Unix())
	if _, err := chromedp.Call(page.(*cdpPage).ctx, network.SetCookie, network.SetCookieParams{
		Name: cookie.Name, Value: cookie.Value, Domain: "ao-smoke.test", Path: "/", Expires: expires,
	}); err != nil {
		t.Fatalf("set a cookie: %v", err)
	}
	process := profile.currentProcess()
	page.Close()
	if err := profile.Dispose(t.Context()); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	if got := process.exitStatus(); got != "exit status 0" {
		t.Fatalf("the persisted profile's Chromium ended with %q, want it to exit when asked", got)
	}
	return cookie
}

// readRealCookie reads a cookie back from a fresh browser on profile, then
// disposes it.
func readRealCookie(t *testing.T, profile *headlessProfile, name string) string {
	t.Helper()
	browserCtx, err := profile.ensureBrowser()
	if err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	result, err := chromedp.CallBrowser(browserCtx, storage.GetCookies, storage.GetCookiesParams{})
	cookies := result.Cookies
	if err != nil {
		t.Fatalf("read cookies: %v", err)
	}
	if err := profile.Dispose(t.Context()); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie.Value
		}
	}
	return ""
}

// Headless Chromium keeps one user-data directory per persistent jar under
// the profile tree Clear site data deletes, the worktree jar's where it
// always was, and a private jar's nowhere under it.
func TestHeadlessEngineKeepsOneDirectoryPerJar(t *testing.T) {
	engine := newTestHeadlessEngine(t, writeFakeChromium(t, "chromium").path)
	const workspace = "/home/dev/repo"
	digest := sha256.Sum256([]byte(workspace))
	worktree := testHeadlessProfile(t, engine, workspace, true)
	if want := filepath.Join(engine.configDir, browserProfileDir, fmt.Sprintf("%x", digest[:12]), "chromium"); worktree.userDataDir != want {
		t.Fatalf("worktree jar directory = %q, want today's %q", worktree.userDataDir, want)
	}
	project := testHeadlessProfile(t, engine, "project:P", true)
	if project.userDataDir == worktree.userDataDir || !strings.HasPrefix(project.userDataDir, filepath.Join(engine.configDir, browserProfileDir)) {
		t.Fatalf("project jar directory = %q", project.userDataDir)
	}
	private := testHeadlessProfile(t, engine, "private:a:token", false)
	if strings.HasPrefix(private.userDataDir, filepath.Join(engine.configDir, browserProfileDir)) {
		t.Fatalf("private jar directory %q is under the persistent profile tree", private.userDataDir)
	}
}

// ownFrame makes the page with handle report frame as one of its subframes.
func (e *jarEngine) ownFrame(frame, handle string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.frames == nil {
		e.frames = make(map[string]string)
	}
	e.frames[frame] = handle
}

// A page or popup whose attach Chromium leaves unanswered, its connection
// still open, fails at the attach bound, ends when its creation is
// canceled, and ends at Interrupt, which shutdown calls before it waits.
func TestHeadlessPageAttachThatStallsEnds(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(engine *headlessEngine, cancel context.CancelFunc)
	}{
		{"bound", func(*headlessEngine, context.CancelFunc) {}},
		{"canceled", func(_ *headlessEngine, cancel context.CancelFunc) { cancel() }},
		{"interrupted", func(engine *headlessEngine, _ context.CancelFunc) { engine.Interrupt() }},
	} {
		for _, popup := range []bool{false, true} {
			name := tc.name + "/page"
			if popup {
				name = tc.name + "/popup"
			}
			t.Run(name, func(t *testing.T) {
				browser := writeFakeChromium(t, "chromium")
				engine := newTestHeadlessEngine(t, browser.path)
				engine.attachTimeout = time.Hour
				if tc.name == "bound" {
					engine.attachTimeout = 200 * time.Millisecond
				}
				profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)
				if _, err := profile.ensureBrowser(); err != nil {
					t.Fatalf("launch: %v", err)
				}
				browser.stallFrameTree.Store(true)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				attached := make(chan error, 1)
				go func() {
					var err error
					if popup {
						_, err = profile.AttachPage(ctx, "T-popup", pageHooks{})
					} else {
						_, err = profile.NewPage(ctx, pageHooks{})
					}
					attached <- err
				}()
				awaitCall(t, browser.calls, "Page.getFrameTree")
				tc.end(engine, cancel)
				select {
				case err := <-attached:
					if err == nil {
						t.Fatal("a page whose attach never finished was attached")
					}
				case <-time.After(headlessTestDeadline):
					t.Fatal("the stalled attach never returned")
				}
			})
		}
	}
}
