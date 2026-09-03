package bundle

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// fixtureFS is the shared fixture tree — the same directory
// `frontend/src/lib/native/bundleId.test.ts` hashes, so the two
// implementations of the id rule are pinned against one another through
// one golden.
func fixtureFS(t *testing.T) *Bundle {
	t.Helper()
	return New(os.DirFS(filepath.Join("testdata", "fixturebundle")), "1.2.3")
}

// goldenID reads the id both implementations must produce.
func goldenID(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "fixturebundle.id"))
	if err != nil {
		t.Fatalf("read golden id: %v", err)
	}
	return strings.TrimSpace(string(raw))
}

func TestManifestMatchesTheSharedGolden(t *testing.T) {
	manifest, err := fixtureFS(t).Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if manifest.ID != goldenID(t) {
		t.Fatalf("id = %s, want the golden %s\n"+
			"Both this package and frontend/scripts/bundleId.ts hash the same "+
			"fixture. If the rule changed on purpose, change BOTH and re-stamp "+
			"testdata/fixturebundle.id.", manifest.ID, goldenID(t))
	}
	if manifest.Version != "1.2.3" {
		t.Errorf("version = %q, want the link-time stamp", manifest.Version)
	}
	if manifest.MinShellBuild != MinShellBuild {
		t.Errorf("minShellBuild = %d, want %d", manifest.MinShellBuild, MinShellBuild)
	}
}

func TestManifestExcludesSourceMapsAndTheIDFile(t *testing.T) {
	manifest, err := fixtureFS(t).Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	want := []string{"README.txt", "assets/index.js", "index.html"}
	got := make([]string, 0, len(manifest.Files))
	for _, file := range manifest.Files {
		got = append(got, file.Path)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("files = %v, want %v (sorted, no *.map, no %s)", got, want, IDFileName)
	}
}

func TestManifestIsDeterministicAndContentAddressed(t *testing.T) {
	// Two Bundles over trees whose CONTENT is identical share an id even
	// though their versions differ: that is what lets a phone paired with
	// two machines on one release download nothing.
	tree := fstest.MapFS{
		"index.html":     &fstest.MapFile{Data: []byte("<!doctype html>")},
		"assets/app.js":  &fstest.MapFile{Data: []byte("console.log(1)")},
		"assets/app.css": &fstest.MapFile{Data: []byte("body{}")},
	}
	first, err := New(tree, "0.0.14").Manifest()
	if err != nil {
		t.Fatalf("first Manifest: %v", err)
	}
	second, err := New(tree, "dev").Manifest()
	if err != nil {
		t.Fatalf("second Manifest: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("identical content produced two ids: %s and %s", first.ID, second.ID)
	}

	// One byte different is a different bundle.
	moved := fstest.MapFS{
		"index.html":     &fstest.MapFile{Data: []byte("<!doctype html> ")},
		"assets/app.js":  &fstest.MapFile{Data: []byte("console.log(1)")},
		"assets/app.css": &fstest.MapFile{Data: []byte("body{}")},
	}
	changed, err := New(moved, "0.0.14").Manifest()
	if err != nil {
		t.Fatalf("changed Manifest: %v", err)
	}
	if changed.ID == first.ID {
		t.Fatal("a changed file left the id alone")
	}

	// A rename with the same bytes is a different bundle too: the id
	// covers paths, not just content, so a page requesting the old name
	// cannot be told it already has the new tree.
	renamed := fstest.MapFS{
		"index.html":     &fstest.MapFile{Data: []byte("<!doctype html>")},
		"assets/main.js": &fstest.MapFile{Data: []byte("console.log(1)")},
		"assets/app.css": &fstest.MapFile{Data: []byte("body{}")},
	}
	renamedManifest, err := New(renamed, "0.0.14").Manifest()
	if err != nil {
		t.Fatalf("renamed Manifest: %v", err)
	}
	if renamedManifest.ID == first.ID {
		t.Fatal("a renamed file left the id alone")
	}
}

func TestArchiveHoldsExactlyTheManifest(t *testing.T) {
	b := fixtureFS(t)
	manifest, err := b.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	archive, err := b.Archive()
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if len(reader.File) != len(manifest.Files) {
		t.Fatalf("archive holds %d entries, manifest names %d", len(reader.File), len(manifest.Files))
	}
	for i, entry := range reader.File {
		want := manifest.Files[i]
		if entry.Name != want.Path {
			t.Fatalf("entry %d is %q, manifest says %q", i, entry.Name, want.Path)
		}
		if entry.Method != zip.Deflate {
			t.Errorf("%s is stored, not deflated", entry.Name)
		}
		rc, err := entry.Open()
		if err != nil {
			t.Fatalf("open %s: %v", entry.Name, err)
		}
		digest := sha256.New()
		size, err := io.Copy(digest, rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name, err)
		}
		if got := hex.EncodeToString(digest.Sum(nil)); got != want.SHA256 {
			t.Errorf("%s hashes to %s, manifest says %s", entry.Name, got, want.SHA256)
		}
		if size != want.Size {
			t.Errorf("%s is %d bytes, manifest says %d", entry.Name, size, want.Size)
		}
	}
}

