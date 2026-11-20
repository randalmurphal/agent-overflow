package browser

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// fileGateEngine is the jar engine with a renderer file mapping
// (engineFileURL) whose BackendFilePath, once armed, holds until released.
// navigationAllowed asks it for a file address, and a restore checks its
// address after it decides to restore and before its admission, so the gate
// pauses a restore exactly there.
type fileGateEngine struct {
	*jarEngine
	armed  atomic.Bool
	calls  atomic.Int64
	asked  chan struct{}
	gate   chan struct{}
	opened atomic.Bool
}

func newFileGateEngine(t *testing.T, manager *Manager, engine *jarEngine) *fileGateEngine {
	gated := &fileGateEngine{jarEngine: engine, asked: make(chan struct{}, 16), gate: make(chan struct{})}
	t.Cleanup(gated.release)
	manager.engine = gated
	return gated
}

func (e *fileGateEngine) release() {
	if e.opened.CompareAndSwap(false, true) {
		close(e.gate)
	}
}

func (e *fileGateEngine) FileURL(_ context.Context, path string) (string, error) {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String(), nil
}

func (e *fileGateEngine) BackendFilePath(_ context.Context, rawURL string) (string, error) {
	e.calls.Add(1)
	if e.armed.Load() {
		e.asked <- struct{}{}
		<-e.gate
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	return filepath.FromSlash(parsed.Path), nil
}

// openWorkspaceFile writes a page in the caller's workspace and opens it in
// a new tab.
func openWorkspaceFile(t *testing.T, m *Manager, access Access) PageInfo {
	t.Helper()
	path := filepath.Join(access.Workspace(), "index.html")
	if err := os.WriteFile(path, []byte("<title>page</title>"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := m.OpenFile(t.Context(), access, path, OpenOptions{})
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	return info
}

// suspendTab suspends one live tab as the session reaper does.
func suspendTab(t *testing.T, m *Manager, access Access, pageID string) {
	t.Helper()
	p, _, err := m.lookupOwnedPage(access, PageID(pageID))
	if err != nil {
		t.Fatal(err)
	}
	if moved, err := m.suspendPage(t.Context(), p); err != nil || !moved {
		t.Fatalf("suspend %s: moved %v, %v", pageID, moved, err)
	}
}

// fillProfiles opens a page in n threads of their own jars, so each holds a
// profile.
func fillProfiles(t *testing.T, m *Manager, proj project, n int) {
	t.Helper()
	for i := range n {
		other := proj.access(m, "other-"+string(rune('a'+i)), proj.one)
		setJar(t, m, other, JarThread)
		openPage(t, m, other, "https://example.test/")
	}
}

// The jar's policy resolves a request of a page still being created, as a
// restore loads it before registering it, and reads the pages being created
// safely while others are created (run with -race).
func TestTheJarPolicyAnswersForAPageBeingCreated(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	a, b := proj.access(manager, "a", proj.one), proj.access(manager, "b", proj.two)
	setJar(t, manager, a, JarProject)
	setJar(t, manager, b, JarProject)
	other := filepath.Join(proj.two, "other.html")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	openPage(t, manager, b, "https://example.test/b")
	page := openWorkspaceFile(t, manager, a)
	suspendTab(t, manager, a, page.ID)
	policy := engine.profile("project:P").allow
	allow := func(owners []frameID, rawURL string) bool {
		allowed, _ := policy(owners, rawURL)
		return allowed
	}

	stop, polled := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(polled)
		for {
			select {
			case <-stop:
				return
			default:
				allow([]frameID{"F-none"}, "https://example.test/")
			}
		}
	}()
	release := engine.holdNav()
	t.Cleanup(release)
	restored := restore(manager, a, page.ID)
	<-engine.navigating
	engine.mu.Lock()
	handle := engine.lastPage
	engine.mu.Unlock()
	if !allow([]frameID{frameID(handle)}, page.URL) {
		t.Fatal("a page being restored is refused its own workspace's file")
	}
	if allow([]frameID{frameID(handle)}, "file://"+filepath.ToSlash(canonical(t, other))) {
		t.Fatal("a page being restored may open the other worktree's file")
	}
	release()
	if err := await(t, restored, "the restore"); err != nil {
		t.Fatal(err)
	}
	close(stop)
	<-polled
}

// Clearing site data while a restore has decided to restore its tab but is
// not yet admitted leaves nothing: the restore creates no profile and loads
// nothing once the clear has finished. Through the pane's selection the
// clear also cancels the restore; called with a context nothing cancels,
// the admission alone refuses it.
func TestClearingSiteDataBeforeARestoresAdmissionLeavesNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start func(m *Manager, access Access, pageID string) <-chan error
	}{
		{"selection", restore},
		{"uncancelled restore", func(m *Manager, access Access, pageID string) <-chan error {
			m.mu.Lock()
			rec := m.suspended[PageID(pageID)]
			m.mu.Unlock()
			return async(func() error { return m.restorePage(context.Background(), access, rec, false) })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, jars := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
			engine := newFileGateEngine(t, manager, jars)
			proj := newProject(t, "P")
			access := proj.access(manager, "a", proj.one)
			page := openWorkspaceFile(t, manager, access)
			suspendTab(t, manager, access, page.ID)
			waitDisposed(t, jars, canonical(t, proj.one))
			profiles := len(jars.created())

			engine.armed.Store(true)
			restored := tc.start(manager, access, page.ID)
			receive(t, engine.asked, "the restore to check its address")
			if err := manager.ClearSiteData(t.Context()); err != nil {
				t.Fatal(err)
			}
			engine.release()
			err := await(t, restored, "the restore")
			if tc.name == "selection" && err != nil {
				t.Fatalf("selecting a cleared tab answered %v", err)
			}
			if tc.name != "selection" && !errors.Is(err, errPageNotFound) {
				t.Fatalf("restoring a cleared record answered %v, want not found", err)
			}
			if got := jars.created(); len(got) != profiles {
				t.Fatalf("the restore created profiles %q after the clear", got[profiles:])
			}
			if len(manager.ownedPages(access.tb)) != 0 || len(tabIDs(manager, access)) != 0 {
				t.Fatal("the cleared tab came back")
			}
		})
	}
}

