package browser

import (
	"os"
	"path/filepath"
	"testing"
)

// The artifact root is held from its first scan: a later redirect of its
// name to a full folder changes neither what a measurement of it finds nor
// where an artifact is written. A one-byte reservation is judged against
// the folder held, and a bundle written after the redirect lands in it.
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
	if err != nil {
		t.Fatal(err)
	}
	bundle := answered.(AssetBundle)
	if len(bundle.Assets) != 1 {
		t.Fatalf("bundled %+v with failures %+v, want the one asset", bundle.Assets, bundle.Failures)
	}
	rel, err := filepath.Rel(m.artifactRoot, bundle.Assets[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(held, rel)); err != nil || string(got) != driver.body {
		t.Fatalf("the asset in the folder held reads %q, %v; want its bytes", got, err)
	}
	if _, err := os.Stat(filepath.Join(full, "assets")); !os.IsNotExist(err) {
		t.Fatalf("the bundle wrote into the folder the name was redirected to: %v", err)
	}
	manifestRel, err := filepath.Rel(m.artifactRoot, bundle.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.Stat(filepath.Join(held, manifestRel))
	if err != nil {
		t.Fatalf("the manifest is not in the folder held: %v", err)
	}
	if used, want := m.artifactBytes.Used(), int64(len(driver.body))+manifest.Size(); used != want {
		t.Fatalf("the quota holds %d bytes, want the bundle's %d", used, want)
	}
}
