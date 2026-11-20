package browser

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/browser/turnorder"
)

// testThreads makes a hand-built Manager's thread lifetimes, one per id,
// each with the session given, or the default session for a zero one.
func testThreads(sessions map[string]SessionInfo) map[ThreadID]*threadBrowser {
	threads := make(map[ThreadID]*threadBrowser, len(sessions))
	for id, session := range sessions {
		tb := newThreadBrowser(ThreadID(id))
		if !reflect.DeepEqual(session, SessionInfo{}) {
			tb.session = session
		}
		threads[ThreadID(id)] = tb
	}
	return threads
}

// ownThreads makes m the Manager of the lifetimes a hand-built m was given
// (testThreads), as its own registry's begin would, so its entries accept
// an Access bound to them.
func ownThreads(m *Manager) {
	for _, tb := range m.threads {
		tb.manager = m
	}
}

// setActiveTab selects pageID in access's lifetime with no turn and no push.
func setActiveTab(m *Manager, access Access, pageID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	access.tb.session.ActivePageID = pageID
}

// closePendingTab closes access's tab still being created, as a user's
// close of it does.
func closePendingTab(m *Manager, access Access, pageID string) (*pendingPage, bool) {
	m.mu.Lock()
	pending, ok := m.closePendingLocked(access.tb, PageID(pageID))
	m.mu.Unlock()
	if ok {
		m.pendingClosed(access.tb)
	}
	return pending, ok
}

// beginCloseLive starts the close of access's live page, or joins the one
// running, and answers it.
func beginCloseLive(m *Manager, access Access, pageID string) (*pageClose, error) {
	m.mu.Lock()
	c, started, err := m.beginCloseLiveLocked(access.tb, PageID(pageID), false)
	m.mu.Unlock()
	if started != nil {
		m.startClose(started, c)
	}
	return c, err
}

// lifetimeOf answers the thread's current lifetime, nil for none.
func lifetimeOf(m *Manager, threadID string) *threadBrowser {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentLocked(ThreadID(threadID))
}

