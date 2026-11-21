//go:build !windows

package browser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/testutil/mockexec"
)

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
	eventually(t, "the lost browser's page to be reported lost", func() bool { return closed.count(string(handle)) > 0 })

	profile.watchBrowser(lost.ctx)
	if n := closed.count(string(handle)); n != 1 {
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
	engine.events.PageCrashed = func(closedHandle ids.PageHandle, _ string) {
		closed.add(string(closedHandle))
		lostMu.Lock()
		lost, pid := handle, lostPID
		lostMu.Unlock()
		if closedHandle != ids.PageHandle(lost) {
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
		during <- created{handle: string(next.Handle())}
	}

	page, err := profile.NewPage(context.Background(), testPageHooks())
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	launchedPID := browser.pid(t)
	lostMu.Lock()
	handle, lostPID = string(page.Handle()), launchedPID
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
		lostPages = append(lostPages, string(page.Handle()))

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
		eventually(t, "page "+string(page.Handle())+" to be reported lost", func() bool {
			return closed.count(string(page.Handle())) > 0
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
