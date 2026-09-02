package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The headless engine is exercised against a FAKE Chromium: a shell script
// that records its argv, prints the "DevTools listening on" line every
// Chromium prints, and then sleeps until it is killed. The endpoint that
// line advertises is the same loopback CDP fake the hosted engine's tests
// use. No test here starts a real browser, and none downloads one — the
// real launch is the manual AO_HEADLESS_CHROMIUM_SMOKE gate at the bottom
// of this file.

const headlessTestDeadline = 10 * time.Second

// fakeChromium is one scripted browser: where it wrote its argv, the pid it
// runs under, and the CDP endpoint it advertised.
type fakeChromium struct {
	path     string
	argvFile string
	pidFile  string
	calls    func() []fakeCDPCall
}

// writeFakeChromium installs the script and the endpoint behind it.
//
// `exec sleep` rather than a shell loop on purpose: the process the
// allocator kills is then the one whose pid the script recorded, so
// "Dispose killed the browser" is provable rather than inferred.
func writeFakeChromium(t *testing.T, name string) *fakeChromium {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake Chromium is a POSIX shell script; the headless engine ships on the serve hosts, which are Linux and macOS")
	}
	dir := t.TempDir()
	browser := &fakeChromium{
		path:     filepath.Join(dir, name),
		argvFile: filepath.Join(dir, "argv"),
		pidFile:  filepath.Join(dir, "pid"),
	}
	var targets, sessions atomic.Int64
	wsURL, calls := newFakeCDPServer(t, func(call fakeCDPCall) any {
		switch call.Method {
		case "Target.createTarget":
			return map[string]any{"targetId": fmt.Sprintf("T-%d", targets.Add(1))}
		case "Target.attachToTarget":
			return map[string]any{"sessionId": fmt.Sprintf("S-%d", sessions.Add(1))}
		case "Runtime.evaluate":
			// chromedp asks the fresh target what `self` is, to find out
			// whether it attached to a worker. A page answers Window.
			return map[string]any{"result": map[string]any{"type": "object", "className": "Window"}}
		case "Page.getFrameTree":
			return map[string]any{"frameTree": map[string]any{"frame": map[string]any{
				"id": "F-1", "loaderId": "L-1", "url": "about:blank",
				"securityOrigin": "://", "mimeType": "text/html",
			}}}
		}
		return nil
	})
	browser.calls = calls
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + shellQuote(browser.argvFile) + "\n" +
		"echo $$ > " + shellQuote(browser.pidFile) + "\n" +
		"echo 'DevTools listening on " + wsURL + "' >&2\n" +
		"exec sleep 300\n"
	if err := os.WriteFile(browser.path, []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake Chromium: %v", err)
	}
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

