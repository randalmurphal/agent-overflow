package browser

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// A read of a page's info (SelectPage, Pages) commits what it read. A
// navigation to B that runs between the read and its commit, whole or with
// only its own read there, still leaves the cache and the newest push at B:
// the read and its commit share one hold of the page lock, so neither
// ordering can happen, and a commit that did land during a read rejects it.
func TestAReadNeverCommitsOverANavigation(t *testing.T) {
	for _, reader := range infoReaders {
		for _, whole := range []bool{true, false} {
			name := reader.name + "/navigation read in the gap"
			if whole {
				name = reader.name + "/navigation whole in the gap"
			}
			t.Run(name, func(t *testing.T) {
				manager, _ := newLifecycleManager(t)
				access := Access{ThreadID: "thread", Workspace: t.TempDir()}
				page := mustOpen(t, manager, access, "https://example.test/a")
				p, _, err := manager.lookupOwnedPage(access, page.ID)
				if err != nil {
					t.Fatal(err)
				}
				pushes := countStatePushes(manager)
				const b = "https://example.test/b"
				navigated := make(chan error, 1)
				navigate := func() {
					_, err := manager.Open(context.Background(), access, b, OpenOptions{PageID: page.ID})
					navigated <- err
				}
				navigationRead := make(chan struct{})
				release := make(chan struct{})
				var calls atomic.Int32
				var gapOpen bool
				manager.infoRead = func(*managedPage) {
					switch calls.Add(1) {
					case 1:
						// The read under test, between its read and its commit.
						if !p.mu.TryLock() {
							// The page lock is held: the navigation waits for
							// the commit.
							go navigate()
							return
						}
						p.mu.Unlock()
						gapOpen = true
						if whole {
							navigate()
							return
						}
						go navigate()
						<-navigationRead
					case 2:
						// The navigation's read, before its commit.
						if !whole && gapOpen {
							close(navigationRead)
							<-release
						}
					}
				}
				reader.read(t, manager, access, page.ID)
				close(release)
				if err := <-navigated; err != nil {
					t.Fatal(err)
				}
				if got := p.cachedInfo().URL; got != b {
					t.Fatalf("the cache holds %q after a navigation to %q (gap open: %v)", got, b, gapOpen)
				}
				if got, want := pushes.shownTitle(page.ID), fakeTitle(b); got != want {
					t.Fatalf("the newest push shows %q, want %q (gap open: %v)", got, want, gapOpen)
				}
			})
		}
	}
}

// An engine report is not under the page lock: one that lands during a read
// (the page moved to B) wins over the read's older answer.
func TestAReadNeverCommitsOverAnEngineReport(t *testing.T) {
	for _, reader := range infoReaders {
		t.Run(reader.name, func(t *testing.T) {
			manager, _ := newLifecycleManager(t)
			access := Access{ThreadID: "thread", Workspace: t.TempDir()}
			page := mustOpen(t, manager, access, "https://example.test/a")
			p, _, err := manager.lookupOwnedPage(access, page.ID)
			if err != nil {
				t.Fatal(err)
			}
			pushes := countStatePushes(manager)
			const b = "https://example.test/b"
			var calls atomic.Int32
			manager.infoRead = func(*managedPage) {
				if calls.Add(1) == 1 {
					p.driver.(*lifecycleDriver).pageDriver.(*fakePage).SameDocumentNavigation(b)
					manager.updatePageInfo(p.driver.Handle(), b, fakeTitle(b))
				}
			}
			reader.read(t, manager, access, page.ID)
			if got := p.cachedInfo().URL; got != b {
				t.Fatalf("the cache holds %q after the engine reported %q", got, b)
			}
			if got, want := pushes.shownTitle(page.ID), fakeTitle(b); got != want {
				t.Fatalf("the newest push shows %q, want %q", got, want)
			}
		})
	}
}

// The history refresh after an engine report runs without the page lock: an
// operation that commits while it reads (a back) wins over its older answer.
func TestAHistoryRefreshNeverCommitsOverANewerRead(t *testing.T) {
	manager, _ := newLifecycleManager(t)
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	page := mustOpen(t, manager, access, "https://example.test/a")
	if _, err := manager.Open(t.Context(), access, "https://example.test/b", OpenOptions{PageID: page.ID}); err != nil {
		t.Fatal(err)
	}
	p, _, err := manager.lookupOwnedPage(access, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info := p.cachedInfo(); !info.CanGoBack || info.CanGoForward {
		t.Fatalf("at the second entry the cache reads %+v", info)
	}
	var calls atomic.Int32
	manager.infoRead = func(*managedPage) {
		if calls.Add(1) == 1 {
			// The refresh read back available, forward not; the page goes back.
			if _, err := manager.History(t.Context(), access, page.ID, "back"); err != nil {
				t.Error(err)
			}
		}
	}
	manager.refreshHistoryState(p, p.infoGeneration())
	if info := p.cachedInfo(); info.CanGoBack || !info.CanGoForward {
		t.Fatalf("after going back the cache reads back %v forward %v, want false true", info.CanGoBack, info.CanGoForward)
	}
}

// A native view's reports are delivered in the order it made them, the
// newest last, whatever runs while one is delivered.
func TestNativeInfoReportsArriveInOrder(t *testing.T) {
	var reports infoReports
	delivered := make(chan string, 8)
	release := make(chan struct{})
	deliver := func(url, _ string) {
		delivered <- url
		if url == "1" {
			<-release
		}
	}
	next := func(want string) {
		t.Helper()
		select {
		case got := <-delivered:
			if got != want {
				t.Fatalf("delivered %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%q was not delivered", want)
		}
	}
	reports.report("1", "", deliver)
	next("1")
	// While 1 is delivered, 2 and then 3 are made: 3 replaces 2 and follows 1.
	reports.report("2", "", deliver)
	reports.report("3", "", deliver)
	close(release)
	next("3")
	reports.report("4", "", deliver)
	next("4")
}
