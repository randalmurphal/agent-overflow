package browser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The default jar is exactly what every earlier build used: the engine is
// handed the canonical workspace path as its identity, persistent, so each
// engine derives the profile directory, WebView2 profile name and WebKit
// store it derived before, and downloads land in the same directory.
func TestWorktreeJarIsTodaysWorkspaceProfile(t *testing.T) {
	dir := t.TempDir()
	manager, engine := newJarManager(t, dir, Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "thread", proj.one)
	mustOpen(t, manager, access, "https://example.test/")
	workspace := canonical(t, proj.one)

	if got := engine.created(); len(got) != 1 || got[0] != workspace {
		t.Fatalf("profiles created for %q, want the workspace path %q", got, workspace)
	}
	profile := engine.profile(workspace)
	if profile.ephemeral {
		t.Fatal("the default jar is ephemeral with site data persisted")
	}
	digest := sha256.Sum256([]byte(workspace))
	if got, want := jarDirName(jarKey(workspace)), fmt.Sprintf("%x", digest[:12]); got != want {
		t.Fatalf("profile directory name = %q, want today's %q", got, want)
	}
	manager.mu.Lock()
	scope := manager.scopes[jarKey(workspace)]
	manager.mu.Unlock()
	if want := filepath.Join(dir, "browser-artifacts", "downloading", fmt.Sprintf("%x", digest[:12])); scope.downloadDir != want {
		t.Fatalf("download directory = %q, want %q", scope.downloadDir, want)
	}
	if state := manager.CompanionState(access.Caller()); state.Jar != JarWorktree {
		t.Fatalf("state names jar %q, want worktree", state.Jar)
	}
}

// A project jar is one profile for every thread of the project that chose it,
// across worktrees; a thread that did not choose it keeps its worktree's.
func TestProjectJarIsSharedAcrossWorktreesOnlyByThreadsThatChoseIt(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	a, b, c := proj.access(manager, "a", proj.one), proj.access(manager, "b", proj.two), proj.access(manager, "c", proj.two)

	pageA := mustOpen(t, manager, a, "https://example.test/a")
	pageB := mustOpen(t, manager, b, "https://example.test/b")
	if pageJar(t, manager, pageA.ID) == pageJar(t, manager, pageB.ID) {
		t.Fatal("two worktrees share a jar with the default setting")
	}
	setJar(t, manager, a, JarProject)
	setJar(t, manager, b, JarProject)
	restoreTabs(t, manager, a)
	restoreTabs(t, manager, b)
	pageC := mustOpen(t, manager, c, "https://example.test/c")

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
	d := other.access(manager, "d", other.one)
	setJar(t, manager, d, JarProject)
	if got := pageJar(t, manager, mustOpen(t, manager, d, "https://example.test/d").ID); got != "project:Q" {
		t.Fatalf("another project's thread opened in %q, want its own project jar", got)
	}
}

