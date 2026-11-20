package browser

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/browser/ids"
)

// jarEngine is the fake engine recording every profile it creates with the
// options it was created with, and counting its own site-data clears. A
// navigation to an address in navErr fails with that error; navGate, when
// set, holds every navigation, signalling navigating first. newPageGate
// holds every page creation the same way, signalling creating.
type jarEngine struct {
	*fakeEngine
	mu          sync.Mutex
	profiles    []*jarProfile
	navErr      map[string]error
	navGate     chan struct{}
	navigating  chan string
	newPageGate chan struct{}
	creating    chan struct{}
	onAttach    func()
	closeGate   chan struct{}
	closeStep   chan struct{}
	profileGate chan struct{}
	profiling   chan struct{}
	// frames maps a subframe to the handle of the page that reports it.
	frames map[string]string
	// closeHolds holds the close of one page, by handle (holdCloseOf).
	closeHolds  map[string]chan struct{}
	disposeGate chan struct{}
	// lastPage is the handle of the page NewPage created last.
	lastPage ids.PageHandle
	// afterNewPage, when set, runs once with the next created page's handle
	// before NewPage returns.
	afterNewPage func(ids.PageHandle)
	// failNewPage, when set, fails the next page creation once its gate
	// opens, before the engine makes a page.
	failNewPage error
	// startErr, when set, fails every engine start.
	startErr error
	cleared  atomic.Int64
	// clearGate, when set, holds the engine's site-data clear, signalling
	// clearing first; profileInClear records a profile created during it.
	clearGate      chan struct{}
	clearing       chan struct{}
	inClear        atomic.Bool
	profileInClear atomic.Bool
}

type jarProfile struct {
	*fakeProfile
	engine   *jarEngine
	disposed atomic.Bool
}

type jarPage struct {
	pageDriver
	engine *jarEngine
}

func newJarManager(t *testing.T, dir string, config Config) (*Manager, *jarEngine) {
	t.Helper()
	manager := NewManager(dir, config, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() { _ = manager.Close() })
	checkSelectionAtEnd(t, manager)
	// Downloads AO keeps, whose directories and quotas are per workspace.
	fake := newFakeEngine()
	fake.saving = downloadsAsArtifacts
	engine := &jarEngine{
		fakeEngine: fake, navErr: make(map[string]error),
		navigating: make(chan string, 16), creating: make(chan struct{}, 16), profiling: make(chan struct{}, 16),
	}
	manager.engine = engine
	return manager, engine
}

func (e *jarEngine) NewProfile(ctx context.Context, opts profileOptions) (engineProfile, error) {
	e.mu.Lock()
	gate := e.profileGate
	e.mu.Unlock()
	if gate != nil {
		e.profiling <- struct{}{}
		<-gate
	}
	if e.inClear.Load() {
		e.profileInClear.Store(true)
	}
	profile, err := e.fakeEngine.NewProfile(ctx, opts)
	if err != nil {
		return nil, err
	}
	p := &jarProfile{fakeProfile: profile.(*fakeProfile), engine: e}
	e.mu.Lock()
	e.profiles = append(e.profiles, p)
	e.mu.Unlock()
	return p, nil
}

func (e *jarEngine) Start(ctx context.Context) error {
	e.mu.Lock()
	err := e.startErr
	e.mu.Unlock()
	if err != nil {
		return err
	}
	return e.fakeEngine.Start(ctx)
}

func (e *jarEngine) ClearSiteData(context.Context) error {
	e.inClear.Store(true)
	defer e.inClear.Store(false)
	e.mu.Lock()
	gate := e.clearGate
	e.mu.Unlock()
	if gate != nil {
		e.clearing <- struct{}{}
		<-gate
	}
	e.cleared.Add(1)
	return nil
}

// created answers the jar identities of the profiles created so far, in order.
func (e *jarEngine) created() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.profiles))
	for _, p := range e.profiles {
		out = append(out, string(p.jar))
	}
	return out
}

func (e *jarEngine) profile(jar string) *jarProfile {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := len(e.profiles) - 1; i >= 0; i-- {
		if e.profiles[i].jar == jarKey(jar) {
			return e.profiles[i]
		}
	}
	return nil
}

func (e *jarEngine) holdNav() (release func()) {
	gate := make(chan struct{})
	e.mu.Lock()
	e.navGate = gate
	e.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			e.mu.Lock()
			e.navGate = nil
			e.mu.Unlock()
			close(gate)
		})
	}
}

func (e *jarEngine) holdNewPage() (release func()) {
	gate := make(chan struct{})
	e.mu.Lock()
	e.newPageGate = gate
	e.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			e.mu.Lock()
			e.newPageGate = nil
			e.mu.Unlock()
			close(gate)
		})
	}
}

func (p *jarProfile) NewPage(ctx context.Context, hooks pageHooks) (pageDriver, error) {
	p.engine.mu.Lock()
	gate := p.engine.newPageGate
	p.engine.mu.Unlock()
	if gate != nil {
		p.engine.creating <- struct{}{}
		<-gate
	}
	p.engine.mu.Lock()
	failure := p.engine.failNewPage
	p.engine.failNewPage = nil
	p.engine.mu.Unlock()
	if failure != nil {
		return nil, failure
	}
	driver, err := p.fakeProfile.NewPage(ctx, hooks)
	if err != nil {
		return nil, err
	}
	p.engine.mu.Lock()
	p.engine.lastPage = driver.Handle()
	after := p.engine.afterNewPage
	p.engine.afterNewPage = nil
	p.engine.mu.Unlock()
	if after != nil {
		after(driver.Handle())
	}
	return &jarPage{pageDriver: driver, engine: p.engine}, nil
}