// Closing a tab whose restore is loading cancels the restore: it ends
// without waiting for the load.
func TestClosingATabCancelsItsRestore(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
	page := openPage(t, manager, access, "https://example.test/")
	suspendTab(t, manager, access, page.ID)
	release := engine.holdNav()
	t.Cleanup(release)
	restored := restore(manager, access, page.ID)
	<-engine.navigating
	if err := manager.CloseCompanionPage(t.Context(), access, PageID(page.ID)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-restored:
		if err != nil {
			t.Fatalf("selecting a closed tab answered %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the restore of a closed tab kept loading")
	}
}

// A restore is admitted only once every page its thread had closing at its
// latest jar switch has finished its teardown, whichever path left the tab
// suspended and at whichever attempt the restore is. Each case runs at the
// profile cap, where the old profile, still closing one page, is the only
// thing between the restore and its new jar's profile.
func TestARestoreWaitsForItsThreadsJarTeardown(t *testing.T) {
	type setup struct {
		manager *Manager
		jars    *jarEngine
		gate    *fileGateEngine
		proj    project
		access  Access
	}
	newSetup := func(t *testing.T) setup {
		manager, jars := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
		proj := newProject(t, "P")
		return setup{manager: manager, jars: jars, gate: newFileGateEngine(t, manager, jars), proj: proj, access: proj.access(manager, "a", proj.one)}
	}
	// expectWaiting fails when the restore ends, or tries again more than
	// once, before the held teardown is released, then releases it and
	// expects the tab in jar. Each attempt checks its file address once.
	expectWaiting := func(t *testing.T, s setup, restored <-chan error, release func(), pageID, jar string) {
		t.Helper()
		before := s.gate.calls.Load()
		select {
		case err := <-restored:
			t.Fatalf("the restore ended while its thread's old page was still closing: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
		if attempts := s.gate.calls.Load() - before; attempts > 1 {
			t.Fatalf("the restore tried %d times while its thread's old page was closing, instead of waiting", attempts)
		}
		release()
		if err := await(t, restored, "the restore"); err != nil {
			t.Fatalf("restore after the old page closed: %v", err)
		}
		if got := pageJar(t, s.manager, pageID); got != jar {
			t.Fatalf("the restored tab is in %s, want %s", got, jar)
		}
	}

	t.Run("suspended before the switch", func(t *testing.T) {
		s := newSetup(t)
		suspended := openWorkspaceFile(t, s.manager, s.access)
		live := openPage(t, s.manager, s.access, "https://example.test/live")
		suspendTab(t, s.manager, s.access, suspended.ID)
		fillProfiles(t, s.manager, s.proj, maxWorkspaceContexts-1)
		release := s.jars.holdCloseOf(t, paneHandle(s.manager, s.access, live.ID))
		setJar(t, s.manager, s.access, JarThread)
		expectWaiting(t, s, restore(s.manager, s.access, suspended.ID), release, suspended.ID, "thread:a")
	})

	t.Run("old profile still disposing", func(t *testing.T) {
		s := newSetup(t)
		suspended := openWorkspaceFile(t, s.manager, s.access)
		live := openPage(t, s.manager, s.access, "https://example.test/live")
		suspendTab(t, s.manager, s.access, suspended.ID)
		fillProfiles(t, s.manager, s.proj, maxWorkspaceContexts-1)
		release := s.jars.holdDispose(t)
		// The last page of the old profile has closed; its profile has not.
		if err := s.manager.CloseCompanionPage(t.Context(), s.access, PageID(live.ID)); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			s.manager.mu.Lock()
			disposing := len(s.manager.disposing) == 1 && len(s.manager.scopes) == maxWorkspaceContexts-1
			s.manager.mu.Unlock()
			if disposing {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the old profile never started disposing")
			}
			time.Sleep(time.Millisecond)
		}
		setJar(t, s.manager, s.access, JarThread)
		expectWaiting(t, s, restore(s.manager, s.access, suspended.ID), release, suspended.ID, "thread:a")
	})

	t.Run("left suspended by an earlier switch", func(t *testing.T) {
		s := newSetup(t)
		first := openPage(t, s.manager, s.access, "https://example.test/first")
		second := openWorkspaceFile(t, s.manager, s.access)
		fillProfiles(t, s.manager, s.proj, maxWorkspaceContexts-1)
		setJar(t, s.manager, s.access, JarThread)
		if err := await(t, restore(s.manager, s.access, first.ID), "the first restore"); err != nil {
			t.Fatal(err)
		}
		release := s.jars.holdCloseOf(t, paneHandle(s.manager, s.access, first.ID))
		setJar(t, s.manager, s.access, JarProject)
		expectWaiting(t, s, restore(s.manager, s.access, second.ID), release, second.ID, "project:P")
	})

	t.Run("retrying after a newer switch", func(t *testing.T) {
		s := newSetup(t)
		suspended := openWorkspaceFile(t, s.manager, s.access)
		live := openPage(t, s.manager, s.access, "https://example.test/live")
		suspendTab(t, s.manager, s.access, suspended.ID)
		fillProfiles(t, s.manager, s.proj, maxWorkspaceContexts-1)
		releaseNav := s.jars.holdNav()
		t.Cleanup(releaseNav)
		restored := restore(s.manager, s.access, suspended.ID)
		<-s.jars.navigating
		release := s.jars.holdCloseOf(t, paneHandle(s.manager, s.access, live.ID))
		setJar(t, s.manager, s.access, JarThread)
		// The load finishes in the old jar, so the restore tries again.
		releaseNav()
		expectWaiting(t, s, restored, release, suspended.ID, "thread:a")
	})

	t.Run("admitted after a newer switch", func(t *testing.T) {
		s := newSetup(t)
		suspended := openWorkspaceFile(t, s.manager, s.access)
		live := openPage(t, s.manager, s.access, "https://example.test/live")
		suspendTab(t, s.manager, s.access, suspended.ID)
		fillProfiles(t, s.manager, s.proj, maxWorkspaceContexts-1)
		s.gate.armed.Store(true)
		restored := restore(s.manager, s.access, suspended.ID)
		// Past its wait for a teardown, which had none, and not yet admitted.
		receive(t, s.gate.asked, "the restore to check its address")
		s.gate.armed.Store(false)
		release := s.jars.holdCloseOf(t, paneHandle(s.manager, s.access, live.ID))
		setJar(t, s.manager, s.access, JarThread)
		s.gate.release()
		expectWaiting(t, s, restored, release, suspended.ID, "thread:a")
	})
}
