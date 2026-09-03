package browser

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

// The two ends of a headless profile's browser that no fake Chromium can
// reach: adopting a launch that REPLACES a dead one, and the profile
// ending when its browser context is cancelled under it.
//
// Both are tested against plain contexts rather than a process, because
// what they turn on is chromedp's contract and not Chromium's: the browser
// context is cancelled when the connection to the process is lost
// (allocate.go's LostConnection goroutine calls that context's own cancel),
// so "the browser died" IS "this context was cancelled" and nothing else
// has to be simulated.

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

// TestAdoptingABrowserCancelsTheOneItReplaced: ensureBrowser relaunches
// when the previous browser context is CANCELLED, which is exactly what a
// Chromium that died looks like, and chromedp's allocator holds a
// goroutine, a WaitGroup and the process's own reaping behind the ALLOC
// cancel, which is not cancelled by the browser context going. Overwriting
// the two funcs dropped the only reference to both.
func TestAdoptingABrowserCancelsTheOneItReplaced(t *testing.T) {
	lp := newLifetimeProfile(t)

	firstCtx, firstCancel := context.WithCancel(context.Background())
	var firstAllocCancelled bool
	if err := lp.adopt(firstCtx, firstCancel, func() { firstAllocCancelled = true }); err != nil {
		t.Fatalf("adopt the first browser: %v", err)
	}

	secondCtx, secondCancel := context.WithCancel(context.Background())
	defer secondCancel()
	if err := lp.adopt(secondCtx, secondCancel, func() {}); err != nil {
		t.Fatalf("adopt the replacement: %v", err)
	}

	if firstCtx.Err() == nil {
		t.Error("the replaced browser's context was left live; its page contexts stay under it")
	}
	if !firstAllocCancelled {
		t.Error("the replaced browser's allocator was never cancelled; its process is never reaped")
	}
	if secondCtx.Err() != nil {
		t.Fatal("adopting cancelled the browser it was installing")
	}
	// The replaced browser's watcher must stay silent: the profile is
	// alive and running the new one.
	time.Sleep(50 * time.Millisecond)
	if got := lp.closedPages(); len(got) != 0 {
		t.Fatalf("the replaced browser's watcher reported %v; only the CURRENT browser's death ends the profile", got)
	}
	lp.headlessProfile.mu.Lock()
	disposed := lp.disposed
	lp.headlessProfile.mu.Unlock()
	if disposed {
		t.Fatal("adopting a replacement disposed the profile")
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
	if err := lp.adopt(browserCtx, browserCancel, func() {}); err != nil {
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
	if err := lp.adopt(browserCtx, browserCancel, func() {}); err != nil {
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
