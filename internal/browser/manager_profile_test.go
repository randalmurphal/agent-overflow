package browser

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

// disposeCountingEngine records the profile disposals the Manager performs.
// It is the fake engine plus a tally, because WHEN a profile is disposed is
// Manager policy and holds on every engine. Each hook, when set, runs once as
// the engine creates a profile or a profile creates a page; an onNewPage
// error refuses the page. A disposeErr, when set, fails every disposal after
// the profile is gone, as a browser process that would not stop does.
type disposeCountingEngine struct {
	*fakeEngine
	disposed     atomic.Int64
	disposeErr   error
	onStarted    func()
	onNewProfile func()
	onNewPage    func() error
}

// Start runs onStarted once, after the engine started.
func (e *disposeCountingEngine) Start(ctx context.Context) error {
	err := e.fakeEngine.Start(ctx)
	if hook := e.onStarted; hook != nil && err == nil {
		e.onStarted = nil
		hook()
	}
	return err
}

func newDisposeCountingManager(t *testing.T) (*Manager, *disposeCountingEngine) {
	t.Helper()
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() { manager.Close() })
	engine := &disposeCountingEngine{fakeEngine: newFakeEngine()}
	manager.engine = engine
	return manager, engine
}

func (e *disposeCountingEngine) NewProfile(ctx context.Context, opts profileOptions) (engineProfile, error) {
	if hook := e.onNewProfile; hook != nil {
		e.onNewProfile = nil
		hook()
	}
	profile, err := e.fakeEngine.NewProfile(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &countingProfile{engineProfile: profile, owner: e}, nil
}

type countingProfile struct {
	engineProfile
	owner *disposeCountingEngine
}

func (p *countingProfile) NewPage(ctx context.Context, hooks pageHooks) (pageDriver, error) {
	if hook := p.owner.onNewPage; hook != nil {
		p.owner.onNewPage = nil
		if err := hook(); err != nil {
			return nil, err
		}
	}
	return p.engineProfile.NewPage(ctx, hooks)
}

func (p *countingProfile) Dispose(ctx context.Context) error {
	p.owner.disposed.Add(1)
	return errors.Join(p.engineProfile.Dispose(ctx), p.owner.disposeErr)
}

// A thread's close reports a workspace profile that fails to dispose, as
// closing its last page alone does, and still closes the thread's pages in
// every other workspace. Deleting a thread is the caller: a headless
// browser that would not stop, or ephemeral site data left on disk, reaches
// the person deleting it.
func TestCloseThreadReportsAProfileThatFailsToDispose(t *testing.T) {
	manager, engine := newDisposeCountingManager(t)
	engine.disposeErr = errors.New("the profile's browser would not stop")
	thread := "thread"
	for range 2 {
		access := manager.Bind(Caller{ThreadID: ThreadID(thread), Workspace: t.TempDir()})
		if _, err := manager.Open(t.Context(), access, "https://example.test/", OpenOptions{}); err != nil {
			t.Fatalf("open: %v", err)
		}
	}
	err := manager.CloseThread(t.Context(), ThreadID(thread))
	if !errors.Is(err, engine.disposeErr) {
		t.Fatalf("CloseThread = %v, want the dispose failure", err)
	}
	if pages := manager.ownedPages(lifetimeOf(manager, thread)); len(pages) != 0 {
		t.Fatalf("the closed thread still owns %d page(s)", len(pages))
	}
	if got := engine.disposed.Load(); got != 2 {
		t.Fatalf("%d of 2 workspace profiles were disposed", got)
	}
	if err := manager.CloseThread(t.Context(), ThreadID(thread)); err != nil {
		t.Fatalf("a second close of the closed thread: %v", err)
	}
}

// A workspace's profile is disposed when its LAST page closes, not when the
// idle timer eventually stops the engine. On an engine whose profile owns a
// browser PROCESS — the headless Chromium one — that is the moment the
// process dies, so a serve host with no open page runs no browser long
// before idleBrowserDelay elapses. The engine half of that chain is
// headless_engine_test.go; this is the half that decides when.
func TestManagerDisposesAWorkspaceProfileWithItsLastPage(t *testing.T) {
	manager, engine := newDisposeCountingManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	first, err := manager.Open(t.Context(), access, "https://example.test/one", OpenOptions{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	second, err := manager.Open(t.Context(), access, "https://example.test/two", OpenOptions{})
	if err != nil {
		t.Fatalf("open a second page: %v", err)
	}
	if err := manager.ClosePage(t.Context(), access, PageID(first.ID)); err != nil {
		t.Fatalf("close the first page: %v", err)
	}
	if got := engine.disposed.Load(); got != 0 {
		t.Fatalf("the workspace profile was disposed with a page still open (%d disposals)", got)
	}
	if err := manager.ClosePage(t.Context(), access, PageID(second.ID)); err != nil {
		t.Fatalf("close the last page: %v", err)
	}
	if got := engine.disposed.Load(); got != 1 {
		t.Fatalf("the workspace's last page left its profile alive (%d disposals)", got)
	}
}

// lastPageClosers are the two ways a page leaves the registry: the Manager
// closes it, or the engine reports it closed.
var lastPageClosers = []struct {
	name  string
	close func(t *testing.T, m *Manager, access Access, pageID string)
}{
	{"closed by the manager", func(t *testing.T, m *Manager, access Access, pageID string) {
		if err := m.ClosePage(context.Background(), access, PageID(pageID)); err != nil {
			t.Errorf("close page: %v", err)
		}
	}},
	{"closed by the engine", func(t *testing.T, m *Manager, access Access, pageID string) {
		m.removeClosedPage(driverHandle(m, access, pageID))
	}},
}

func registeredScopes(m *Manager) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.scopes)
}

// A page opened in a workspace after its last page's close has emptied the
// workspace, but before that close has disposed the profile, stays open: the
// close disposes only the profile it emptied.
func TestAPageOpenedAsItsWorkspacesLastPageClosesStaysOpen(t *testing.T) {
	for _, closer := range lastPageClosers {
		t.Run(closer.name, func(t *testing.T) {
			manager, engine := newDisposeCountingManager(t)
			workspace := t.TempDir()
			closing := manager.Bind(Caller{ThreadID: "closing", Workspace: workspace})
			last, err := manager.Open(t.Context(), closing, "https://example.test/", OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			other := manager.Bind(Caller{ThreadID: "other", Workspace: workspace})
			var opened PageInfo
			var openErr error
			var reported atomic.Bool
			// Either close reports the closing thread's empty state between
			// emptying the workspace and disposing its profile.
			manager.SetEventSink(func(event CompanionEvent) {
				if ThreadID(event.ThreadID) == closing.ThreadID() && len(event.Pages) == 0 && reported.CompareAndSwap(false, true) {
					opened, openErr = manager.Open(context.Background(), other, "https://example.test/other", OpenOptions{})
				}
			})
			closer.close(t, manager, closing, last.ID)
			if !reported.Load() {
				t.Fatal("the close reported no empty state for the closing thread")
			}
			if openErr != nil {
				t.Fatalf("open during the close: %v", openErr)
			}
			// The Manager's close keeps the profile until the page's
			// teardown ends, so the page opened on its state push joins it;
			// an engine's close has already emptied it, and the open waits
			// for its disposal.
			want := int64(1)
			if closer.name == "closed by the manager" {
				want = 0
			}
			if got := engine.disposed.Load(); got != want {
				t.Fatalf("%d profile disposals, want %d", got, want)
			}
			if _, err := manager.Open(t.Context(), other, "https://example.test/next", OpenOptions{PageID: PageID(opened.ID)}); err != nil {
				t.Fatalf("navigate the page opened during the close: %v", err)
			}
		})
	}
}

// A page being created keeps its workspace's profile alive when the
// workspace's last page closes meanwhile.
func TestAPageCreatedAsItsWorkspacesLastPageClosesStaysOpen(t *testing.T) {
	for _, closer := range lastPageClosers {
		t.Run(closer.name, func(t *testing.T) {
			manager, engine := newDisposeCountingManager(t)
			workspace := t.TempDir()
			closing := manager.Bind(Caller{ThreadID: "closing", Workspace: workspace})
			last, err := manager.Open(t.Context(), closing, "https://example.test/", OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			engine.onNewPage = func() error {
				closer.close(t, manager, closing, last.ID)
				return nil
			}
			other := manager.Bind(Caller{ThreadID: "other", Workspace: workspace})
			opened, err := manager.Open(t.Context(), other, "https://example.test/other", OpenOptions{})
			if err != nil {
				t.Fatalf("open while the workspace's last page closes: %v", err)
			}
			if got := engine.disposed.Load(); got != 0 {
				t.Fatalf("the profile a page was being created in was disposed (%d disposals)", got)
			}
			if _, err := manager.Open(t.Context(), other, "https://example.test/next", OpenOptions{PageID: PageID(opened.ID)}); err != nil {
				t.Fatalf("navigate the page created during the close: %v", err)
			}
		})
	}
}

// A page creation that fails disposes the profile it leaves empty: a new
// workspace's, or one whose last page closed while the page was being
// created.
func TestAFailedPageCreationDisposesTheProfileItLeavesEmpty(t *testing.T) {
	refused := errors.New("engine refused the page")
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, m *Manager, workspace string) func()
	}{
		{"new workspace", func(*testing.T, *Manager, string) func() { return func() {} }},
		{"last page closed meanwhile", func(t *testing.T, m *Manager, workspace string) func() {
			closing := m.Bind(Caller{ThreadID: "closing", Workspace: workspace})
			last, err := m.Open(t.Context(), closing, "https://example.test/", OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			return func() {
				if err := m.ClosePage(context.Background(), closing, PageID(last.ID)); err != nil {
					t.Errorf("close page: %v", err)
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, engine := newDisposeCountingManager(t)
			workspace := t.TempDir()
			during := tc.setup(t, manager, workspace)
			engine.onNewPage = func() error {
				during()
				return refused
			}
			_, err := manager.Open(t.Context(), manager.Bind(Caller{ThreadID: "other", Workspace: workspace}), "https://example.test/other", OpenOptions{})
			if !errors.Is(err, refused) {
				t.Fatalf("open = %v, want the engine's refusal", err)
			}
			if got := engine.disposed.Load(); got != 1 {
				t.Fatalf("%d profile disposals, want the emptied profile's one", got)
			}
			if got := registeredScopes(manager); got != 0 {
				t.Fatalf("%d workspace scopes stay registered with no page", got)
			}
		})
	}
}

// A page close that finishes after a browser restart leaves the profile its
// workspace reopened with alone.
func TestACloseSpanningABrowserRestartSparesTheReopenedProfile(t *testing.T) {
	manager, engine := newCloseHookManager(t)
	workspace := t.TempDir()
	closing := manager.Bind(Caller{ThreadID: "closing", Workspace: workspace})
	last, err := manager.Open(t.Context(), closing, "https://example.test/", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	other := manager.Bind(Caller{ThreadID: "other", Workspace: workspace})
	var opened PageInfo
	engine.onClose = func(string) {
		// A persistence change closes the browser, and the workspace
		// reopens before the page's close finishes.
		if err := manager.Reconfigure(Config{Enabled: true, PersistSiteData: true}); err != nil {
			t.Errorf("reconfigure: %v", err)
		}
		var openErr error
		if opened, openErr = manager.Open(context.Background(), other, "https://example.test/other", OpenOptions{}); openErr != nil {
			t.Errorf("reopen the workspace: %v", openErr)
		}
	}
	if err := manager.ClosePage(t.Context(), closing, PageID(last.ID)); err != nil {
		t.Fatalf("close page: %v", err)
	}
	if _, err := manager.Open(t.Context(), other, "https://example.test/next", OpenOptions{PageID: PageID(opened.ID)}); err != nil {
		t.Fatalf("navigate the page the workspace reopened with: %v", err)
	}
}

// runConcurrently runs fn on its own goroutine and returns once fn is
// running. The returned channel closes when fn returns.
func runConcurrently(fn func()) <-chan struct{} {
	running, done := make(chan struct{}), make(chan struct{})
	go func() {
		close(running)
		fn()
		close(done)
	}()
	<-running
	return done
}

// The idle close that fires while a page is being created, once the engine
// has started for it or while its workspace's profile is being created,
// keeps the engine and the page.
func TestAnIdleCloseDuringPageCreationKeepsThePage(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(t *testing.T, e *disposeCountingEngine, hook func()) context.Context
	}{
		{"engine started", func(t *testing.T, e *disposeCountingEngine, hook func()) context.Context {
			e.onStarted = hook
			return t.Context()
		}},
		{"profile being created", func(t *testing.T, e *disposeCountingEngine, hook func()) context.Context {
			e.onNewProfile = hook
			return t.Context()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, engine := newDisposeCountingManager(t)
			var idleClosed <-chan struct{}
			ctx := tc.arm(t, engine, func() { idleClosed = runConcurrently(manager.closeIdleBrowser) })
			access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
			page, err := manager.Open(ctx, access, "https://example.test/", OpenOptions{})
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if idleClosed == nil {
				t.Fatal("the page's creation did not reach the hook")
			}
			<-idleClosed
			if _, err := manager.Open(t.Context(), access, "https://example.test/next", OpenOptions{PageID: PageID(page.ID)}); err != nil {
				t.Fatalf("navigate the page after the idle close: %v", err)
			}
		})
	}
}

// With no page left, the idle close stops the engine.
func TestAnIdleCloseStopsTheEngineWithNoPage(t *testing.T) {
	manager, engine := newDisposeCountingManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page, err := manager.Open(t.Context(), access, "https://example.test/", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	manager.closeIdleBrowser()
	if !engine.Running() {
		t.Fatal("the idle close stopped the engine with a page open")
	}
	if err := manager.ClosePage(t.Context(), access, PageID(page.ID)); err != nil {
		t.Fatal(err)
	}
	manager.closeIdleBrowser()
	if engine.Running() {
		t.Fatal("the idle close left the engine running with no page")
	}
}

// refusingProfileEngine refuses new profiles once refuse is set.
type refusingProfileEngine struct {
	*fakeEngine
	refuse atomic.Bool
}

func (e *refusingProfileEngine) NewProfile(ctx context.Context, opts profileOptions) (engineProfile, error) {
	if e.refuse.Load() {
		return nil, errors.New("profile refused")
	}
	return e.fakeEngine.NewProfile(ctx, opts)
}

// An open whose profile is refused leaves no profile, so the running engine
// gets an idle close: whether the open started it or found it idle.
func TestAnOpenWhoseProfileIsRefusedLeavesTheEngineToTheIdleClose(t *testing.T) {
	for _, openedBefore := range []bool{false, true} {
		t.Run(fmt.Sprintf("opened before=%v", openedBefore), func(t *testing.T) {
			manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
			t.Cleanup(func() { manager.Close() })
			engine := &refusingProfileEngine{fakeEngine: newFakeEngine()}
			manager.engine = engine
			access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
			if openedBefore {
				page, err := manager.Open(t.Context(), access, "https://example.test/", OpenOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if err := manager.ClosePage(t.Context(), access, PageID(page.ID)); err != nil {
					t.Fatal(err)
				}
			}
			engine.refuse.Store(true)
			if _, err := manager.Open(t.Context(), access, "https://example.test/", OpenOptions{}); err == nil {
				t.Fatal("the open succeeded without a profile")
			}
			manager.mu.Lock()
			armed := manager.idleTimer != nil
			scopes := len(manager.scopes)
			manager.mu.Unlock()
			if !engine.Running() || scopes != 0 || !armed {
				t.Fatalf("engine running %v, %d profiles, idle close armed %v; want a running engine with no profile armed to close", engine.Running(), scopes, armed)
			}
		})
	}
}
