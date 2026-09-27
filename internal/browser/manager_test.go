package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Engine selection is provable on every platform without a display, and it is
// what decides what every deployment gets. These tests are deliberately
// tag-free: "windowless means NO engine" is the rule `--connect`, a headless
// serve mode, the harness and `go test` itself all land on, and a rule whose
// only failure mode is a silently launched browser must not live behind a
// platform tag.

func TestSelectEngineWithoutAWindowHasNoEngine(t *testing.T) {
	engine := selectEngine(t.TempDir(), ManagerOptions{}, engineEvents{})
	if _, ok := engine.(unavailableEngine); !ok {
		t.Fatalf("windowless selection = %T, want no engine", engine)
	}
}

func TestSelectEngineTakesTheFakeEngineWhenPinned(t *testing.T) {
	engine := selectEngine(t.TempDir(), ManagerOptions{FakeEngine: true}, engineEvents{})
	if _, ok := engine.(*fakeEngine); !ok {
		t.Fatalf("pinned selection = %T, want the fake engine", engine)
	}
}

// A windowless deployment answers every browser tool with ONE sentence. The
// path under test is the whole of it: the null-object engine refuses at the
// profile boundary, so no Manager path can nil-deref its way past the refusal.
func TestWindowlessDeploymentRefusesBrowserToolsBySentence(t *testing.T) {
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{})
	if manager.Available() {
		t.Fatal("a windowless deployment reported browser tools as available")
	}
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	_, err := manager.Open(t.Context(), access, "https://example.test", OpenOptions{})
	if err == nil || !strings.Contains(err.Error(), "not available in this deployment") {
		t.Fatalf("browser tool error = %v, want the unavailable sentence", err)
	}
	// Teardown on a deployment that never had an engine must be silent.
	manager.Close()
}

// The fake engine carries the Manager's whole policy layer: pages exist, are
// owned by one thread, navigate, and close. It is what the harness renders the
// companion pane against (spec §10), so its liveness is a shipped property.
func TestFakeEngineCarriesPageOwnershipAndNavigation(t *testing.T) {
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	defer manager.Close()
	workspace := t.TempDir()
	owner := Access{ThreadID: "owner", Workspace: workspace}
	info, err := manager.Open(t.Context(), owner, "https://example.test/docs/guide", OpenOptions{})
	if err != nil {
		t.Fatalf("open on the fake engine: %v", err)
	}
	if info.URL != "https://example.test/docs/guide" || info.Title != "guide" {
		t.Fatalf("page info = %#v", info)
	}
	state := manager.CompanionState(owner)
	if len(state.Pages) != 1 || state.Pages[0].ID != info.ID {
		t.Fatalf("companion state = %#v", state)
	}

	// A page belongs to the thread that opened it, and no other thread can
	// address it. That is Manager policy, and it holds on every engine.
	intruder := Access{ThreadID: "intruder", Workspace: workspace}
	if err := manager.ClosePage(t.Context(), intruder, info.ID); err == nil {
		t.Fatal("another thread closed a page it does not own")
	}
	if err := manager.ClosePage(t.Context(), owner, info.ID); err != nil {
		t.Fatalf("close own page: %v", err)
	}
	if pages := manager.CompanionState(owner).Pages; len(pages) != 0 {
		t.Fatalf("closed page survives: %#v", pages)
	}
}

// The fake engine keeps a real session history: navigating truncates only the
// entries AFTER the current one, so back/forward walk the pages actually
// visited. The pane's back/forward buttons drive this in the harness, and a
// history that forgot the previous page would make both dead ends.
func TestFakeEngineHistoryWalksBackAndForward(t *testing.T) {
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	defer manager.Close()
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	info, err := manager.Open(t.Context(), access, "https://example.test/first", OpenOptions{})
	if err != nil {
		t.Fatalf("open on the fake engine: %v", err)
	}
	if info.CanGoBack || info.CanGoForward {
		t.Fatalf("first page reports history to walk: %#v", info)
	}
	second, err := manager.Open(t.Context(), access, "https://example.test/second", OpenOptions{PageID: info.ID})
	if err != nil {
		t.Fatalf("second navigation: %v", err)
	}
	if !second.CanGoBack || second.CanGoForward {
		t.Fatalf("second page misreports history state: %#v", second)
	}
	back, err := manager.History(t.Context(), access, info.ID, "back")
	if err != nil || back.URL != "https://example.test/first" {
		t.Fatalf("back = %#v, %v", back, err)
	}
	if back.CanGoBack || !back.CanGoForward {
		t.Fatalf("walked-back page misreports history state: %#v", back)
	}
	forward, err := manager.History(t.Context(), access, info.ID, "forward")
	if err != nil || forward.URL != "https://example.test/second" {
		t.Fatalf("forward = %#v, %v", forward, err)
	}
	if _, err := manager.History(t.Context(), access, info.ID, "forward"); err == nil {
		t.Fatal("forward past the newest entry succeeded")
	}
}

