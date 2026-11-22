package browser

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// assetDriver is a page whose inventory lists assets, each fetched as
// body.
type assetDriver struct {
	pageDriver
	assets []AssetInfo
	body   string
	// copying, when set, runs as an asset's bytes are about to be written.
	copying func()
}

func (d *assetDriver) AssetInventory(context.Context) (pageAssets, error) {
	return pageAssets{PageURL: "https://example.test/", Assets: d.assets}, nil
}

func (d *assetDriver) AssetFetcher(context.Context) (assetFetcher, error) {
	return func(string) (assetStream, error) {
		return assetStream{
			ContentType: "text/css",
			Copy: func(out io.Writer, _, _ int64) (int64, error) {
				if d.copying != nil {
					d.copying()
				}
				n, err := io.WriteString(out, d.body)
				return int64(n), err
			},
			Close: func() {},
		}, nil
	}, nil
}

// An asset named manifest.json is bundled under the next variant name: the
// bundle's manifest keeps its own name and the asset its bytes.
func TestAnAssetNamedLikeTheManifestKeepsItsBytes(t *testing.T) {
	m, engine := newLifecycleManager(t)
	access := m.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	driver := &assetDriver{
		assets: []AssetInfo{{URL: "https://example.test/manifest.json", Kind: "stylesheet", Name: "manifest.json"}},
		body:   "the asset's own bytes",
	}
	engine.mu.Lock()
	engine.wrapDriver = func(inner pageDriver) pageDriver {
		driver.pageDriver = inner
		return driver
	}
	engine.mu.Unlock()
	page := mustOpen(t, m, access, "https://example.test/")
	listed, err := m.Assets(t.Context(), access, AssetOptions{PageID: PageID(page.ID), Action: "list"})
	if err != nil {
		t.Fatal(err)
	}
	inventory := listed.(AssetInventory)
	answered, err := m.Assets(t.Context(), access, AssetOptions{PageID: PageID(page.ID), Action: "bundle", InventoryID: inventory.ID, Kinds: []string{"stylesheet"}})
	if err != nil {
		t.Fatal(err)
	}
	bundle := answered.(AssetBundle)
	if len(bundle.Assets) != 1 {
		t.Fatalf("bundled %+v with failures %+v, want the one asset", bundle.Assets, bundle.Failures)
	}
	asset := bundle.Assets[0]
	if asset.Path == bundle.ManifestPath {
		t.Fatalf("the asset and the manifest share %s", asset.Path)
	}
	if got, err := os.ReadFile(asset.Path); err != nil || string(got) != driver.body {
		t.Fatalf("the asset holds %q, %v; want its own bytes", got, err)
	}
	manifest, err := os.ReadFile(bundle.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Assets []BundledAsset `json:"assets"`
	}
	if err := json.Unmarshal(manifest, &decoded); err != nil || len(decoded.Assets) != 1 || !strings.HasSuffix(decoded.Assets[0].Path, "manifest-1.json") {
		t.Fatalf("the manifest reads %s, %v; want it to list the asset as manifest-1.json", manifest, err)
	}
}
