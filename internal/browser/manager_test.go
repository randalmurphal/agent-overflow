package browser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	if _, err := manager.Open(t.Context(), access, "https://example.test/second", OpenOptions{PageID: info.ID}); err != nil {
		t.Fatalf("second navigation: %v", err)
	}
	back, err := manager.History(t.Context(), access, info.ID, "back")
	if err != nil || back.URL != "https://example.test/first" {
		t.Fatalf("back = %#v, %v", back, err)
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

// Clearing site data deletes the ENGINE's profile tree (spec §4). There is no
// checkpoint left to clear, so the directory IS the site data.
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
