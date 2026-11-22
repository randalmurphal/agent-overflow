package browser

import (
	"errors"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
)

// wantLiveInJar fails unless the tab id is live in jar with no failure.
func wantLiveInJar(t *testing.T, manager *Manager, access Access, id, jar string) {
	t.Helper()
	if got := pageJar(t, manager, id); got != jar {
		t.Fatalf("the tab is in %q, want the new jar %q", got, jar)
	}
	for _, tab := range manager.CompanionState(access.Caller()).Pages {
		if tab.ID == id && (tab.Suspended || tab.Error != "") {
			t.Fatalf("the tab is %+v, want it live with no failure", tab)
		}
	}
}

// The crash of a page created or restored in the jar its thread switched
// away from means nothing for its tab: the attempt is stale, and its tab
// is created or restored again in the new jar, live, carrying no failure.
func TestAStalePagesCrashDoesNotFailItsTab(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
		proj := newProject(t, "P")
		access := proj.access(manager, "a", proj.one)
		release := engine.holdNewPage()
		t.Cleanup(release)
		type answer struct {
			info PageInfo
			err  error
		}
		created := make(chan answer, 1)
		go func() {
			info, err := manager.NewPage(t.Context(), access)
			created <- answer{info, err}
		}()
		receive(t, engine.creating, "the page's creation in the old jar")
		setJar(t, manager, access, JarProject)
		engine.mu.Lock()
		engine.afterNewPage = func(handle ids.PageHandle) { manager.pageCrashedReport(handle, "oom") }
		engine.mu.Unlock()
		release()
		got := <-created
		if got.err != nil {
			t.Fatalf("the create answered %v, want the page created again in the new jar", got.err)
		}
		wantLiveInJar(t, manager, access, got.info.ID, "project:P")
	})
	t.Run("restore", func(t *testing.T) {
		manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
		proj := newProject(t, "P")
		access := proj.access(manager, "a", proj.one)
		page := mustOpen(t, manager, access, "https://example.test/saved")
		suspendTab(t, manager, access, page.ID)
		release := engine.holdNav()
		t.Cleanup(release)
		restored := restore(manager, access, page.ID)
		if got := <-engine.navigating; got != "https://example.test/saved" {
			t.Fatalf("the restore navigated to %q", got)
		}
		engine.mu.Lock()
		stale := engine.lastPage
		engine.mu.Unlock()
		setJar(t, manager, access, JarThread)
		manager.pageCrashedReport(stale, "oom")
		release()
		if err := <-restored; err != nil {
			t.Fatalf("the restore answered %v, want it restored again in the new jar", err)
		}
		wantLiveInJar(t, manager, access, page.ID, "thread:a")
	})
}

// An attempt whose engine fails to make its page after its thread switched
// jars is as stale as one whose page crashed: the failure belongs to the
// old jar, and the tab is created or restored again in the new one, live,
// carrying no failure.
func TestAStaleAttemptsFailureBeforeItsPageDoesNotFailItsTab(t *testing.T) {
	oom := errors.New("renderer out of memory")
	t.Run("create", func(t *testing.T) {
		manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
		proj := newProject(t, "P")
		access := proj.access(manager, "a", proj.one)
		release := engine.holdNewPage()
		t.Cleanup(release)
		type answer struct {
			info PageInfo
			err  error
		}
		created := make(chan answer, 1)
		go func() {
			info, err := manager.NewPage(t.Context(), access)
			created <- answer{info, err}
		}()
		receive(t, engine.creating, "the page's creation in the old jar")
		setJar(t, manager, access, JarProject)
		engine.mu.Lock()
		engine.failNewPage = oom
		engine.mu.Unlock()
		release()
		got := <-created
		if got.err != nil {
			t.Fatalf("the create answered %v, want the page created again in the new jar", got.err)
		}
		wantLiveInJar(t, manager, access, got.info.ID, "project:P")
	})
	t.Run("restore", func(t *testing.T) {
		manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
		proj := newProject(t, "P")
		access := proj.access(manager, "a", proj.one)
		page := mustOpen(t, manager, access, "https://example.test/saved")
		suspendTab(t, manager, access, page.ID)
		release := engine.holdNewPage()
		t.Cleanup(release)
		restored := restore(manager, access, page.ID)
		receive(t, engine.creating, "the restore's page creation in the old jar")
		setJar(t, manager, access, JarThread)
		engine.mu.Lock()
		engine.failNewPage = oom
		engine.mu.Unlock()
		release()
		if err := <-restored; err != nil {
			t.Fatalf("the restore answered %v, want it restored again in the new jar", err)
		}
		wantLiveInJar(t, manager, access, page.ID, "thread:a")
	})
}

// An attempt carries its thread's jar generation from its start: after a
// switch, a setup that fails before it chooses a jar is current, and its
// failure answers once instead of retrying as if stale.
func TestAFailureBeforeTheJarIsChosenAnswersAfterASwitch(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
	setJar(t, manager, access, JarProject)
	refused := errors.New("the engine cannot start")
	engine.mu.Lock()
	engine.startErr = refused
	engine.mu.Unlock()
	created := make(chan error, 1)
	go func() {
		_, err := manager.NewPage(t.Context(), access)
		created <- err
	}()
	select {
	case err := <-created:
		if !errors.Is(err, refused) {
			t.Fatalf("the create answered %v, want its engine's failure", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the create never answered its engine's failure")
	}
}