// alive answers whether a recorded pid is still a live process. A killed
// browser is reaped by the allocator before Dispose returns, so this needs
// no polling.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func newTestHeadlessEngine(t *testing.T, binary string) *headlessEngine {
	t.Helper()
	engine := &headlessEngine{
		configDir:   t.TempDir(),
		binary:      binary,
		events:      engineEvents{},
		logf:        func(format string, args ...any) { t.Logf(format, args...) },
		profiles:    make(map[*headlessProfile]struct{}),
		pageProfile: make(map[string]*headlessProfile),
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
		Workspace: workspace, DownloadDir: filepath.Join(t.TempDir(), "downloads"), Persist: persist,
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

// The argv tripwire. --no-sandbox is the one flag that must never appear:
// it is a whole security boundary, and chromedp adds it by itself when the
// process runs as root unless the flag is already set (allocate.go), so
// this assertion is the thing standing between a root-installed serve unit
// and an unsandboxed browser.
func assertChromiumArgv(t *testing.T, argv []string, wantUserDataDir string) {
	t.Helper()
	for _, arg := range argv {
		if arg == "--no-sandbox" || strings.HasPrefix(arg, "--no-sandbox=") {
			t.Fatalf("the launch disabled the renderer sandbox: %v", argv)
		}
	}
	for _, want := range []string{
		"--headless=new",
		"--user-data-dir=" + wantUserDataDir,
		"--disable-gpu",
		"--no-first-run",
		"--no-default-browser-check",
	} {
		if !hasArg(argv, want) {
			t.Fatalf("argv %v is missing %q", argv, want)
		}
	}
}

func hasArg(argv []string, want string) bool {
	for _, arg := range argv {
		if arg == want {
			return true
		}
	}
	return false
}

// The same rule again, one layer down and without a process: the flag is
// PRESENT and false rather than absent, which is what stops chromedp from
// adding it on a root-run backend. Deleting the line would still pass the
// argv assertion above on every developer machine, and fail only in
// production.
func TestHeadlessChromiumFlagsWithholdTheSandboxFlagExplicitly(t *testing.T) {
	var withheld bool
	for _, flag := range chromiumLaunchFlags("/data/chromium") {
		if flag.name != "no-sandbox" {
			continue
		}
		enabled, ok := flag.value.(bool)
		if !ok || enabled {
			t.Fatalf("no-sandbox is set to %#v; it must be present and false", flag.value)
		}
		withheld = true
	}
	if !withheld {
		t.Fatal("no-sandbox is absent from the flag set; chromedp then adds it by itself for a root-run backend")
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
// disk and Chromium has no in-memory profile to keep it with.
func TestHeadlessProfileDisposeKillsTheBrowserAndHonoursPersistence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		persist    bool
		wantOnDisk bool
	}{
		{name: "an ephemeral profile leaves nothing behind", persist: false},
		{name: "a persisted profile keeps its logins", persist: true, wantOnDisk: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			browser := writeFakeChromium(t, "chromium")
			engine := newTestHeadlessEngine(t, browser.path)
			profile := testHeadlessProfile(t, engine, "/home/dev/repo", tc.persist)
			if _, err := profile.ensureBrowser(); err != nil {
				t.Fatalf("launch: %v", err)
			}
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
func TestHeadlessProfileSurfacesWhyTheBrowserRefusedToStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake Chromium is a POSIX shell script")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "chromium")
	script := "#!/bin/sh\n" +
		"echo 'Failed to move to new namespace: Operation not permitted' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
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
	if elapsed := time.Since(started); elapsed > headlessTestDeadline {
		t.Fatalf("the launch took %s: the wait is not bounded", elapsed)
	}
}

// Selection is a POSITIVE option. The windowless rule
// (TestSelectEngineWithoutAWindowHasNoEngine) and this one are the same
// rule from both sides: no window never means a browser, and a serve boot
// that asked for one and has none gets no engine rather than a broken one.
func TestSelectEngineTakesTheHeadlessEngineOnlyWhenAsked(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := selectEngine(t.TempDir(), ManagerOptions{
		HeadlessChromium: &HeadlessChromiumOptions{Binary: browser.path},
	}, engineEvents{})
	if _, ok := engine.(*headlessEngine); !ok {
		t.Fatalf("an asked-for headless engine selected %T", engine)
	}

	missing := filepath.Join(t.TempDir(), "chromium")
	none := selectEngine(t.TempDir(), ManagerOptions{
		HeadlessChromium: &HeadlessChromiumOptions{Binary: missing},
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
// documented beside `make provider-smoke` in the root CLAUDE.md and is on
// no automatic target.
//
// What it proves that nothing above can: that this machine's Chromium
// accepts the exact command line chromiumLaunchFlags builds — sandbox and
// all — and hands out a target the shared CDP driver can attach to and
// navigate. Run it after a Chromium major upgrade and before shipping a
// change to the launch flags.
func TestHeadlessChromiumReal(t *testing.T) {
	if os.Getenv("AO_HEADLESS_CHROMIUM_SMOKE") != "1" {
		t.Skip("set AO_HEADLESS_CHROMIUM_SMOKE=1 to launch this machine's real Chromium")
	}
	engine, err := newHeadlessChromiumEngine(t.TempDir(), HeadlessChromiumOptions{}, engineEvents{
		PopupOpened:      func(enginePopup) {},
		PageClosed:       func(string) {},
		PageInfoChanged:  func(string, string, string) {},
		DownloadStarted:  func(downloadStart) {},
		DownloadProgress: func(downloadProgress) {},
	})
	if err != nil {
		t.Fatalf("find a Chromium: %v", err)
	}
	engine.logf = func(format string, args ...any) { t.Logf(format, args...) }
	if err := engine.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer engine.Stop()
	t.Logf("launching %s", engine.binary)

	profile := testHeadlessProfile(t, engine, t.TempDir(), false)
	page, err := profile.NewPage(t.Context(), testPageHooks())
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	if err := page.Navigate(t.Context(), "about:blank"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	url, _, err := page.Info(t.Context())
	if err != nil {
		t.Fatalf("read page state: %v", err)
	}
	if url != "about:blank" {
		t.Fatalf("page URL = %q, want about:blank", url)
	}
	page.Close()
	if err := profile.Dispose(t.Context()); err != nil {
		t.Fatalf("dispose: %v", err)
	}
}
