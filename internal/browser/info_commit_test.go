package browser

import (
	"context"
	"sync/atomic"
	"testing"
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
				access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
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

// operationReaders are tool calls that read a page's info after acting on
// it, under the page-operation lock, and answer it.
var operationReaders = []struct {
	name string
	read func(*testing.T, *Manager, Access, string)
}{
	{"Wait", func(t *testing.T, m *Manager, access Access, pageID string) {
		if _, err := m.Wait(t.Context(), access, pageID, "", 1); err != nil {
			t.Error(err)
		}
	}},
	{"Open", func(t *testing.T, m *Manager, access Access, pageID string) {
		if _, err := m.Open(t.Context(), access, "https://example.test/c", OpenOptions{PageID: pageID}); err != nil {
			t.Error(err)
		}
	}},
	{"Snapshot", func(t *testing.T, m *Manager, access Access, pageID string) {
		p, _, err := m.lookupOwnedPage(access, pageID)
		if err != nil {
			t.Fatal(err)
		}
		fake := p.driver.(*lifecycleDriver).pageDriver.(*fakePage)
		fake.mu.Lock()
		fake.snapshot = func(context.Context) (Snapshot, error) {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			return Snapshot{PageInfo: PageInfo{URL: fake.url, Title: fake.title}}, nil
		}
		fake.mu.Unlock()
		if _, err := m.Snapshot(t.Context(), access, pageID); err != nil {
			t.Error(err)
		}
	}},
}

// An engine report is not under the page lock: one that lands during a read
// (the page moved to B), the read for itself or after an operation, wins
// over the read's older answer.
func TestAReadNeverCommitsOverAnEngineReport(t *testing.T) {
	for _, reader := range append(infoReaders[:len(infoReaders):len(infoReaders)], operationReaders...) {
		t.Run(reader.name, func(t *testing.T) {
			manager, _ := newLifecycleManager(t)
			access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
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
					manager.updatePageInfo(p.driver.Handle(), b, fakeTitle(b), 1)
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
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
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
	manager.refreshHistoryState(p, p.readStamp())
	if info := p.cachedInfo(); info.CanGoBack || !info.CanGoForward {
		t.Fatalf("after going back the cache reads back %v forward %v, want false true", info.CanGoBack, info.CanGoForward)
	}
}