// A thread jar isolates two threads on one worktree from each other and from
// the worktree's shared jar.
func TestThreadJarIsolatesThreadsOnOneWorktree(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	a, b, c := proj.access(manager, "a", proj.one), proj.access(manager, "b", proj.one), proj.access(manager, "c", proj.one)
	setJar(t, manager, a, JarThread)
	setJar(t, manager, b, JarThread)
	jarA := pageJar(t, manager, mustOpen(t, manager, a, "https://example.test/").ID)
	jarB := pageJar(t, manager, mustOpen(t, manager, b, "https://example.test/").ID)
	jarC := pageJar(t, manager, mustOpen(t, manager, c, "https://example.test/").ID)
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
	access := proj.access(manager, "a", proj.one)
	setJar(t, manager, access, JarPrivate)
	first := mustOpen(t, manager, access, "https://example.test/1")
	second := mustOpen(t, manager, access, "https://example.test/2")
	jar := pageJar(t, manager, first.ID)
	if !strings.HasPrefix(jar, "private:a:") || pageJar(t, manager, second.ID) != jar {
		t.Fatalf("private pages opened in %q and %q, want one private jar", jar, pageJar(t, manager, second.ID))
	}
	if !engine.profile(jar).ephemeral {
		t.Fatal("a private jar was persistent")
	}
	for _, page := range []PageInfo{first, second} {
		if err := manager.ClosePage(t.Context(), access, PageID(page.ID)); err != nil {
			t.Fatal(err)
		}
	}
	if !engine.profile(jar).disposed.Load() {
		t.Fatal("the private jar outlived its last page")
	}
	reopened := pageJar(t, manager, mustOpen(t, manager, access, "https://example.test/3").ID)
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
	a, b := proj.access(manager, "a", proj.one), proj.access(manager, "b", proj.two)
	setJar(t, manager, a, JarProject)
	setJar(t, manager, b, JarProject)
	pageA := mustOpen(t, manager, a, "https://example.test/a")
	pageB := mustOpen(t, manager, b, "https://example.test/b")
	if pageJar(t, manager, pageA.ID) != pageJar(t, manager, pageB.ID) {
		t.Fatal("the two worktrees do not share the project jar")
	}

	if _, err := manager.OpenFile(t.Context(), a, files["two"], OpenOptions{PageID: PageID(pageA.ID)}); err == nil {
		t.Fatal("a page opened a file of the other worktree sharing its jar")
	}
	if _, err := manager.OpenFile(t.Context(), a, files["one"], OpenOptions{PageID: PageID(pageA.ID)}); err != nil {
		t.Fatalf("a page could not open its own worktree's file: %v", err)
	}
	livePage, _, err := manager.lookupOwnedPage(b, PageID(pageB.ID))
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
	openerA, openerB := frameID(driverHandle(manager, a, pageA.ID)), frameID(driverHandle(manager, b, pageB.ID))
	if !allow([]frameID{"popup", openerA}, "file://"+files["one"]) || !allow([]frameID{"popup", openerB}, "file://"+files["two"]) {
		t.Fatal("a popup in the shared jar is refused its opener's worktree file")
	}
	if allow([]frameID{"popup", openerA}, "file://"+files["two"]) || allow([]frameID{"popup", openerB}, "file://"+files["one"]) {
		t.Fatal("a popup in the shared jar may open the other worktree's file")
	}
	if !allow([]frameID{openerA}, "file://"+files["one"]) || allow([]frameID{openerA}, "file://"+files["two"]) {
		t.Fatal("a page's own request is not answered with its own authority")
	}

	manager.mu.Lock()
	manager.authorities[canonical(t, proj.one)].downloadBytes.SetUsed(maxWorkspaceDownloadBytes)
	manager.mu.Unlock()
	if manager.downloadStarted(downloadStart{Frame: frameID(paneHandle(manager, a, pageA.ID)), ID: "over"}) == downloadAdmitted {
		t.Fatal("a download over its workspace's quota was kept")
	}
	if manager.downloadStarted(downloadStart{Frame: frameID(paneHandle(manager, b, pageB.ID)), ID: "kept"}) != downloadAdmitted {
		t.Fatal("the other workspace's download was charged to the exhausted workspace")
	}
	manager.mu.Lock()
	dirOne, dirTwo := manager.authorities[canonical(t, proj.one)].downloadDir, manager.authorities[canonical(t, proj.two)].downloadDir
	manager.mu.Unlock()
	if dirOne == dirTwo {
		t.Fatal("two workspaces sharing a jar share a download directory")
	}

	// A workspace's authority lives while it has a page in any jar.
	if err := manager.ClosePage(t.Context(), a, PageID(pageA.ID)); err != nil {
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
	if want := "ws" + hex.EncodeToString(digest[:16]); hosted != profileHandle(want) {
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

	seen := map[string]string{string(hosted): workspace}
	for _, jar := range []string{"project:P", "thread:a", "private:a:token"} {
		id, err := hostedProfileID(jarKey(jar))
		if err != nil {
			t.Fatalf("WebView2 profile for %s: %v", jar, err)
		}
		if other, dup := seen[string(id)]; dup {
			t.Fatalf("%s and %s share WebView2 profile %q", jar, other, id)
		}
		seen[string(id)] = jar
		if !uuidPattern.MatchString(wkStoreIdentifier(jarKey(jar))) {
			t.Fatalf("WebKit store for %s is not a valid identifier", jar)
		}
		if wkStoreIdentifier(jarKey(jar)) == wkStoreIdentifier(workspace) || jarDirName(jarKey(jar)) == jarDirName(workspace) {
			t.Fatalf("%s shares the worktree jar's store or directory", jar)
		}
	}
}

// A private jar closed with the browser is gone: the next private page opens
// a new one.
func TestAPrivateJarEndsWithTheBrowser(t *testing.T) {
	manager, _ := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	access := proj.access(manager, "a", proj.one)
	setJar(t, manager, access, JarPrivate)
	before := pageJar(t, manager, mustOpen(t, manager, access, "https://example.test/").ID)
	if err := manager.ClearSiteData(t.Context()); err != nil {
		t.Fatal(err)
	}
	after := pageJar(t, manager, mustOpen(t, manager, access, "https://example.test/").ID)
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
	access := proj.access(manager, "a", proj.one)
	setJar(t, manager, access, JarPrivate)
	first := mustOpen(t, manager, access, "https://example.test/1")
	old := pageJar(t, manager, first.ID)
	release := engine.holdClose()
	defer release()
	closed := make(chan error, 1)
	go func() { closed <- manager.ClosePage(context.Background(), access, PageID(first.ID)) }()
	eventually(t, "the closed tab leaves the tab set", func() bool { return len(manager.CompanionState(access.Caller()).Pages) == 0 })

	next := pageJar(t, manager, mustOpen(t, manager, access, "https://example.test/2").ID)
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

// A page creating its jar's profile takes its workspace's authority with its
// reservation, so a close elsewhere that prunes authorities meanwhile cannot
// leave it holding a counter its workspace no longer uses: the workspace keeps
// one authority, and one download quota.
func TestANewJarsFirstPageSharesItsWorkspacesAuthority(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	a, b := proj.access(manager, "a", proj.one), proj.access(manager, "b", proj.one)
	first := mustOpen(t, manager, a, "https://example.test/a")
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
	if err := manager.ClosePage(t.Context(), a, PageID(first.ID)); err != nil {
		t.Fatal(err)
	}
	release()
	var info PageInfo
	select {
	case info = <-created:
	case err := <-failed:
		t.Fatalf("create in the new jar: %v", err)
	}
	second := mustOpen(t, manager, a, "https://example.test/again")
	pb, _, err := manager.lookupOwnedPage(b, PageID(info.ID))
	if err != nil {
		t.Fatal(err)
	}
	pa, _, err := manager.lookupOwnedPage(a, PageID(second.ID))
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