// A close bound before the thread's close, delivered through the pane mount
// of the closed lifetime after a fresh lifetime opened a tab, acts on the
// closed lifetime, which has no tab: it passes, and the fresh tab stays,
// even named. The action's end answers the close.
func TestARetiredKeyboardCloseLeavesTheFreshTab(t *testing.T) {
	m, access, _, mount := newSelectionManager(t)
	if err := m.CloseThread(t.Context(), access.ThreadID()); err != nil {
		t.Fatal(err)
	}
	fresh := m.Bind(access.Caller())
	tab, err := m.NewCompanionPage(t.Context(), fresh, addr("fresh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CloseCompanionPage(numbered(t.Context(), mount, 1), access, PageID(tab.ID)); err != nil {
		t.Fatalf("the retired close answered %v; want it passed", err)
	}
	if err := access.End(); !errors.Is(err, ErrThreadClosed) {
		t.Fatalf("the retired action ended with %v, want ErrThreadClosed", err)
	}
	if ids := tabIDs(m, fresh); !slices.Equal(ids, []string{tab.ID}) {
		t.Fatalf("after the retired close the thread has tabs %q, want the fresh one", ids)
	}
	// With no number its commit refuses the retired lifetime.
	if err := m.CloseCompanionPage(t.Context(), access, PageID(tab.ID)); err != nil {
		t.Fatalf("the retired unnumbered close answered %v; want it passed", err)
	}
	if ids := tabIDs(m, fresh); !slices.Equal(ids, []string{tab.ID}) {
		t.Fatalf("after the retired commit the thread has tabs %q, want the fresh one", ids)
	}
}

// A turn admitted in a mount before its lifetime's close commits as passed
// after it: the close ends the mount's order.
func TestAThreadsCloseEndsItsMountsOrder(t *testing.T) {
	m, access, _, mount := newSelectionManager(t)
	turn, err := m.awaitTurn(t.Context(), access, place(mount, 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CloseThread(t.Context(), access.ThreadID()); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	standing := turn.Commit()
	m.mu.Unlock()
	if standing != turnorder.Passed {
		t.Fatalf("a turn admitted before the close commits %v, want passed", standing)
	}
}

// A numbered command of a fresh lifetime's action, made in a mount of the
// closed lifetime that never ordered one, passes: the mount belongs to the
// lifetime it attached to, not to whichever is the thread's now.
func TestAMountOfAClosedLifetimeOrdersNothingOfTheFreshOne(t *testing.T) {
	m, access, _ := newTabsManager(t, addr("a"))
	mount, err := m.AttachPane(access.Caller())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CloseThread(t.Context(), access.ThreadID()); err != nil {
		t.Fatal(err)
	}
	fresh := m.Bind(access.Caller())
	tab, err := m.NewCompanionPage(t.Context(), fresh, addr("fresh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CloseCompanionPage(numbered(t.Context(), mount.ID, 1), fresh, PageID(tab.ID)); err != nil {
		t.Fatalf("a command of the closed lifetime's mount answered %v; want it passed", err)
	}
	if ids := tabIDs(m, fresh); !slices.Equal(ids, []string{tab.ID}) {
		t.Fatalf("the thread has tabs %q, want the fresh one", ids)
	}
}

// The thread's close detaches its lifetime and then closes what it
// detached: a lifetime bound during that cleanup opens and saves tabs the
// cleanup never touches, in memory and in the saved copy a fresh Manager
// loads.
func TestAnOldThreadsCloseLeavesTheFreshLifetime(t *testing.T) {
	dir := t.TempDir()
	manager, engine := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	old := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page := mustOpen(t, manager, old, addr("old"))
	release := engine.holdCloseOf(t, string(livePage(t, manager, old, page.ID).driver.Handle()))
	closed := async(func() error { return manager.CloseThread(context.Background(), old.ThreadID()) })
	eventually(t, "the close detaches the old lifetime", func() bool { return lifetimeOf(manager, string(old.ThreadID())) != old.tb })

	fresh := manager.Bind(old.Caller())
	saved := mustOpen(t, manager, fresh, addr("saved"))
	live := mustOpen(t, manager, fresh, addr("live"))
	suspendTab(t, manager, fresh, saved.ID)
	if err := manager.persistThreads([]ThreadID{fresh.ThreadID()}); err != nil {
		t.Fatal(err)
	}
	release()
	if err := await(t, closed, "the old lifetime's close"); err != nil {
		t.Fatal(err)
	}
	if err := fresh.End(); err != nil {
		t.Fatalf("the fresh lifetime's action ended with %v", err)
	}
	if ids := tabIDs(manager, fresh); !slices.Equal(ids, []string{saved.ID, live.ID}) {
		t.Fatalf("after the old close the thread has tabs %q, want the fresh lifetime's", ids)
	}
	restarted, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	if ids := tabIDs(restarted, fresh); !slices.Equal(ids, []string{saved.ID}) {
		t.Fatalf("a fresh Manager loads tabs %q, want the fresh lifetime's saved one", ids)
	}
}

// An action bound before the thread's close opens nothing after it, however
// long ago it ended or dropped its references: the lifetime it holds stays
// detached.
func TestAReleasedBindingOpensNothingAfterTheClose(t *testing.T) {
	m, access, _ := newTabsManager(t, addr("a"))
	if err := access.End(); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseThread(t.Context(), access.ThreadID()); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	if _, err := m.NewCompanionPage(t.Context(), access, addr("late")); !errors.Is(err, ErrThreadClosed) {
		t.Fatalf("a released binding's open answered %v, want ErrThreadClosed", err)
	}
	if pages := m.CompanionState(access.Caller()).Pages; len(pages) != 0 {
		t.Fatalf("the closed thread has tabs %+v", pages)
	}
}

// A duplicate whose source's browsing is forgotten while its tab is created
// (Clear Site Data run to completion as the creating tab is pushed) opens
// nothing at the forgotten address: its claim on the browsing is void.
func TestADuplicateWhoseBrowsingWasForgottenOpensNothing(t *testing.T) {
	m, access, ids := newTabsManager(t, addr("keep"), addr("x"))
	var cleared bool
	m.SetEventSink(func(event CompanionEvent) {
		if cleared || ThreadID(event.ThreadID) != access.ThreadID() {
			return
		}
		for _, page := range event.Pages {
			if page.Creating {
				cleared = true
				if err := m.ClearSiteData(context.Background()); err != nil {
					t.Error(err)
				}
				return
			}
		}
	})
	if err := m.DuplicateCompanionPage(t.Context(), access, PageID(ids[1])); !errors.Is(err, errPageNotFound) {
		t.Fatalf("the duplicate answered %v, want its source gone", err)
	}
	if !cleared {
		t.Fatal("the clear never ran")
	}
	for _, tab := range m.CompanionState(access.Caller()).Pages {
		if tab.URL == addr("x") {
			t.Fatalf("the duplicate opened the forgotten address: %+v", tab)
		}
	}
	m.mu.Lock()
	pending := len(m.pending)
	m.mu.Unlock()
	if pending != 0 {
		t.Fatalf("%d creations are left", pending)
	}
}

// holdCloseAfterDetach holds the thread's close at its first push after it
// detached the lifetime of access, before it closes any page, and answers
// when that point is reached and the release.
func holdCloseAfterDetach(m *Manager, access Access) (reached <-chan struct{}, release func()) {
	at := make(chan struct{})
	gate := make(chan struct{})
	var held atomic.Bool
	m.SetEventSink(func(event CompanionEvent) {
		// Only the first push after the detach waits; any other passes.
		if ThreadID(event.ThreadID) == access.ThreadID() && access.tb.detached.Load() && held.CompareAndSwap(false, true) {
			close(at)
			<-gate
		}
	})
	var released sync.Once
	return at, func() { released.Do(func() { close(gate) }) }
}

// recordsOf counts the suspended records tb owns.
func recordsOf(m *Manager, tb *threadBrowser) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, rec := range m.suspended {
		if rec.owner == tb {
			n++
		}
	}
	return n
}

// A suspension of a page whose lifetime the thread's close detached before
// it reached the page records nothing: the close closes the page, and a
// record of a detached lifetime is one nothing would ever forget.
func TestASuspensionDuringTheThreadsCloseRecordsNothing(t *testing.T) {
	m, engine := newLifecycleManager(t)
	access := m.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page := mustOpen(t, m, access, addr("a"))
	p := livePage(t, m, access, page.ID)
	releaseRead := engine.hold(&engine.infoGate)
	suspended := async(func() bool {
		moved, _ := m.suspendPage(context.Background(), p)
		return moved
	})
	receive(t, engine.reading, "the suspension's read")
	reached, releaseClose := holdCloseAfterDetach(m, access)
	closed := async(func() error { return m.CloseThread(context.Background(), access.ThreadID()) })
	<-reached
	releaseRead()
	if await(t, suspended, "the suspension") {
		t.Fatal("a page of a detached lifetime was suspended")
	}
	releaseClose()
	if err := await(t, closed, "the thread's close"); err != nil {
		t.Fatal(err)
	}
	if n := recordsOf(m, access.tb); n != 0 {
		t.Fatalf("the closed lifetime keeps %d records", n)
	}
}

// A crash of a page whose lifetime the thread's close detached before it
// reached the page records nothing, for the same reason.
func TestACrashDuringTheThreadsCloseRecordsNothing(t *testing.T) {
	m, _ := newLifecycleManager(t)
	access := m.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page := mustOpen(t, m, access, addr("a"))
	handle := livePage(t, m, access, page.ID).driver.Handle()
	reached, releaseClose := holdCloseAfterDetach(m, access)
	closed := async(func() error { return m.CloseThread(context.Background(), access.ThreadID()) })
	<-reached
	m.pageCrashed(handle, "crashed")
	releaseClose()
	if err := await(t, closed, "the thread's close"); err != nil {
		t.Fatal(err)
	}
	if n := recordsOf(m, access.tb); n != 0 {
		t.Fatalf("the closed lifetime keeps %d records", n)
	}
}

// A clear of the downloads list bound before the thread's close is refused
// after it and leaves the list of the lifetime bound after the close.
func TestARetiredClearOfTheDownloadsLeavesTheFreshList(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	startDownload(t, manager, access, page, "old.txt", 1)
	if err := manager.CloseThread(t.Context(), access.ThreadID()); err != nil {
		t.Fatal(err)
	}
	fresh := manager.Bind(access.Caller())
	startDownload(t, manager, fresh, mustOpen(t, manager, fresh, addr("files")), "fresh.txt", 1)
	if err := manager.ClearThreadDownloads(access); !errors.Is(err, ErrThreadClosed) {
		t.Fatalf("the retired clear answered %v, want ErrThreadClosed", err)
	}
	if listed := threadDownloads(manager, fresh); len(listed) != 1 || listed[0].Name != "fresh.txt" {
		t.Fatalf("the fresh lifetime lists %+v, want its own download", listed)
	}
}
