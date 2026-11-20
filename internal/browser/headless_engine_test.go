//go:build !windows

package browser

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/testutil/mockexec"
)

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

	oneDir := filepath.Join(engine.configDir, browserProfileDir, jarDirName("/home/dev/one"), "chromium")
	twoDir := filepath.Join(engine.configDir, browserProfileDir, jarDirName("/home/dev/two"), "chromium")
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

// A jar's next profile, after the one before was disposed, keeps the jar's
// site data directory and has a handle of its own: a report the disposed
// profile's browser left names no open profile.
func TestAHeadlessProfileOpenedAgainHasItsOwnHandle(t *testing.T) {
	engine := newTestHeadlessEngine(t, writeFakeChromium(t, "chromium").path)
	first := testHeadlessProfile(t, engine, "/home/dev/repo", true)
	if err := first.Dispose(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := testHeadlessProfile(t, engine, "/home/dev/repo", true)
	if first.Handle() == second.Handle() {
		t.Fatalf("both profiles report handle %q", first.Handle())
	}
	if first.userDataDir != second.userDataDir {
		t.Fatalf("the jar's site data moved from %q to %q", first.userDataDir, second.userDataDir)
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