func (p *jarProfile) AttachPage(ctx context.Context, _ ids.PageHandle, hooks pageHooks) (pageDriver, error) {
	p.engine.mu.Lock()
	hook := p.engine.onAttach
	p.engine.mu.Unlock()
	if hook != nil {
		hook()
	}
	driver, err := p.fakeProfile.NewPage(ctx, hooks)
	if err != nil {
		return nil, err
	}
	return &jarPage{pageDriver: driver, engine: p.engine}, nil
}

func (p *jarProfile) Dispose(ctx context.Context) error {
	p.engine.mu.Lock()
	gate := p.engine.disposeGate
	p.engine.mu.Unlock()
	if gate != nil {
		<-gate
	}
	p.disposed.Store(true)
	return p.fakeProfile.Dispose(ctx)
}

// holdDispose holds every profile disposal until release.
func (e *jarEngine) holdDispose(t *testing.T) (release func()) {
	gate := make(chan struct{})
	e.mu.Lock()
	e.disposeGate = gate
	e.mu.Unlock()
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return release
}

func (p *jarPage) OwnsFrame(frame frameID) bool {
	p.engine.mu.Lock()
	owner, ok := p.engine.frames[string(frame)]
	p.engine.mu.Unlock()
	return (ok && ids.PageHandle(owner) == p.Handle()) || p.pageDriver.OwnsFrame(frame)
}

func (p *jarPage) Close() {
	p.engine.mu.Lock()
	gate, step, own := p.engine.closeGate, p.engine.closeStep, p.engine.closeHolds[string(p.Handle())]
	p.engine.mu.Unlock()
	switch {
	case gate != nil:
		<-gate
	case step != nil:
		<-step
	case own != nil:
		<-own
	}
	p.pageDriver.Close()
}

// holdCloseOf holds the close of the page with handle alone until release.
func (e *jarEngine) holdCloseOf(t *testing.T, handle string) (release func()) {
	gate := make(chan struct{})
	e.mu.Lock()
	if e.closeHolds == nil {
		e.closeHolds = make(map[string]chan struct{})
	}
	e.closeHolds[handle] = gate
	e.mu.Unlock()
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return release
}

// stepCloses holds every page close until step lets one through; at the
// test's end every close passes.
func (e *jarEngine) stepCloses(t *testing.T) (step func()) {
	ch := make(chan struct{})
	e.mu.Lock()
	e.closeStep = ch
	e.mu.Unlock()
	t.Cleanup(func() {
		e.mu.Lock()
		e.closeStep = nil
		e.mu.Unlock()
		// Lets every close still waiting for a step through.
		close(ch)
	})
	return func() { ch <- struct{}{} }
}

func (e *jarEngine) holdClose() (release func()) {
	gate := make(chan struct{})
	e.mu.Lock()
	e.closeGate = gate
	e.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			e.mu.Lock()
			e.closeGate = nil
			e.mu.Unlock()
			close(gate)
		})
	}
}

func (p *jarPage) Navigate(ctx context.Context, rawURL string) error {
	p.engine.mu.Lock()
	gate, failure := p.engine.navGate, p.engine.navErr[rawURL]
	p.engine.mu.Unlock()
	if gate != nil {
		p.engine.navigating <- rawURL
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if failure != nil {
		return failure
	}
	return p.pageDriver.Navigate(ctx, rawURL)
}

// pageJar answers the jar identity of the scope holding a live page.
func pageJar(t *testing.T, m *Manager, pageID string) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	_, scope := m.livePageLocked(PageID(pageID))
	if scope == nil {
		t.Fatalf("page %s is not live", pageID)
	}
	return string(scope.jar.key)
}

// project is a project root with two linked worktrees beside it, and a
// thread access maker for any of them.
type project struct {
	root, one, two string
	id             string
}

func newProject(t *testing.T, id string) project {
	t.Helper()
	base := t.TempDir()
	p := project{root: filepath.Join(base, "root"), one: filepath.Join(base, "one"), two: filepath.Join(base, "two"), id: id}
	for _, dir := range []string{p.root, p.one, p.two} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func (p project) access(m *Manager, thread, workspace string) Access {
	return m.Bind(Caller{ThreadID: ThreadID(thread), Workspace: workspace, ProjectRoot: p.root, ProjectID: p.id})
}

func canonical(t *testing.T, path string) string {
	t.Helper()
	resolved, err := canonicalRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func setJar(t *testing.T, m *Manager, access Access, kind string) {
	t.Helper()
	if err := m.SetJar(context.Background(), access, kind, JarSwitchConfirmed); err != nil {
		t.Fatalf("set jar %s for %s: %v", kind, access.ThreadID(), err)
	}
}

// restoreTabs restores every suspended tab of the thread, as using each
// would, so the tabs a jar switch suspended are live in the new jar.
func restoreTabs(t *testing.T, m *Manager, access Access) {
	t.Helper()
	for _, page := range m.CompanionState(access.Caller()).Pages {
		if page.Suspended {
			if err := m.ActivateCompanionPage(t.Context(), access, PageID(page.ID)); err != nil {
				t.Fatalf("restore %s: %v", page.ID, err)
			}
		}
	}
}
