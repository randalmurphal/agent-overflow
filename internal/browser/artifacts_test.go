package browser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The artifact root is held from its first scan: a later redirect of its
// name to a full folder changes neither what a measurement of it finds nor
// where an artifact is written. A one-byte reservation is judged against
// the folder held. A bundle written after the redirect goes into the folder
// held, where the paths it would hand out no longer reach, so it is
// refused, removed through the folder held and its bytes released.
func TestAnArtifactRootRedirectedAfterItsScanChangesNothing(t *testing.T) {
	m, engine := newLifecycleManager(t)
	access := m.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	driver := &assetDriver{assets: []AssetInfo{{URL: "https://example.test/a.css", Kind: "stylesheet", Name: "a.css"}}, body: "the asset"}
	engine.mu.Lock()
	engine.wrapDriver = func(inner pageDriver) pageDriver {
		driver.pageDriver = inner
		return driver
	}
	engine.mu.Unlock()
	if err := m.prepareArtifacts(); err != nil {
		t.Fatal(err)
	}

	full := t.TempDir()
	sparse, err := os.Create(filepath.Join(full, "full.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sparse.Truncate(maxArtifactRootBytes); err != nil {
		t.Fatal(err)
	}
	if err := sparse.Close(); err != nil {
		t.Fatal(err)
	}
	held := m.artifactRoot + ".held"
	if err := os.Rename(m.artifactRoot, held); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(full, m.artifactRoot); err != nil {
		t.Fatal(err)
	}

	// A measurement made again measures the folder held.
	m.artifactInitMu.Lock()
	m.artifactReady = false
	m.artifactInitMu.Unlock()
	reservation, err := m.reserveArtifacts(1)
	if err != nil {
		t.Fatalf("a one-byte reservation after the redirect answered %v, with %d bytes measured", err, m.artifactBytes.Used())
	}
	reservation.Settle(0)
	if used := m.artifactBytes.Used(); used != 0 {
		t.Fatalf("the held folder measures %d bytes, want the empty folder held, not the full one", used)
	}

	page := mustOpen(t, m, access, "https://example.test/")
	listed, err := m.Assets(t.Context(), access, AssetOptions{PageID: PageID(page.ID), Action: "list"})
	if err != nil {
		t.Fatal(err)
	}
	answered, err := m.Assets(t.Context(), access, AssetOptions{PageID: PageID(page.ID), Action: "bundle", InventoryID: listed.(AssetInventory).ID, Kinds: []string{"stylesheet"}})
	if err == nil || !strings.Contains(err.Error(), "AO wrote there") {
		t.Fatalf("a bundle whose paths name the redirected folder answered %+v, %v; want it refused", answered, err)
	}
	if entries, err := os.ReadDir(filepath.Join(held, "assets")); err != nil || len(entries) != 0 {
		t.Fatalf("the refused bundle left %v in the folder held: %v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(full, "assets")); !os.IsNotExist(err) {
		t.Fatalf("the bundle wrote into the folder the name was redirected to: %v", err)
	}
	if used := m.artifactBytes.Used(); used != 0 {
		t.Fatalf("the quota holds %d bytes after the refused bundle was removed", used)
	}
}

// A jar's download directory a headless profile opens (createScope's
// OpenDownloadDir) is opened through the held artifact root, so after the
// root's name is redirected the room is made in the folder held, where the
// path Chromium would be told no longer reaches, and the launch is refused
// with nothing made in the folder the name now reaches.
func TestADownloadRoomAfterTheArtifactRootIsRedirectedIsRefused(t *testing.T) {
	m := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() { _ = m.Close() })
	if err := m.prepareArtifacts(); err != nil {
		t.Fatal(err)
	}
	held := m.artifactRoot + ".held"
	if err := os.Rename(m.artifactRoot, held); err != nil {
		t.Fatal(err)
	}
	redirected := t.TempDir()
	if err := os.Symlink(redirected, m.artifactRoot); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(m.artifactRoot, jarDownloadingDir, jarDirName("jar"))
	parent, err := m.openArtifactDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if room, err := openDownloadRoom(parent, dir, t.Logf); err == nil {
		room.close()
		t.Fatalf("a room was opened at %s, which the redirected name reaches", room.path())
	}
	if entries, err := os.ReadDir(redirected); err != nil || len(entries) != 0 {
		t.Fatalf("the folder the name was redirected to holds %v: %v", entries, err)
	}
	rel, err := filepath.Rel(m.artifactRoot, dir)
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Join(held, rel)); err != nil || len(entries) != 0 {
		t.Fatalf("the refused room stayed in the folder held: %v, %v", entries, err)
	}
}
