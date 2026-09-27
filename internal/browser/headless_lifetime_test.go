package browser

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The ends of a headless profile's browser, tested against plain contexts
// rather than a process: a relaunch retiring a browser that was lost, a
// page binding only to the connected browser, and the profile ending when
// its browser context is cancelled under it.
//
// What they turn on is chromedp's contract and not Chromium's: the browser
// context is cancelled when the connection to the process is lost (the
// remote allocator's LostConnection goroutine cancels it), so "the browser
// died" IS "this context was cancelled" and nothing else has to be
// simulated. The same paths against a fake Chromium and a dropped CDP
// connection, including the reap, are in headless_engine_test.go.

// lifetimeProfile is a profile whose engine records what it was told,
// wired to no browser at all.
type lifetimeProfile struct {
	*headlessProfile
	engine *headlessEngine

	mu     sync.Mutex
	closed []string
}

func newLifetimeProfile(t *testing.T) *lifetimeProfile {
	t.Helper()
	lp := &lifetimeProfile{}
	lp.engine = &headlessEngine{
		logf:        func(format string, args ...any) { t.Logf(format, args...) },
		profiles:    make(map[*headlessProfile]struct{}),
		pageProfile: make(map[string]*headlessProfile),
		events: engineEvents{PageClosed: func(handle string) {
			lp.mu.Lock()
			defer lp.mu.Unlock()
			lp.closed = append(lp.closed, handle)
		}},
	}
	lp.headlessProfile = &headlessProfile{engine: lp.engine, handle: "workspace-1"}
	lp.engine.profiles[lp.headlessProfile] = struct{}{}
	return lp
}

func (lp *lifetimeProfile) closedPages() []string {
	lp.mu.Lock()
	defer lp.mu.Unlock()
	out := append([]string(nil), lp.closed...)
	sort.Strings(out)
	return out
}

// eventually polls until cond holds, so a watcher on its own goroutine is
// waited for rather than slept past.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// publish makes a browser the profile's without the watcher adopt starts,
// which leaves the caller to decide when, or whether, that watcher runs.
func (lp *lifetimeProfile) publish(browser *launchedBrowser) {
	lp.headlessProfile.mu.Lock()
	defer lp.headlessProfile.mu.Unlock()
	lp.current = browser
}

// TestARelaunchRetiresOnlyALostBrowser: ensureBrowser relaunches when the
// previous browser context is CANCELLED, which is exactly what a Chromium
// whose connection was lost looks like. The lost browser is owed its close
// (its allocator is cancelled nowhere else) and its pages are owed their
// report, once, even when its watcher runs afterwards. A browser that is
// still connected is never retired.
func TestARelaunchRetiresOnlyALostBrowser(t *testing.T) {
	lp := newLifetimeProfile(t)
	lp.engine.bindPage("page-a", lp.headlessProfile)
	browserCtx, browserCancel := context.WithCancel(context.Background())
	var allocCancelled atomic.Bool
	lp.publish(&launchedBrowser{ctx: browserCtx, cancel: browserCancel, allocCancel: func() { allocCancelled.Store(true) }})

	lp.retireLostBrowser()
	if _, ok := lp.browser(); !ok || allocCancelled.Load() {
		t.Fatal("a connected browser was retired")
	}

	browserCancel()
	lp.retireLostBrowser()
	if !allocCancelled.Load() {
		t.Error("the lost browser's allocator was never cancelled")
	}
	eventually(t, "the lost browser's page to be reported closed", func() bool {
		return len(lp.closedPages()) > 0
	})
	lp.retireLostBrowser()
	lp.watchBrowser(browserCtx)
	if got := lp.closedPages(); !slices.Equal(got, []string{"page-a"}) {
		t.Fatalf("reported %v closed, want page-a once", got)
	}
	lp.headlessProfile.mu.Lock()
	disposed := lp.disposed
	lp.headlessProfile.mu.Unlock()
	if disposed {
		t.Fatal("retiring a lost browser disposed the profile the relaunch is for")
	}
}

// TestAProfileAdoptsOneBrowser: a profile holds at most one browser, so
// adopting a second one is refused and leaves the first in place rather
// than dropping the only reference to a running process.
func TestAProfileAdoptsOneBrowser(t *testing.T) {
	lp := newLifetimeProfile(t)
	firstCtx, firstCancel := context.WithCancel(context.Background())
	defer firstCancel()
	if err := lp.adopt(&launchedBrowser{ctx: firstCtx, cancel: firstCancel, allocCancel: func() {}}); err != nil {
		t.Fatalf("adopt the first browser: %v", err)
	}
	secondCtx, secondCancel := context.WithCancel(context.Background())
	defer secondCancel()
	if err := lp.adopt(&launchedBrowser{ctx: secondCtx, cancel: secondCancel, allocCancel: func() {}}); err == nil {
		t.Fatal("a profile adopted a second browser")
	}
	if got, ok := lp.browser(); !ok || got != firstCtx {
		t.Fatal("a refused adopt replaced the profile's browser")
	}
}