// Content operations REFUSE on the fake engine rather than inventing a page,
// so a test that thinks it is driving a renderer fails loudly.
func TestFakeEngineRefusesPageContentByName(t *testing.T) {
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	defer manager.Close()
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	info, err := manager.Open(t.Context(), access, "https://example.test", OpenOptions{})
	if err != nil {
		t.Fatalf("open on the fake engine: %v", err)
	}
	if _, err := manager.Snapshot(t.Context(), access, info.ID); err == nil || !strings.Contains(err.Error(), "renders no page content") {
		t.Fatalf("snapshot error = %v, want the fake engine's refusal", err)
	}
}

// disposeCountingEngine records the profile disposals the Manager performs.
// It is the fake engine plus a tally, because WHEN a profile is disposed is
// Manager policy and holds on every engine. Each hook, when set, runs once as
// the engine creates a profile or a profile creates a page; an onNewPage
// error refuses the page.
type disposeCountingEngine struct {
	*fakeEngine
	disposed     atomic.Int64
	onNewProfile func()
	onNewPage    func() error
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
	return p.engineProfile.Dispose(ctx)
}

// A workspace's profile is disposed when its LAST page closes, not when the
// idle timer eventually stops the engine. On an engine whose profile owns a
// browser PROCESS — the headless Chromium one — that is the moment the
// process dies, so a serve host with no open page runs no browser long
// before idleBrowserDelay elapses. The engine half of that chain is
// headless_engine_test.go; this is the half that decides when.
func TestManagerDisposesAWorkspaceProfileWithItsLastPage(t *testing.T) {
	manager, engine := newDisposeCountingManager(t)
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	first, err := manager.Open(t.Context(), access, "https://example.test/one", OpenOptions{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	second, err := manager.Open(t.Context(), access, "https://example.test/two", OpenOptions{})
	if err != nil {
		t.Fatalf("open a second page: %v", err)
	}
	if err := manager.ClosePage(t.Context(), access, first.ID); err != nil {
		t.Fatalf("close the first page: %v", err)
	}
	if got := engine.disposed.Load(); got != 0 {
		t.Fatalf("the workspace profile was disposed with a page still open (%d disposals)", got)
	}
	if err := manager.ClosePage(t.Context(), access, second.ID); err != nil {
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
		if err := m.ClosePage(context.Background(), access, pageID); err != nil {
			t.Errorf("close page: %v", err)
		}
	}},
	{"closed by the engine", func(t *testing.T, m *Manager, access Access, pageID string) {
		m.removeClosedPage(paneHandle(m, access, pageID))
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
			closing := Access{ThreadID: "closing", Workspace: workspace}
			last, err := manager.Open(t.Context(), closing, "https://example.test/", OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			other := Access{ThreadID: "other", Workspace: workspace}
			var opened PageInfo
			var openErr error
			var reported atomic.Bool
			// Either close reports the closing thread's empty state between
			// emptying the workspace and disposing its profile.
			manager.SetEventSink(func(event CompanionEvent) {
				if event.ThreadID == closing.ThreadID && len(event.Pages) == 0 && reported.CompareAndSwap(false, true) {
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
			if got := engine.disposed.Load(); got != 1 {
				t.Fatalf("%d profile disposals, want the emptied profile's one", got)
			}
			if _, err := manager.Open(t.Context(), other, "https://example.test/next", OpenOptions{PageID: opened.ID}); err != nil {
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
			closing := Access{ThreadID: "closing", Workspace: workspace}
			last, err := manager.Open(t.Context(), closing, "https://example.test/", OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			engine.onNewPage = func() error {
				closer.close(t, manager, closing, last.ID)
				return nil
			}
			other := Access{ThreadID: "other", Workspace: workspace}
			opened, err := manager.Open(t.Context(), other, "https://example.test/other", OpenOptions{})
			if err != nil {
				t.Fatalf("open while the workspace's last page closes: %v", err)
			}
			if got := engine.disposed.Load(); got != 0 {
				t.Fatalf("the profile a page was being created in was disposed (%d disposals)", got)
			}
			if _, err := manager.Open(t.Context(), other, "https://example.test/next", OpenOptions{PageID: opened.ID}); err != nil {
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
			closing := Access{ThreadID: "closing", Workspace: workspace}
			last, err := m.Open(t.Context(), closing, "https://example.test/", OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			return func() {
				if err := m.ClosePage(context.Background(), closing, last.ID); err != nil {
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
			_, err := manager.Open(t.Context(), Access{ThreadID: "other", Workspace: workspace}, "https://example.test/other", OpenOptions{})
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
	closing := Access{ThreadID: "closing", Workspace: workspace}
	last, err := manager.Open(t.Context(), closing, "https://example.test/", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	other := Access{ThreadID: "other", Workspace: workspace}
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
	if err := manager.ClosePage(t.Context(), closing, last.ID); err != nil {
		t.Fatalf("close page: %v", err)
	}
	if _, err := manager.Open(t.Context(), other, "https://example.test/next", OpenOptions{PageID: opened.ID}); err != nil {
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

// engineStartedContext runs onStarted once, the first time the caller's
// context is checked with the engine running.
type engineStartedContext struct {
	context.Context
	engine    browserEngine
	onStarted func()
	fired     atomic.Bool
}

func (c *engineStartedContext) Err() error {
	if c.engine.Running() && c.fired.CompareAndSwap(false, true) {
		c.onStarted()
	}
	return c.Context.Err()
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
			return &engineStartedContext{Context: t.Context(), engine: e, onStarted: hook}
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
			access := Access{ThreadID: "thread", Workspace: t.TempDir()}
			page, err := manager.Open(ctx, access, "https://example.test/", OpenOptions{})
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if idleClosed == nil {
				t.Fatal("the page's creation did not reach the hook")
			}
			<-idleClosed
			if _, err := manager.Open(t.Context(), access, "https://example.test/next", OpenOptions{PageID: page.ID}); err != nil {
				t.Fatalf("navigate the page after the idle close: %v", err)
			}
		})
	}
}

// With no page left, the idle close stops the engine.
func TestAnIdleCloseStopsTheEngineWithNoPage(t *testing.T) {
	manager, engine := newDisposeCountingManager(t)
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	page, err := manager.Open(t.Context(), access, "https://example.test/", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	manager.closeIdleBrowser()
	if !engine.Running() {
		t.Fatal("the idle close stopped the engine with a page open")
	}
	if err := manager.ClosePage(t.Context(), access, page.ID); err != nil {
		t.Fatal(err)
	}
	manager.closeIdleBrowser()
	if engine.Running() {
		t.Fatal("the idle close left the engine running with no page")
	}
}

func TestAuthorizeFileStaysWithinGrantedRoots(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "index.html")
	if err := os.WriteFile(inside, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{config: Config{}, scopes: make(map[string]*workspaceScope)}
	access := Access{ThreadID: "t", Workspace: root}
	wantInside, _ := filepath.EvalSymlinks(inside)
	if got, err := manager.authorizeFile(access, inside); err != nil || got != wantInside {
		t.Fatalf("inside = %q, %v", got, err)
	}
	if _, err := manager.authorizeFile(access, outside); err == nil {
		t.Fatal("outside file unexpectedly allowed")
	}
	manager.config.AllowOutsideWorkspace = true
	wantOutside, _ := filepath.EvalSymlinks(outside)
	if got, err := manager.authorizeFile(access, outside); err != nil || got != wantOutside {
		t.Fatalf("outside with grant = %q, %v", got, err)
	}
}

// Clearing site data deletes the AO-owned profile tree (spec §4). There is no
// checkpoint left to clear, so for an engine that keeps its data under that
// tree the directory IS the site data; an engine whose store lives elsewhere
// clears its own through engineSiteData, which needs the real platform.
func TestClearSiteDataRemovesTheProfileTree(t *testing.T) {
	configDir := t.TempDir()
	manager := NewManager(configDir, Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	defer manager.Close()
	if err := os.MkdirAll(filepath.Join(manager.profileDir, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := manager.ClearSiteData(context.Background()); err != nil {
		t.Fatalf("clear site data: %v", err)
	}
	if _, err := os.Stat(manager.profileDir); !os.IsNotExist(err) {
		t.Fatalf("profile tree survives clear: %v", err)
	}
}

// Old encrypted checkpoints are deleted on the first boot of this code (spec
// §4). They were keyed by a per-install secret nothing reads any more, so
// leaving them behind would leave undecryptable cookies on disk forever.
func TestBootPrunesTheEncryptedCheckpointsFromTheDeletedStateStore(t *testing.T) {
	configDir := t.TempDir()
	stateDir := filepath.Join(configDir, "browser-state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "workspace.json"), []byte("ciphertext"), 0o600); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(configDir, "browser-state.key")
	if err := os.WriteFile(keyFile, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(configDir, Config{}, ManagerOptions{})
	defer manager.Close()
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("encrypted checkpoint directory survives boot: %v", err)
	}
	if _, err := os.Stat(keyFile); !os.IsNotExist(err) {
		t.Fatalf("checkpoint key file survives boot: %v", err)
	}
}

// attachHookProfile runs onAttach while the engine attaches a popup.
type attachHookProfile struct {
	*fakeProfile
	onAttach func()
}

func (p *attachHookProfile) AttachPage(ctx context.Context, _ string, hooks pageHooks) (pageDriver, error) {
	p.onAttach()
	return p.fakeProfile.NewPage(ctx, hooks)
}

// A popup whose opener closes while the engine attaches it, as when the
// opener's thread is deleted, is not adopted by that thread.
func TestPopupOfAClosedOpenerIsNotAdopted(t *testing.T) {
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() { manager.Close() })
	workspace := t.TempDir()
	opener := Access{ThreadID: "opener", Workspace: workspace}
	page, err := manager.Open(t.Context(), opener, "https://example.test/", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Another thread's page keeps the workspace profile alive.
	if _, err := manager.Open(t.Context(), Access{ThreadID: "other", Workspace: workspace}, "https://example.test/", OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	openerHandle := paneHandle(manager, opener, page.ID)
	manager.mu.Lock()
	var profile *fakeProfile
	for _, scope := range manager.scopes {
		profile = scope.profile.(*fakeProfile)
		scope.profile = &attachHookProfile{fakeProfile: profile, onAttach: func() {
			if err := manager.CloseThread(context.Background(), opener.ThreadID); err != nil {
				t.Errorf("close the opener's thread: %v", err)
			}
		}}
	}
	manager.mu.Unlock()
	manager.adoptPopup(enginePopup{Profile: profile.Handle(), Opener: openerHandle, Handle: "popup"})
	if pages := manager.ownedPages(opener.ThreadID); len(pages) != 0 {
		t.Fatalf("the closed thread adopted %d pages", len(pages))
	}
}

// closeHookEngine's pages run onClose, once, as the first of them closes. Its
// profiles attach a popup as a new page.
type closeHookEngine struct {
	*fakeEngine
	onClose func(closing string)
	once    sync.Once
	profile *fakeProfile
}

func (e *closeHookEngine) NewProfile(ctx context.Context, opts profileOptions) (engineProfile, error) {
	profile, err := e.fakeEngine.NewProfile(ctx, opts)
	if err != nil {
		return nil, err
	}
	e.profile = profile.(*fakeProfile)
	return &closeHookProfile{fakeProfile: e.profile, engine: e}, nil
}

type closeHookProfile struct {
	*fakeProfile
	engine *closeHookEngine
}

func (p *closeHookProfile) NewPage(ctx context.Context, hooks pageHooks) (pageDriver, error) {
	driver, err := p.fakeProfile.NewPage(ctx, hooks)
	if err != nil {
		return nil, err
	}
	return &closeHookPage{pageDriver: driver, engine: p.engine}, nil
}

func (p *closeHookProfile) AttachPage(ctx context.Context, _ string, hooks pageHooks) (pageDriver, error) {
	return p.fakeProfile.NewPage(ctx, hooks)
}

type closeHookPage struct {
	pageDriver
	engine *closeHookEngine
}

func (p *closeHookPage) Close() {
	p.engine.once.Do(func() { p.engine.onClose(p.Handle()) })
	p.pageDriver.Close()
}

func newCloseHookManager(t *testing.T) (*Manager, *closeHookEngine) {
	t.Helper()
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() { manager.Close() })
	engine := &closeHookEngine{fakeEngine: newFakeEngine()}
	manager.engine = engine
	return manager, engine
}

// A popup its opener opens while the thread's close is closing that opener
// is closed with the thread.
func TestCloseThreadClosesAPopupAdoptedWhileItRuns(t *testing.T) {
	manager, engine := newCloseHookManager(t)
	opener := Access{ThreadID: "opener", Workspace: t.TempDir()}
	page, err := manager.Open(t.Context(), opener, "https://example.test/", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	openerHandle := paneHandle(manager, opener, page.ID)
	engine.onClose = func(string) {
		manager.adoptPopup(enginePopup{Profile: engine.profile.Handle(), Opener: openerHandle, Handle: "popup"})
	}
	if err := manager.CloseThread(context.Background(), opener.ThreadID); err != nil {
		t.Fatalf("CloseThread: %v", err)
	}
	if pages := manager.ownedPages(opener.ThreadID); len(pages) != 0 {
		t.Fatalf("the closed thread still owns %d page(s)", len(pages))
	}
}

// A page the engine closes by itself while the thread's close runs counts as
// closed rather than failing the close.
func TestCloseThreadCountsAPageTheEngineClosedAsClosed(t *testing.T) {
	manager, engine := newCloseHookManager(t)
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	var handles []string
	for range 2 {
		page, err := manager.Open(t.Context(), access, "https://example.test/", OpenOptions{})
		if err != nil {
			t.Fatal(err)
		}
		handles = append(handles, paneHandle(manager, access, page.ID))
	}
	engine.onClose = func(closing string) {
		for _, handle := range handles {
			if handle != closing {
				manager.removeClosedPage(handle)
			}
		}
	}
	if err := manager.CloseThread(context.Background(), access.ThreadID); err != nil {
		t.Fatalf("CloseThread: %v", err)
	}
	if pages := manager.ownedPages(access.ThreadID); len(pages) != 0 {
		t.Fatalf("the closed thread still owns %d page(s)", len(pages))
	}
}

// A page closed either way stops being its thread's active page: another of
// its pages takes over, and with none left the companion hides.
func TestAClosedPageIsNoLongerTheActivePage(t *testing.T) {
	for _, closer := range lastPageClosers {
		t.Run(closer.name, func(t *testing.T) {
			manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
			t.Cleanup(func() { manager.Close() })
			access := Access{ThreadID: "thread", Workspace: t.TempDir()}
			first, err := manager.Open(t.Context(), access, "https://example.test/one", OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			second, err := manager.Open(t.Context(), access, "https://example.test/two", OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			show := true
			if _, err := manager.Visibility(t.Context(), access, &show, second.ID); err != nil {
				t.Fatal(err)
			}
			closer.close(t, manager, access, second.ID)
			if state := manager.threadState(access.ThreadID); state.ActivePageID != first.ID || !*state.Visible {
				t.Fatalf("after closing the active page: active %q visible %v, want %q visible", state.ActivePageID, *state.Visible, first.ID)
			}
			closer.close(t, manager, access, first.ID)
			if state := manager.threadState(access.ThreadID); state.ActivePageID != "" || *state.Visible {
				t.Fatalf("after closing the last page: active %q visible %v, want none hidden", state.ActivePageID, *state.Visible)
			}
		})
	}
}

// discardRecordingEngine records the pages the Manager discards.
type discardRecordingEngine struct {
	*fakeEngine
	mu        sync.Mutex
	discarded []string
}

func (e *discardRecordingEngine) DiscardPage(handle string) {
	e.mu.Lock()
	e.discarded = append(e.discarded, handle)
	e.mu.Unlock()
}

// A popup its profile fails to attach is discarded, not left open with no
// page to close it. The fake profile fails every attach.
func TestAPopupThatFailsToAttachIsDiscarded(t *testing.T) {
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() { manager.Close() })
	engine := &discardRecordingEngine{fakeEngine: newFakeEngine()}
	manager.engine = engine
	opener := Access{ThreadID: "thread", Workspace: t.TempDir()}
	page, err := manager.Open(t.Context(), opener, "https://example.test/", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	var profile string
	for _, scope := range manager.scopes {
		profile = scope.profile.Handle()
	}
	manager.mu.Unlock()
	manager.adoptPopup(enginePopup{Profile: profile, Opener: paneHandle(manager, opener, page.ID), Handle: "popup"})
	engine.mu.Lock()
	discarded := slices.Clone(engine.discarded)
	engine.mu.Unlock()
	if !slices.Equal(discarded, []string{"popup"}) {
		t.Fatalf("discarded %v, want [popup]", discarded)
	}
	if pages := manager.ownedPages(opener.ThreadID); len(pages) != 1 {
		t.Fatalf("the thread owns %d pages, want its opener only", len(pages))
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
			access := Access{ThreadID: "thread", Workspace: t.TempDir()}
			if openedBefore {
				page, err := manager.Open(t.Context(), access, "https://example.test/", OpenOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if err := manager.ClosePage(t.Context(), access, page.ID); err != nil {
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

// policyRecordingEngine keeps the options of the last profile it created.
type policyRecordingEngine struct {
	*fakeEngine
	mu   sync.Mutex
	opts profileOptions
}

func (e *policyRecordingEngine) NewProfile(ctx context.Context, opts profileOptions) (engineProfile, error) {
	e.mu.Lock()
	e.opts = opts
	e.mu.Unlock()
	return e.fakeEngine.NewProfile(ctx, opts)
}

// A profile carries its workspace's navigation policy, which an engine
// applies to a page it opened by itself before the Manager adopts it: the
// roots of the thread that created the profile, and the outside-workspace
// setting as it stands when a request is decided.
func TestAProfileCarriesItsWorkspacesNavigationPolicy(t *testing.T) {
	workspace, project, outside := t.TempDir(), t.TempDir(), t.TempDir()
	files := map[string]string{
		"workspace": filepath.Join(workspace, "index.html"),
		"project":   filepath.Join(project, "readme.html"),
		"outside":   filepath.Join(outside, "secret.txt"),
	}
	for _, path := range files {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() { manager.Close() })
	engine := &policyRecordingEngine{fakeEngine: newFakeEngine()}
	manager.engine = engine
	if _, err := manager.NewPage(t.Context(), Access{ThreadID: "thread", Workspace: workspace, ProjectRoot: project}); err != nil {
		t.Fatal(err)
	}
	engine.mu.Lock()
	allow := engine.opts.Allow
	engine.mu.Unlock()
	if allow == nil {
		t.Fatal("the profile was created without a navigation policy")
	}
	for url, want := range map[string]bool{
		"file://" + files["workspace"]: true,
		"file://" + files["project"]:   true,
		"file://" + files["outside"]:   false,
		"https://example.test/":        true,
		"chrome://settings/":           false,
	} {
		if got := allow(url); got != want {
			t.Errorf("the profile's policy answers %v for %s, want %v", got, url, want)
		}
	}
	manager.mu.Lock()
	manager.config.AllowOutsideWorkspace = true
	manager.mu.Unlock()
	if !allow("file://" + files["outside"]) {
		t.Error("the profile's policy ignores the outside-workspace setting")
	}
}

// The Manager keeps a download only when a managed page owns the frame it
// began in and the workspace has quota for it. It answers false for every
// other one, which the engine then cancels.
func TestTheManagerKeepsOnlyADownloadAPageOwnsWithinQuota(t *testing.T) {
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() { manager.Close() })
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	page, err := manager.NewPage(t.Context(), access)
	if err != nil {
		t.Fatal(err)
	}
	frame := paneHandle(manager, access, page.ID)

	if manager.downloadStarted(downloadStart{Frame: "unowned", ID: "G-unowned"}) {
		t.Fatal("a download no managed page owns was kept")
	}
	if !manager.downloadStarted(downloadStart{Frame: frame, ID: "G-kept", SuggestedName: "a.bin"}) {
		t.Fatal("a download in a managed page within quota was refused")
	}
	manager.mu.Lock()
	for _, scope := range manager.scopes {
		scope.downloadBytes.Store(maxWorkspaceDownloadBytes)
	}
	manager.mu.Unlock()
	if manager.downloadStarted(downloadStart{Frame: frame, ID: "G-over"}) {
		t.Fatal("a download over the workspace quota was kept")
	}

	listed, err := manager.Downloads(t.Context(), access, DownloadOptions{PageID: page.ID})
	if err != nil {
		t.Fatal(err)
	}
	var states []string
	for _, download := range listed.([]DownloadInfo) {
		states = append(states, download.ID+" "+download.State)
	}
	if want := []string{"G-kept in_progress", "G-over canceled"}; !slices.Equal(states, want) {
		t.Fatalf("the page lists %q, want %q", states, want)
	}
}
