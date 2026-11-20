package browser

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"agent-overflow/internal/browser/ids"
)

// attachHookProfile runs onAttach while the engine attaches a popup.
type attachHookProfile struct {
	*fakeProfile
	onAttach func()
}

func (p *attachHookProfile) AttachPage(ctx context.Context, _ ids.PageHandle, hooks pageHooks) (pageDriver, error) {
	p.onAttach()
	return p.fakeProfile.NewPage(ctx, hooks)
}

// A popup whose opener closes while the engine attaches it, as when the
// opener's thread is deleted, is not adopted by that thread.
func TestPopupOfAClosedOpenerIsNotAdopted(t *testing.T) {
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() { manager.Close() })
	workspace := t.TempDir()
	opener := manager.Bind(Caller{ThreadID: "opener", Workspace: workspace})
	page, err := manager.Open(t.Context(), opener, "https://example.test/", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Another thread's page keeps the workspace profile alive.
	if _, err := manager.Open(t.Context(), manager.Bind(Caller{ThreadID: "other", Workspace: workspace}), "https://example.test/", OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	openerHandle := paneHandle(manager, opener, page.ID)
	manager.mu.Lock()
	var profile *fakeProfile
	for _, scope := range manager.scopes {
		profile = scope.profile.(*fakeProfile)
		scope.profile = &attachHookProfile{fakeProfile: profile, onAttach: func() {
			if err := manager.CloseThread(context.Background(), opener.ThreadID()); err != nil {
				t.Errorf("close the opener's thread: %v", err)
			}
		}}
	}
	manager.mu.Unlock()
	manager.adoptPopup(enginePopup{Profile: profile.Handle(), Opener: ids.PageHandle(openerHandle), Handle: "popup"})
	if pages := manager.ownedPages(opener.tb); len(pages) != 0 {
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

func (p *closeHookProfile) AttachPage(ctx context.Context, _ ids.PageHandle, hooks pageHooks) (pageDriver, error) {
	return p.fakeProfile.NewPage(ctx, hooks)
}

type closeHookPage struct {
	pageDriver
	engine *closeHookEngine
}

func (p *closeHookPage) Close() {
	p.engine.once.Do(func() { p.engine.onClose(string(p.Handle())) })
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
	opener := manager.Bind(Caller{ThreadID: "opener", Workspace: t.TempDir()})
	page, err := manager.Open(t.Context(), opener, "https://example.test/", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	openerHandle := paneHandle(manager, opener, page.ID)
	engine.onClose = func(string) {
		manager.adoptPopup(enginePopup{Profile: engine.profile.Handle(), Opener: ids.PageHandle(openerHandle), Handle: "popup"})
	}
	if err := manager.CloseThread(context.Background(), opener.ThreadID()); err != nil {
		t.Fatalf("CloseThread: %v", err)
	}
	if pages := manager.ownedPages(opener.tb); len(pages) != 0 {
		t.Fatalf("the closed thread still owns %d page(s)", len(pages))
	}
}

// A page the engine closes by itself while the thread's close runs counts as
// closed rather than failing the close.
func TestCloseThreadCountsAPageTheEngineClosedAsClosed(t *testing.T) {
	manager, engine := newCloseHookManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
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
				manager.removeClosedPage(ids.PageHandle(handle))
			}
		}
	}
	if err := manager.CloseThread(context.Background(), access.ThreadID()); err != nil {
		t.Fatalf("CloseThread: %v", err)
	}
	if pages := manager.ownedPages(access.tb); len(pages) != 0 {
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
			access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
			first, err := manager.Open(t.Context(), access, "https://example.test/one", OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			second, err := manager.Open(t.Context(), access, "https://example.test/two", OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			show := true
			if _, err := manager.Visibility(t.Context(), access, &show, PageID(second.ID)); err != nil {
				t.Fatal(err)
			}
			closer.close(t, manager, access, second.ID)
			if state := manager.threadState(access.ThreadID()); state.ActivePageID != first.ID || !*state.Visible {
				t.Fatalf("after closing the active page: active %q visible %v, want %q visible", state.ActivePageID, *state.Visible, first.ID)
			}
			closer.close(t, manager, access, first.ID)
			if state := manager.threadState(access.ThreadID()); state.ActivePageID != "" || *state.Visible {
				t.Fatalf("after closing the last page: active %q visible %v, want none hidden", state.ActivePageID, *state.Visible)
			}
		})
	}
}

// discardRecordingEngine records the pages the Manager discards.
type discardRecordingEngine struct {
	*fakeEngine
	mu        sync.Mutex
	discarded []ids.PageHandle
}

func (e *discardRecordingEngine) DiscardPage(_ context.Context, handle ids.PageHandle) error {
	e.mu.Lock()
	e.discarded = append(e.discarded, handle)
	e.mu.Unlock()
	return nil
}

// A popup its profile fails to attach is discarded, not left open with no
// page to close it. The fake profile fails every attach.
func TestAPopupThatFailsToAttachIsDiscarded(t *testing.T) {
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() { manager.Close() })
	engine := &discardRecordingEngine{fakeEngine: newFakeEngine()}
	manager.engine = engine
	opener := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page, err := manager.Open(t.Context(), opener, "https://example.test/", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	var profile profileHandle
	for _, scope := range manager.scopes {
		profile = scope.profile.Handle()
	}
	manager.mu.Unlock()
	manager.adoptPopup(enginePopup{Profile: profile, Opener: driverHandle(manager, opener, page.ID), Handle: "popup"})
	engine.mu.Lock()
	discarded := slices.Clone(engine.discarded)
	engine.mu.Unlock()
	if !slices.Equal(discarded, []ids.PageHandle{"popup"}) {
		t.Fatalf("discarded %v, want [popup]", discarded)
	}
	if pages := manager.ownedPages(opener.tb); len(pages) != 1 {
		t.Fatalf("the thread owns %d pages, want its opener only", len(pages))
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

// A profile carries its pages' navigation policy, which an engine applies to
// a page it opened by itself before the Manager adopts it: the roots of the
// page a request resolves to, and the outside-workspace setting as it stands
// when a request is decided.
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
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: workspace, ProjectRoot: project})
	page, err := manager.NewPage(t.Context(), access)
	if err != nil {
		t.Fatal(err)
	}
	owners := []frameID{frameID(driverHandle(manager, access, page.ID))}
	engine.mu.Lock()
	policy := engine.opts.Allow
	engine.mu.Unlock()
	if policy == nil {
		t.Fatal("the profile was created without a navigation policy")
	}
	allow := resolvedPolicy(t, policy)
	// A request no page owns is answered unresolved, with what a page with
	// no workspace's authority may open.
	for url, want := range map[string]bool{"https://example.test/": true, "file://" + files["workspace"]: false} {
		if allowed, resolved := policy([]frameID{"F-none"}, url); allowed != want || resolved {
			t.Errorf("a request no page owns for %s was answered allowed %v, resolved %v", url, allowed, resolved)
		}
	}
	for url, want := range map[string]bool{
		"file://" + files["workspace"]: true,
		"file://" + files["project"]:   true,
		"file://" + files["outside"]:   false,
		"https://example.test/":        true,
		"chrome://settings/":           false,
	} {
		if got := allow(owners, url); got != want {
			t.Errorf("the profile's policy answers %v for %s, want %v", got, url, want)
		}
	}
	manager.mu.Lock()
	manager.config.AllowOutsideWorkspace = true
	manager.mu.Unlock()
	if !allow(owners, "file://"+files["outside"]) {
		t.Error("the profile's policy ignores the outside-workspace setting")
	}
}

