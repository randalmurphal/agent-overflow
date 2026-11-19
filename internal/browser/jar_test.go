package browser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
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
	lastPage string
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
		out = append(out, p.jar)
	}
	return out
}

func (e *jarEngine) profile(jar string) *jarProfile {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := len(e.profiles) - 1; i >= 0; i-- {
		if e.profiles[i].jar == jar {
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
	driver, err := p.fakeProfile.NewPage(ctx, hooks)
	if err != nil {
		return nil, err
	}
	p.engine.mu.Lock()
	p.engine.lastPage = driver.Handle()
	p.engine.mu.Unlock()
	return &jarPage{pageDriver: driver, engine: p.engine}, nil
}

func (p *jarProfile) AttachPage(ctx context.Context, _ string, hooks pageHooks) (pageDriver, error) {
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

func (p *jarPage) OwnsFrame(frame string) bool {
	p.engine.mu.Lock()
	owner, ok := p.engine.frames[frame]
	p.engine.mu.Unlock()
	return (ok && owner == p.Handle()) || p.pageDriver.OwnsFrame(frame)
}

func (p *jarPage) Close() {
	p.engine.mu.Lock()
	gate, step, own := p.engine.closeGate, p.engine.closeStep, p.engine.closeHolds[p.Handle()]
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
	_, scope := m.livePageLocked(pageID)
	if scope == nil {
		t.Fatalf("page %s is not live", pageID)
	}
	return scope.jar.key
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

func (p project) access(thread, workspace string) Access {
	return Access{ThreadID: thread, Workspace: workspace, ProjectRoot: p.root, ProjectID: p.id}
}

func canonical(t *testing.T, path string) string {
	t.Helper()
	resolved, err := canonicalRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func openPage(t *testing.T, m *Manager, access Access, rawURL string) PageInfo {
	t.Helper()
	info, err := m.Open(t.Context(), access, rawURL, OpenOptions{})
	if err != nil {
		t.Fatalf("open %s for %s: %v", rawURL, access.ThreadID, err)
	}
	return info
}

func setJar(t *testing.T, m *Manager, access Access, kind string) {
	t.Helper()
	if err := m.SetJar(context.Background(), access, kind); err != nil {
		t.Fatalf("set jar %s for %s: %v", kind, access.ThreadID, err)
	}
}

// The default jar is exactly what every earlier build used: the engine is
// handed the canonical workspace path as its identity, persistent, so each
// engine derives the profile directory, WebView2 profile name and WebKit
// store it derived before, and downloads land in the same directory.
func TestWorktreeJarIsTodaysWorkspaceProfile(t *testing.T) {
	dir := t.TempDir()
	manager, engine := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access("thread", proj.one)
	openPage(t, manager, access, "https://example.test/")
	workspace := canonical(t, proj.one)

	if got := engine.created(); len(got) != 1 || got[0] != workspace {
		t.Fatalf("profiles created for %q, want the workspace path %q", got, workspace)
	}
	profile := engine.profile(workspace)
	if profile.ephemeral {
		t.Fatal("the default jar is ephemeral with site data persisted")
	}
	digest := sha256.Sum256([]byte(workspace))
	if got, want := jarDirName(workspace), fmt.Sprintf("%x", digest[:12]); got != want {
		t.Fatalf("profile directory name = %q, want today's %q", got, want)
	}
	manager.mu.Lock()
	scope := manager.scopes[workspace]
	manager.mu.Unlock()
	if want := filepath.Join(dir, "browser-artifacts", "downloading", fmt.Sprintf("%x", digest[:12])); scope.downloadDir != want {
		t.Fatalf("download directory = %q, want %q", scope.downloadDir, want)
	}
	if state := manager.CompanionState(access); state.Jar != JarWorktree {
		t.Fatalf("state names jar %q, want worktree", state.Jar)
	}
}

// A project jar is one profile for every thread of the project that chose it,
// across worktrees; a thread that did not choose it keeps its worktree's.
func TestProjectJarIsSharedAcrossWorktreesOnlyByThreadsThatChoseIt(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	a, b, c := proj.access("a", proj.one), proj.access("b", proj.two), proj.access("c", proj.two)

	pageA := openPage(t, manager, a, "https://example.test/a")
	pageB := openPage(t, manager, b, "https://example.test/b")
	if pageJar(t, manager, pageA.ID) == pageJar(t, manager, pageB.ID) {
		t.Fatal("two worktrees share a jar with the default setting")
	}
	setJar(t, manager, a, JarProject)
	setJar(t, manager, b, JarProject)
	restoreTabs(t, manager, a)
	restoreTabs(t, manager, b)
	pageC := openPage(t, manager, c, "https://example.test/c")

	jarA, jarB := pageJar(t, manager, tabIDs(manager, a)[0]), pageJar(t, manager, tabIDs(manager, b)[0])
	if jarA != "project:P" || jarB != jarA {
		t.Fatalf("project jars = %q and %q, want both project:P", jarA, jarB)
	}
	if got := pageJar(t, manager, pageC.ID); got != canonical(t, proj.two) {
		t.Fatalf("a thread that kept the default opened in %q, want its worktree's jar", got)
	}
	shared := 0
	for _, jar := range engine.created() {
		if jar == "project:P" {
			shared++
		}
	}
	if shared != 1 {
		t.Fatalf("%d profiles created for the project jar, want one shared", shared)
	}
	other := newProject(t, "Q")
	d := other.access("d", other.one)
	setJar(t, manager, d, JarProject)
	if got := pageJar(t, manager, openPage(t, manager, d, "https://example.test/d").ID); got != "project:Q" {
		t.Fatalf("another project's thread opened in %q, want its own project jar", got)
	}
}

// A thread jar isolates two threads on one worktree from each other and from
// the worktree's shared jar.
func TestThreadJarIsolatesThreadsOnOneWorktree(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	a, b, c := proj.access("a", proj.one), proj.access("b", proj.one), proj.access("c", proj.one)
	setJar(t, manager, a, JarThread)
	setJar(t, manager, b, JarThread)
	jarA := pageJar(t, manager, openPage(t, manager, a, "https://example.test/").ID)
	jarB := pageJar(t, manager, openPage(t, manager, b, "https://example.test/").ID)
	jarC := pageJar(t, manager, openPage(t, manager, c, "https://example.test/").ID)
	if jarA != "thread:a" || jarB != "thread:b" || jarC != canonical(t, proj.one) {
		t.Fatalf("jars = %q, %q, %q; want each thread's own and the worktree's", jarA, jarB, jarC)
	}
	if engine.profile("thread:a").ephemeral {
		t.Fatal("a thread jar is ephemeral with site data persisted")
	}
}

// A private jar is never persistent, its pages share it while one is open,
// and its identity is new for every generation of the setting and once its
// last page has closed.
func TestPrivateJarIsEphemeralAndNewEachGeneration(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access("a", proj.one)
	setJar(t, manager, access, JarPrivate)
	first := openPage(t, manager, access, "https://example.test/1")
	second := openPage(t, manager, access, "https://example.test/2")
	jar := pageJar(t, manager, first.ID)
	if !strings.HasPrefix(jar, "private:a:") || pageJar(t, manager, second.ID) != jar {
		t.Fatalf("private pages opened in %q and %q, want one private jar", jar, pageJar(t, manager, second.ID))
	}
	if !engine.profile(jar).ephemeral {
		t.Fatal("a private jar was persistent")
	}
	for _, page := range []PageInfo{first, second} {
		if err := manager.ClosePage(t.Context(), access, page.ID); err != nil {
			t.Fatal(err)
		}
	}
	if !engine.profile(jar).disposed.Load() {
		t.Fatal("the private jar outlived its last page")
	}
	reopened := pageJar(t, manager, openPage(t, manager, access, "https://example.test/3").ID)
	if reopened == jar || !strings.HasPrefix(reopened, "private:a:") {
		t.Fatalf("a private page after the last one closed opened in %q, want a new private jar", reopened)
	}
	setJar(t, manager, access, JarThread)
	restoreTabs(t, manager, access)
	setJar(t, manager, access, JarPrivate)
	restoreTabs(t, manager, access)
	again := pageJar(t, manager, tabIDs(manager, access)[0])
	if again == reopened || again == jar || !strings.HasPrefix(again, "private:a:") {
		t.Fatalf("a new private generation opened in %q, want an identity it never had", again)
	}
}

// A shared jar shares storage, never authority: each page keeps its own
// workspace's file permissions and download quota, and the jar's own policy
// answers a popup not yet adopted with its opener's.
func TestASharedJarKeepsEachWorkspacesAuthority(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	files := map[string]string{}
	for name, dir := range map[string]string{"root": proj.root, "one": proj.one, "two": proj.two} {
		path := filepath.Join(dir, name+".html")
		if err := os.WriteFile(path, []byte("<title>"+name+"</title>"), 0o600); err != nil {
			t.Fatal(err)
		}
		files[name] = canonical(t, path)
	}
	a, b := proj.access("a", proj.one), proj.access("b", proj.two)
	setJar(t, manager, a, JarProject)
	setJar(t, manager, b, JarProject)
	pageA := openPage(t, manager, a, "https://example.test/a")
	pageB := openPage(t, manager, b, "https://example.test/b")
	if pageJar(t, manager, pageA.ID) != pageJar(t, manager, pageB.ID) {
		t.Fatal("the two worktrees do not share the project jar")
	}

	if _, err := manager.OpenFile(t.Context(), a, files["two"], OpenOptions{PageID: pageA.ID}); err == nil {
		t.Fatal("a page opened a file of the other worktree sharing its jar")
	}
	if _, err := manager.OpenFile(t.Context(), a, files["one"], OpenOptions{PageID: pageA.ID}); err != nil {
		t.Fatalf("a page could not open its own worktree's file: %v", err)
	}
	livePage, _, err := manager.lookupOwnedPage(b, pageB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if manager.navigationAllowed(livePage.access, "file://"+files["one"]) {
		t.Fatal("the other worktree's page may navigate to this worktree's file")
	}
	// The jar's own policy, which an engine asks for a popup it loads before
	// adoption, answers with the opener's authority, so the popup may open
	// what it may open once adopted: its worktree's files, not the other's.
	allow := resolvedPolicy(t, engine.profile("project:P").allow)
	openerA, openerB := paneHandle(manager, a, pageA.ID), paneHandle(manager, b, pageB.ID)
	if !allow([]string{"popup", openerA}, "file://"+files["one"]) || !allow([]string{"popup", openerB}, "file://"+files["two"]) {
		t.Fatal("a popup in the shared jar is refused its opener's worktree file")
	}
	if allow([]string{"popup", openerA}, "file://"+files["two"]) || allow([]string{"popup", openerB}, "file://"+files["one"]) {
		t.Fatal("a popup in the shared jar may open the other worktree's file")
	}
	if !allow([]string{openerA}, "file://"+files["one"]) || allow([]string{openerA}, "file://"+files["two"]) {
		t.Fatal("a page's own request is not answered with its own authority")
	}

	manager.mu.Lock()
	manager.authorities[canonical(t, proj.one)].downloadBytes.Store(maxWorkspaceDownloadBytes)
	manager.mu.Unlock()
	if manager.downloadStarted(downloadStart{Frame: paneHandle(manager, a, pageA.ID), ID: "over"}) {
		t.Fatal("a download over its workspace's quota was kept")
	}
	if !manager.downloadStarted(downloadStart{Frame: paneHandle(manager, b, pageB.ID), ID: "kept"}) {
		t.Fatal("the other workspace's download was charged to the exhausted workspace")
	}
	manager.mu.Lock()
	dirOne, dirTwo := manager.authorities[canonical(t, proj.one)].downloadDir, manager.authorities[canonical(t, proj.two)].downloadDir
	manager.mu.Unlock()
	if dirOne == dirTwo {
		t.Fatal("two workspaces sharing a jar share a download directory")
	}

	// A workspace's authority lives while it has a page in any jar.
	if err := manager.ClosePage(t.Context(), a, pageA.ID); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	_, kept := manager.authorities[canonical(t, proj.one)]
	_, other := manager.authorities[canonical(t, proj.two)]
	manager.mu.Unlock()
	if kept || !other {
		t.Fatalf("authorities after the first worktree's last page closed: one=%v two=%v, want only two", kept, other)
	}
}

// The caps bound engine resources as before: a profile is a jar, so the
// context cap counts jars; the workspace cap counts a workspace's pages in
// every jar; and a jar holds no more pages than one workspace may.
func TestCapsBoundJarsWorkspacesAndSharedJars(t *testing.T) {
	t.Run("profiles", func(t *testing.T) {
		manager, _ := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
		proj := newProject(t, "P")
		for i := range maxWorkspaceContexts {
			access := proj.access(fmt.Sprintf("t%d", i), proj.one)
			setJar(t, manager, access, JarThread)
			openPage(t, manager, access, "https://example.test/")
		}
		access := proj.access("last", proj.one)
		setJar(t, manager, access, JarThread)
		if _, err := manager.NewPage(t.Context(), access); err == nil || !strings.Contains(err.Error(), "browser profile limit reached") {
			t.Fatalf("a thirteenth jar answered %v, want the profile limit", err)
		}
	})
	t.Run("workspace", func(t *testing.T) {
		manager, _ := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
		proj := newProject(t, "P")
		for i := range maxPagesPerWorkspace / maxPagesPerThread {
			access := proj.access(fmt.Sprintf("t%d", i), proj.one)
			setJar(t, manager, access, JarThread)
			for range maxPagesPerThread {
				openPage(t, manager, access, "https://example.test/")
			}
		}
		access := proj.access("last", proj.one)
		setJar(t, manager, access, JarThread)
		if _, err := manager.NewPage(t.Context(), access); err == nil || !strings.Contains(err.Error(), "page limit reached for workspace") {
			t.Fatalf("a page past the workspace's cap in another jar answered %v, want the workspace limit", err)
		}
	})
	t.Run("shared jar", func(t *testing.T) {
		manager, _ := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
		proj := newProject(t, "P")
		threads := []Access{proj.access("a", proj.one), proj.access("b", proj.one), proj.access("c", proj.two)}
		for _, access := range threads {
			setJar(t, manager, access, JarProject)
			for range maxPagesPerThread {
				openPage(t, manager, access, "https://example.test/")
			}
		}
		access := proj.access("d", proj.two)
		setJar(t, manager, access, JarProject)
		if _, err := manager.NewPage(t.Context(), access); err == nil || !strings.Contains(err.Error(), "page limit reached for the cookie jar") {
			t.Fatalf("a page past the shared jar's cap answered %v, want the jar limit", err)
		}
	})
}

// Clear site data closes every page in every kind of jar, disposes every
// profile, deletes the profile tree and has the engine clear its own store.
// The threads keep their jar settings.
func TestClearSiteDataClearsEveryJarKind(t *testing.T) {
	dir := t.TempDir()
	manager, engine := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	kinds := []string{JarWorktree, JarProject, JarThread, JarPrivate}
	for i, kind := range kinds {
		access := proj.access(fmt.Sprintf("t%d", i), proj.one)
		setJar(t, manager, access, kind)
		openPage(t, manager, access, "https://example.test/")
	}
	if err := os.MkdirAll(filepath.Join(dir, browserProfileDir, "leftover"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := manager.ClearSiteData(t.Context()); err != nil {
		t.Fatalf("clear site data: %v", err)
	}
	engine.mu.Lock()
	for _, profile := range engine.profiles {
		if !profile.disposed.Load() {
			t.Errorf("the %s jar's profile survived the clear", profile.jar)
		}
	}
	engine.mu.Unlock()
	if _, err := os.Stat(filepath.Join(dir, browserProfileDir)); !os.IsNotExist(err) {
		t.Fatalf("the profile tree survived the clear: %v", err)
	}
	if engine.cleared.Load() != 1 {
		t.Fatalf("the engine cleared its own store %d times, want once", engine.cleared.Load())
	}
	for i, kind := range kinds {
		access := proj.access(fmt.Sprintf("t%d", i), proj.one)
		state := manager.CompanionState(access)
		if len(state.Pages) != 0 || state.Jar != kind {
			t.Fatalf("thread %d after the clear: %d pages, jar %q; want none and %q", i, len(state.Pages), state.Jar, kind)
		}
	}
	restarted, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	if got := restarted.CompanionState(proj.access("t2", proj.one)).Jar; got != JarThread {
		t.Fatalf("a restart after the clear reads jar %q, want the thread's own", got)
	}
}

// With site data off every jar is in memory only, and a jar setting changes
// nothing about that.
func TestPersistOffKeepsEveryJarEphemeral(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: false})
	proj := newProject(t, "P")
	for i, kind := range []string{JarWorktree, JarProject, JarThread, JarPrivate} {
		access := proj.access(fmt.Sprintf("t%d", i), proj.one)
		setJar(t, manager, access, kind)
		openPage(t, manager, access, "https://example.test/")
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.profiles) != 4 {
		t.Fatalf("%d profiles, want 4", len(engine.profiles))
	}
	for _, profile := range engine.profiles {
		if !profile.ephemeral {
			t.Errorf("the %s jar is persistent with site data off", profile.jar)
		}
	}
}

// Turning the browser off forgets every thread's jar setting with its saved
// pages; changing the site-data setting keeps them.
func TestTurningTheBrowserOffForgetsJarSettings(t *testing.T) {
	dir := t.TempDir()
	manager, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access("a", proj.one)
	setJar(t, manager, access, JarThread)
	if err := manager.Reconfigure(Config{Enabled: true, PersistSiteData: false}); err != nil {
		t.Fatal(err)
	}
	if got := manager.CompanionState(access).Jar; got != JarThread {
		t.Fatalf("after the site-data change the jar is %q, want thread", got)
	}
	if _, err := os.Stat(manager.recordPath("a")); err != nil {
		t.Fatalf("the jar setting's saved copy went with the site-data change: %v", err)
	}
	if err := manager.Reconfigure(Config{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if got := manager.CompanionState(access).Jar; got != JarWorktree {
		t.Fatalf("after the browser was turned off the jar is %q, want worktree", got)
	}
	if _, err := os.Stat(manager.recordPath("a")); !os.IsNotExist(err) {
		t.Fatalf("the jar setting's saved copy survived the browser being turned off: %v", err)
	}
}

// The jar setting is saved per thread whether or not it has a tab, survives a
// restart, and a thread back on the default jar with no tab keeps no file.
// A thread's saved pages restore into the jar saved with them.
func TestJarSettingSurvivesARestart(t *testing.T) {
	t.Run("without tabs", func(t *testing.T) {
		dir := t.TempDir()
		manager, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
		proj := newProject(t, "P")
		access := proj.access("a", proj.one)
		setJar(t, manager, access, JarThread)
		if err := manager.Close(); err != nil {
			t.Fatal(err)
		}
		restarted, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
		if got := restarted.CompanionState(access).Jar; got != JarThread {
			t.Fatalf("after a restart the jar is %q, want thread", got)
		}
		if got := pageJar(t, restarted, openPage(t, restarted, access, "https://example.test/").ID); got != "thread:a" {
			t.Fatalf("a page after the restart opened in %q, want the thread's jar", got)
		}
		page := tabIDs(restarted, access)[0]
		if err := restarted.ClosePage(t.Context(), access, page); err != nil {
			t.Fatal(err)
		}
		setJar(t, restarted, access, JarWorktree)
		if _, err := os.Stat(restarted.recordPath("a")); !os.IsNotExist(err) {
			t.Fatalf("a thread with no page on the default jar keeps a saved file: %v", err)
		}
	})
	t.Run("with tabs", func(t *testing.T) {
		dir := t.TempDir()
		manager, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
		proj := newProject(t, "P")
		access := proj.access("a", proj.one)
		setJar(t, manager, access, JarProject)
		openPage(t, manager, access, "https://example.test/one")
		openPage(t, manager, access, "https://example.test/two")
		if err := manager.Close(); err != nil {
			t.Fatal(err)
		}
		saved, err := os.ReadFile(manager.recordPath("a"))
		if err != nil {
			t.Fatal(err)
		}
		if got := regexp.MustCompile(`"jar":\s*"project"`).FindAllIndex(saved, -1); len(got) != 1 {
			t.Fatalf("the saved file does not carry the thread's jar: %s", saved)
		}
		restarted, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
		state := restarted.CompanionState(access)
		if state.Jar != JarProject || len(state.Pages) != 2 || !state.Pages[0].Suspended {
			t.Fatalf("after a restart: jar %q, pages %+v", state.Jar, state.Pages)
		}
		if err := restarted.ActivateCompanionPage(t.Context(), access, state.Pages[0].ID); err != nil {
			t.Fatal(err)
		}
		if got := pageJar(t, restarted, state.Pages[0].ID); got != "project:P" {
			t.Fatalf("a saved page restored into %q, want its recorded project jar", got)
		}
	})
	t.Run("an earlier build's file", func(t *testing.T) {
		dir := t.TempDir()
		manager, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
		proj := newProject(t, "P")
		access := proj.access("a", proj.one)
		openPage(t, manager, access, "https://example.test/")
		if err := manager.Close(); err != nil {
			t.Fatal(err)
		}
		saved, err := os.ReadFile(manager.recordPath("a"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(saved), `"jar"`) {
			t.Fatalf("a default-jar thread's file differs from an earlier build's: %s", saved)
		}
		restarted, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
		if got := restarted.CompanionState(access).Jar; got != JarWorktree {
			t.Fatalf("an earlier build's file reads as jar %q, want worktree", got)
		}
	})
}

// restoreTabs restores every suspended tab of the thread, as using each
// would, so the tabs a jar switch suspended are live in the new jar.
func restoreTabs(t *testing.T, m *Manager, access Access) {
	t.Helper()
	for _, page := range m.CompanionState(access).Pages {
		if page.Suspended {
			if err := m.ActivateCompanionPage(t.Context(), access, page.ID); err != nil {
				t.Fatalf("restore %s: %v", page.ID, err)
			}
		}
	}
}

// Every engine derives its profile from the jar identity alone. For a
// worktree jar that is the derivation earlier builds used, byte for byte;
// every other kind derives a distinct, valid identity.
func TestEnginesDeriveTodaysIdentityForAWorktreeJar(t *testing.T) {
	const workspace = "/home/dev/repo"
	digest := sha256.Sum256([]byte(workspace))
	hosted, err := hostedProfileID(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if want := "ws" + hex.EncodeToString(digest[:16]); hosted != want {
		t.Fatalf("WebView2 profile = %q, want today's %q", hosted, want)
	}
	var raw [16]byte
	copy(raw[:], digest[:16])
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	if got, want := wkStoreIdentifier(workspace), fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]); got != want {
		t.Fatalf("WebKit store = %q, want today's %q", got, want)
	}
	if got, want := jarDirName(workspace), fmt.Sprintf("%x", digest[:12]); got != want {
		t.Fatalf("profile directory = %q, want today's %q", got, want)
	}

	seen := map[string]string{hosted: workspace}
	for _, jar := range []string{"project:P", "thread:a", "private:a:token"} {
		id, err := hostedProfileID(jar)
		if err != nil {
			t.Fatalf("WebView2 profile for %s: %v", jar, err)
		}
		if other, dup := seen[id]; dup {
			t.Fatalf("%s and %s share WebView2 profile %q", jar, other, id)
		}
		seen[id] = jar
		if !uuidPattern.MatchString(wkStoreIdentifier(jar)) {
			t.Fatalf("WebKit store for %s is not a valid identifier", jar)
		}
		if wkStoreIdentifier(jar) == wkStoreIdentifier(workspace) || jarDirName(jar) == jarDirName(workspace) {
			t.Fatalf("%s shares the worktree jar's store or directory", jar)
		}
	}
}

// Deleting or archiving a thread forgets its jar setting with its pages.
func TestClosingAThreadForgetsItsJar(t *testing.T) {
	manager, _ := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access("a", proj.one)
	setJar(t, manager, access, JarThread)
	openPage(t, manager, access, "https://example.test/")
	if err := manager.CloseThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(manager.recordPath("a")); !os.IsNotExist(err) {
		t.Fatalf("a closed thread's jar setting is still saved: %v", err)
	}
	if got := manager.CompanionState(access).Jar; got != JarWorktree {
		t.Fatalf("a closed thread's jar is %q, want the default", got)
	}
}

// A private jar closed with the browser is gone: the next private page opens
// a new one.
func TestAPrivateJarEndsWithTheBrowser(t *testing.T) {
	manager, _ := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access("a", proj.one)
	setJar(t, manager, access, JarPrivate)
	before := pageJar(t, manager, openPage(t, manager, access, "https://example.test/").ID)
	if err := manager.ClearSiteData(t.Context()); err != nil {
		t.Fatal(err)
	}
	after := pageJar(t, manager, openPage(t, manager, access, "https://example.test/").ID)
	if after == before {
		t.Fatalf("a private page after the clear reopened the cleared jar %q", before)
	}
}

// A private tab opened while the thread's last private tab is still shutting
// down opens a new jar: the closed tab's login is not the new tab's, and the
// old profile finishes in its own jar.
func TestAPrivateJarRetiresWhenItsLastTabCloses(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access("a", proj.one)
	setJar(t, manager, access, JarPrivate)
	first := openPage(t, manager, access, "https://example.test/1")
	old := pageJar(t, manager, first.ID)
	release := engine.holdClose()
	defer release()
	closed := make(chan error, 1)
	go func() { closed <- manager.ClosePage(context.Background(), access, first.ID) }()
	eventually(t, "the closed tab leaves the tab set", func() bool { return len(manager.CompanionState(access).Pages) == 0 })

	next := pageJar(t, manager, openPage(t, manager, access, "https://example.test/2").ID)
	if next == old || !strings.HasPrefix(next, "private:a:") {
		t.Fatalf("a private tab opened during the last one's teardown is in %q, want a new private jar", next)
	}
	if engine.profile(old).disposed.Load() {
		t.Fatal("the old private jar was disposed before its page finished closing")
	}
	release()
	if err := <-closed; err != nil {
		t.Fatalf("close: %v", err)
	}
	if !engine.profile(old).disposed.Load() {
		t.Fatal("the old private jar outlived its last page's teardown")
	}
	if pageJar(t, manager, tabIDs(manager, access)[0]) != next {
		t.Fatal("the old jar's teardown moved the new tab's jar")
	}
}

// A saved file naming a jar this build does not know is dropped whole, with
// a log line, as any other invalid saved file is.
func TestASavedFileWithAnUnknownJarIsDropped(t *testing.T) {
	dir := t.TempDir()
	manager, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	path := manager.recordPath("a")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"threadId":"a","jar":"shared","pages":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	restarted, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	if got := restarted.CompanionState(Access{ThreadID: "a"}).Jar; got != JarWorktree {
		t.Fatalf("an unknown saved jar reads as %q", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the invalid file was kept: %v", err)
	}
}

// A page creating its jar's profile takes its workspace's authority with its
// reservation, so a close elsewhere that prunes authorities meanwhile cannot
// leave it holding a counter its workspace no longer uses: the workspace keeps
// one authority, and one download quota.
func TestANewJarsFirstPageSharesItsWorkspacesAuthority(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	a, b := proj.access("a", proj.one), proj.access("b", proj.one)
	first := openPage(t, manager, a, "https://example.test/a")
	setJar(t, manager, b, JarThread)
	gate := make(chan struct{})
	engine.mu.Lock()
	engine.profileGate = gate
	engine.mu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			engine.mu.Lock()
			engine.profileGate = nil
			engine.mu.Unlock()
			close(gate)
		})
	}
	t.Cleanup(release)
	created := make(chan PageInfo, 1)
	failed := make(chan error, 1)
	go func() {
		info, err := manager.NewPage(t.Context(), b)
		if err != nil {
			failed <- err
			return
		}
		created <- info
	}()
	receive(t, engine.profiling, "the thread jar's profile creation")
	// The workspace's only registered page closes while b's profile is
	// created: the prune it runs sees no page of the workspace.
	if err := manager.ClosePage(t.Context(), a, first.ID); err != nil {
		t.Fatal(err)
	}
	release()
	var info PageInfo
	select {
	case info = <-created:
	case err := <-failed:
		t.Fatalf("create in the new jar: %v", err)
	}
	second := openPage(t, manager, a, "https://example.test/again")
	pb, _, err := manager.lookupOwnedPage(b, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	pa, _, err := manager.lookupOwnedPage(a, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	registered := manager.authorities[canonical(t, proj.one)]
	manager.mu.Unlock()
	if pb.authority != registered || pa.authority != registered {
		t.Fatal("one workspace's pages hold two authorities, so two download quotas")
	}
}