func TestArchiveIsBuiltOnceAndSharedByteIdentically(t *testing.T) {
	b := fixtureFS(t)
	first, err := b.Archive()
	if err != nil {
		t.Fatalf("first Archive: %v", err)
	}
	second, err := b.Archive()
	if err != nil {
		t.Fatalf("second Archive: %v", err)
	}
	if &first[0] != &second[0] {
		t.Fatal("Archive rebuilt the zip; the sync.Once cache is not holding")
	}

	// Two separate Bundles over one tree produce the same BYTES, which is
	// what "no modification times in the archive" buys.
	other, err := fixtureFS(t).Archive()
	if err != nil {
		t.Fatalf("other Archive: %v", err)
	}
	if !bytes.Equal(first, other) {
		t.Fatal("two builds of one tree produced different archive bytes")
	}
}

func TestAnEmptyTreeIsRefused(t *testing.T) {
	// A dist that was never built, or one holding only source maps: both
	// answer "no bundle" rather than a manifest a phone would stage over
	// a working app.
	if _, err := New(fstest.MapFS{}, "dev").Manifest(); err == nil {
		t.Fatal("an empty tree produced a manifest")
	}
	onlyMaps := fstest.MapFS{"assets/app.js.map": &fstest.MapFile{Data: []byte("{}")}}
	if _, err := New(onlyMaps, "dev").Manifest(); err == nil {
		t.Fatal("a tree of nothing but source maps produced a manifest")
	}
}

func TestNoTreeAtAllIsAnError(t *testing.T) {
	if _, err := New(nil, "dev").Manifest(); err == nil {
		t.Fatal("a nil file tree produced a manifest")
	}
	var absent *Bundle
	if _, err := absent.Manifest(); err == nil {
		t.Fatal("a nil Bundle produced a manifest")
	}
}

func TestATreeWithAPathAShellCannotWriteIsRefused(t *testing.T) {
	// fs.WalkDir yields whatever names the tree holds, and a name with a
	// backslash is legal in an fs.FS and a directory separator on the
	// consumer. The producer refuses to publish it rather than leaving
	// the plugin to reject every stage forever.
	tree := fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<html></html>")},
		`assets\app.js`: &fstest.MapFile{Data: []byte("1")},
	}
	if _, err := New(tree, "dev").Manifest(); err == nil {
		t.Fatal("Manifest published a path a shell may not write")
	}
}

func TestCleanPathRefusesEverythingThatEscapes(t *testing.T) {
	for _, ok := range []string{"index.html", "assets/app.js", "a/b/c.txt"} {
		if !CleanPath(ok) {
			t.Errorf("CleanPath(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{
		"",
		"/etc/passwd",
		"../outside",
		"assets/../../outside",
		"assets/./app.js",
		"assets//app.js",
		`assets\app.js`,
		"C:/windows",
		"assets/",
	} {
		if CleanPath(bad) {
			t.Errorf("CleanPath(%q) = true, want false", bad)
		}
	}
}