// resolvedPolicy answers policy's allowed answer, failing the test when an
// owner it is asked with does not resolve.
func resolvedPolicy(t *testing.T, policy func([]frameID, string) (bool, bool)) func([]frameID, string) bool {
	return func(owners []frameID, rawURL string) bool {
		t.Helper()
		allowed, resolved := policy(owners, rawURL)
		if !resolved {
			t.Errorf("no owner of %v resolved", owners)
		}
		return allowed
	}
}

// The Manager keeps a download only when a managed page owns the frame it
// began in and, on an engine whose downloads are AO's, the workspace has
// quota for it. It refuses one over quota, and answers one no managed page
// owns as unowned; the engine cancels both.
func TestTheManagerKeepsOnlyADownloadAPageOwnsWithinQuota(t *testing.T) {
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() { manager.Close() })
	manager.engine.(*fakeEngine).saving = downloadsAsArtifacts
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page, err := manager.NewPage(t.Context(), access)
	if err != nil {
		t.Fatal(err)
	}
	frame := paneHandle(manager, access, page.ID)

	if got := manager.downloadStarted(downloadStart{Frame: "unowned", ID: "G-unowned"}); got != downloadUnowned {
		t.Fatalf("a download no managed page owns was answered %v, want unowned", got)
	}
	if manager.downloadStarted(downloadStart{Frame: frameID(frame), ID: "G-kept", SuggestedName: "a.bin"}) != downloadAdmitted {
		t.Fatal("a download in a managed page within quota was refused")
	}
	manager.mu.Lock()
	for _, authority := range manager.authorities {
		authority.downloadBytes.SetUsed(maxWorkspaceDownloadBytes)
	}
	manager.mu.Unlock()
	if got := manager.downloadStarted(downloadStart{Frame: frameID(frame), ID: "G-over"}); got != downloadRefused {
		t.Fatalf("a download over the workspace quota was answered %v, want refused", got)
	}

	listed, err := manager.Downloads(t.Context(), access, DownloadOptions{PageID: PageID(page.ID)})
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