// TestAPageBindsOnlyToTheConnectedBrowser: a page is bound under the lock
// that also takes a lost browser's pages, so a page created on a browser
// that is lost or no longer the profile's is refused rather than bound
// where no report would ever reach it.
func TestAPageBindsOnlyToTheConnectedBrowser(t *testing.T) {
	lp := newLifetimeProfile(t)
	browserCtx, browserCancel := context.WithCancel(context.Background())
	lp.publish(&launchedBrowser{ctx: browserCtx, cancel: browserCancel, allocCancel: func() {}})
	otherCtx, otherCancel := context.WithCancel(context.Background())
	defer otherCancel()

	if err := lp.bindPage(browserCtx, "page-a"); err != nil {
		t.Fatalf("bind a page of the connected browser: %v", err)
	}
	if err := lp.bindPage(otherCtx, "page-other"); err == nil {
		t.Error("a page of a browser that is not the profile's was bound")
	}
	browserCancel()
	if err := lp.bindPage(browserCtx, "page-late"); err == nil {
		t.Error("a page of a lost browser was bound")
	}
	for handle, want := range map[string]bool{"page-a": true, "page-other": false, "page-late": false} {
		if _, bound := lp.engine.profileForPage(handle); bound != want {
			t.Errorf("page %s bound = %v, want %v", handle, bound, want)
		}
	}
}

// TestAProfileEndsWhenItsBrowserDies: a Chromium that crashed, was
// OOM-killed or was killed by hand cancels the browser context and nothing
// else. Without a watcher the pages stayed in the Manager as rows nothing
// could drive: every operation answered "the profile's browser is no
// longer running" and no event ever said the page had gone, and an
// ephemeral profile's cookie jar stayed on disk.
func TestAProfileEndsWhenItsBrowserDies(t *testing.T) {
	lp := newLifetimeProfile(t)
	lp.ephemeralRoot = filepath.Join(t.TempDir(), "ephemeral")
	if err := os.MkdirAll(lp.ephemeralRoot, 0o700); err != nil {
		t.Fatalf("lay out the ephemeral root: %v", err)
	}
	lp.engine.bindPage("page-a", lp.headlessProfile)
	lp.engine.bindPage("page-b", lp.headlessProfile)

	browserCtx, browserCancel := context.WithCancel(context.Background())
	if err := lp.adopt(&launchedBrowser{ctx: browserCtx, cancel: browserCancel, allocCancel: func() {}}); err != nil {
		t.Fatalf("adopt: %v", err)
	}

	browserCancel()

	eventually(t, "both pages to be reported closed", func() bool {
		return len(lp.closedPages()) == 2
	})
	if got := lp.closedPages(); got[0] != "page-a" || got[1] != "page-b" {
		t.Fatalf("reported %v closed, want both bound pages", got)
	}
	eventually(t, "the profile to be disposed", func() bool {
		lp.headlessProfile.mu.Lock()
		defer lp.headlessProfile.mu.Unlock()
		return lp.disposed
	})
	if _, err := os.Stat(lp.ephemeralRoot); !os.IsNotExist(err) {
		t.Fatalf("the ephemeral site data survived the browser dying: %v", err)
	}
	if _, still := lp.engine.profileForPage("page-a"); still {
		t.Fatal("a page of a dead browser is still addressable")
	}
	if len(lp.engine.liveProfiles()) != 0 {
		t.Fatal("the engine still holds a profile whose browser is gone")
	}
}

// TestDisposeLeavesTheWatcherSilent: Dispose cancels the same context the
// watcher is on, and it has already reported and forgotten everything. A
// watcher that reported again would tell the Manager a page closed twice.
func TestDisposeLeavesTheWatcherSilent(t *testing.T) {
	lp := newLifetimeProfile(t)
	lp.engine.bindPage("page-a", lp.headlessProfile)

	browserCtx, browserCancel := context.WithCancel(context.Background())
	if err := lp.adopt(&launchedBrowser{ctx: browserCtx, cancel: browserCancel, allocCancel: func() {}}); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if err := lp.Dispose(context.Background()); err != nil {
		t.Fatalf("dispose: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	if got := lp.closedPages(); len(got) != 0 {
		t.Fatalf("an ordinary Dispose reported %v closed through the watcher", got)
	}
}
