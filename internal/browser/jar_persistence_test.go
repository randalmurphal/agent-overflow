package browser

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The caps bound engine resources as before: a profile is a jar, so the
// context cap counts jars; the workspace cap counts a workspace's pages in
// every jar; and a jar holds no more pages than one workspace may.
func TestCapsBoundJarsWorkspacesAndSharedJars(t *testing.T) {
	t.Run("profiles", func(t *testing.T) {
		manager, _ := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
		proj := newProject(t, "P")
		for i := range maxWorkspaceContexts {
			access := proj.access(manager, fmt.Sprintf("t%d", i), proj.one)
			setJar(t, manager, access, JarThread)
			mustOpen(t, manager, access, "https://example.test/")
		}
		access := proj.access(manager, "last", proj.one)
		setJar(t, manager, access, JarThread)
		if _, err := manager.NewPage(t.Context(), access); err == nil || !strings.Contains(err.Error(), "browser profile limit reached") {
			t.Fatalf("a thirteenth jar answered %v, want the profile limit", err)
		}
	})
	t.Run("workspace", func(t *testing.T) {
		manager, _ := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
		proj := newProject(t, "P")
		for i := range maxPagesPerWorkspace / maxPagesPerThread {
			access := proj.access(manager, fmt.Sprintf("t%d", i), proj.one)
			setJar(t, manager, access, JarThread)
			for range maxPagesPerThread {
				mustOpen(t, manager, access, "https://example.test/")
			}
		}
		access := proj.access(manager, "last", proj.one)
		setJar(t, manager, access, JarThread)
		if _, err := manager.NewPage(t.Context(), access); err == nil || !strings.Contains(err.Error(), "page limit reached for workspace") {
			t.Fatalf("a page past the workspace's cap in another jar answered %v, want the workspace limit", err)
		}
	})
	t.Run("shared jar", func(t *testing.T) {
		manager, _ := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
		proj := newProject(t, "P")
		threads := []Access{proj.access(manager, "a", proj.one), proj.access(manager, "b", proj.one), proj.access(manager, "c", proj.two)}
		for _, access := range threads {
			setJar(t, manager, access, JarProject)
			for range maxPagesPerThread {
				mustOpen(t, manager, access, "https://example.test/")
			}
		}
		access := proj.access(manager, "d", proj.two)
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
		access := proj.access(manager, fmt.Sprintf("t%d", i), proj.one)
		setJar(t, manager, access, kind)
		mustOpen(t, manager, access, "https://example.test/")
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
		access := proj.access(manager, fmt.Sprintf("t%d", i), proj.one)
		state := manager.CompanionState(access.Caller())
		if len(state.Pages) != 0 || state.Jar != kind {
			t.Fatalf("thread %d after the clear: %d pages, jar %q; want none and %q", i, len(state.Pages), state.Jar, kind)
		}
	}
	restarted, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	if got := restarted.CompanionState(proj.access(restarted, "t2", proj.one).Caller()).Jar; got != JarThread {
		t.Fatalf("a restart after the clear reads jar %q, want the thread's own", got)
	}
}

// With site data off every jar is in memory only, and a jar setting changes
// nothing about that.
func TestPersistOffKeepsEveryJarEphemeral(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: false})
	proj := newProject(t, "P")
	for i, kind := range []string{JarWorktree, JarProject, JarThread, JarPrivate} {
		access := proj.access(manager, fmt.Sprintf("t%d", i), proj.one)
		setJar(t, manager, access, kind)
		mustOpen(t, manager, access, "https://example.test/")
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
	access := proj.access(manager, "a", proj.one)
	setJar(t, manager, access, JarThread)
	if err := manager.Reconfigure(Config{Enabled: true, PersistSiteData: false}); err != nil {
		t.Fatal(err)
	}
	if got := manager.CompanionState(access.Caller()).Jar; got != JarThread {
		t.Fatalf("after the site-data change the jar is %q, want thread", got)
	}
	if _, err := os.Stat(manager.recordPath("a")); err != nil {
		t.Fatalf("the jar setting's saved copy went with the site-data change: %v", err)
	}
	if err := manager.Reconfigure(Config{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if got := manager.CompanionState(access.Caller()).Jar; got != JarWorktree {
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
		access := proj.access(manager, "a", proj.one)
		setJar(t, manager, access, JarThread)
		if err := manager.Close(); err != nil {
			t.Fatal(err)
		}
		restarted, _ := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
		access = restarted.Bind(access.Caller())
		if got := restarted.CompanionState(access.Caller()).Jar; got != JarThread {
			t.Fatalf("after a restart the jar is %q, want thread", got)
		}
		if got := pageJar(t, restarted, mustOpen(t, restarted, access, "https://example.test/").ID); got != "thread:a" {
			t.Fatalf("a page after the restart opened in %q, want the thread's jar", got)
		}
		page := tabIDs(restarted, access)[0]
		if err := restarted.ClosePage(t.Context(), access, PageID(page)); err != nil {
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
		access := proj.access(manager, "a", proj.one)
		setJar(t, manager, access, JarProject)
		mustOpen(t, manager, access, "https://example.test/one")
		mustOpen(t, manager, access, "https://example.test/two")
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
		access = restarted.Bind(access.Caller())
		state := restarted.CompanionState(access.Caller())
		if state.Jar != JarProject || len(state.Pages) != 2 || !state.Pages[0].Suspended {
			t.Fatalf("after a restart: jar %q, pages %+v", state.Jar, state.Pages)
		}
		if err := restarted.ActivateCompanionPage(t.Context(), access, PageID(state.Pages[0].ID)); err != nil {
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
		access := proj.access(manager, "a", proj.one)
		mustOpen(t, manager, access, "https://example.test/")
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
		if got := restarted.CompanionState(access.Caller()).Jar; got != JarWorktree {
			t.Fatalf("an earlier build's file reads as jar %q, want worktree", got)
		}
	})
}

// Deleting or archiving a thread forgets its jar setting with its pages.
func TestClosingAThreadForgetsItsJar(t *testing.T) {
	manager, _ := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
	setJar(t, manager, access, JarThread)
	mustOpen(t, manager, access, "https://example.test/")
	if err := manager.CloseThread(t.Context(), access.ThreadID()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(manager.recordPath("a")); !os.IsNotExist(err) {
		t.Fatalf("a closed thread's jar setting is still saved: %v", err)
	}
	if got := manager.CompanionState(access.Caller()).Jar; got != JarWorktree {
		t.Fatalf("a closed thread's jar is %q, want the default", got)
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
	if got := restarted.CompanionState(Caller{ThreadID: "a"}).Jar; got != JarWorktree {
		t.Fatalf("an unknown saved jar reads as %q", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the invalid file was kept: %v", err)
	}
}
