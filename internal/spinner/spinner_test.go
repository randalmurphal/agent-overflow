package spinner

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newService builds a Service over a throwaway config dir. Every test in
// this package works inside t.TempDir(): the spinners directory is a real
// user config surface, so a test that resolved the real one would seed
// and overwrite the developer's own reference file.
func newService(t *testing.T) (*Service, string) {
	t.Helper()
	configDir := t.TempDir()
	service, err := New(configDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if service.Dir() != filepath.Join(configDir, DirName) {
		t.Fatalf("Dir() = %q, want %q", service.Dir(), filepath.Join(configDir, DirName))
	}
	return service, configDir
}

// writeSprite drops a whole pair into dir.
func writeSprite(t *testing.T, dir, id string, png []byte, manifest string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".png"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
}

func warningsMentioning(warnings []string, needle string) []string {
	var hits []string
	for _, warning := range warnings {
		if strings.Contains(warning, needle) {
			hits = append(hits, warning)
		}
	}
	return hits
}

func TestEnsureBootSeedsTheReferenceAndRefreshesDrift(t *testing.T) {
	service, _ := newService(t)
	if err := service.EnsureBoot(); err != nil {
		t.Fatalf("EnsureBoot: %v", err)
	}

	path := filepath.Join(service.Dir(), ReferenceFileName)
	seeded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded reference: %v", err)
	}
	embedded, err := assets.ReadFile("assets/" + ReferenceFileName)
	if err != nil {
		t.Fatalf("read embedded reference: %v", err)
	}
	if string(seeded) != string(embedded) {
		t.Fatal("seeded reference differs from the embedded copy")
	}
	// The header has to say the file is generated, because the refresh
	// below is what happens to anything a user writes into it.
	if !strings.Contains(string(seeded[:min(len(seeded), 400)]), "GENERATED FILE") {
		t.Fatalf("reference does not open with a generated-file header: %.200s", seeded)
	}

	// Drift is corrected, not merely absence: the on-disk copy documents
	// THIS build's contract.
	if err := os.WriteFile(path, []byte("hand-edited"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := service.EnsureBoot(); err != nil {
		t.Fatalf("second EnsureBoot: %v", err)
	}
	refreshed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(refreshed) != string(embedded) {
		t.Fatal("a hand-edited reference survived boot")
	}

	// The directory is private: it sits in the user's config dir.
	info, err := os.Stat(service.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if runtimeSupportsUnixPerms() && info.Mode().Perm() != privateDirPerm {
		t.Fatalf("spinners dir mode = %v, want %v", info.Mode().Perm(), privateDirPerm)
	}
}

func TestFilesListsWholePairsVerbatim(t *testing.T) {
	service, _ := newService(t)
	if err := service.EnsureBoot(); err != nil {
		t.Fatalf("EnsureBoot: %v", err)
	}
	dir := service.Dir()

	// Bytes that are not valid UTF-8, because a PNG never is.
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0xff, 0xfe}
	manifest := `{"frames":8,"frameMs":100}`
	writeSprite(t, dir, "robo-papers", png, manifest)
	writeSprite(t, dir, "orb", []byte("second"), `{"frames":2,"frameMs":500}`)

	files := service.Files()
	if files.Dir != dir {
		t.Fatalf("dir = %q, want %q", files.Dir, dir)
	}
	if len(files.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", files.Warnings)
	}
	if len(files.Sprites) != 2 {
		t.Fatalf("sprites = %+v, want 2", files.Sprites)
	}
	// Sorted by id, so a listing is stable across runs.
	if files.Sprites[0].ID != "orb" || files.Sprites[1].ID != "robo-papers" {
		t.Fatalf("sprite order = %q, %q, want id-sorted", files.Sprites[0].ID, files.Sprites[1].ID)
	}
	got := files.Sprites[1]
	if got.Manifest != manifest {
		t.Fatalf("manifest = %q, want the file's bytes verbatim", got.Manifest)
	}
	decoded, err := base64.StdEncoding.DecodeString(got.PNG)
	if err != nil {
		t.Fatalf("png is not base64: %v", err)
	}
	if string(decoded) != string(png) {
		t.Fatalf("decoded png = %v, want the file's bytes verbatim", decoded)
	}

	// The reference doc is not a sprite, and neither is a stray file.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(service.Files().Sprites) != 2 {
		t.Fatal("a non-sprite file entered the listing")
	}
}

func TestFilesWarnsAboutHalfSprites(t *testing.T) {
	service, _ := newService(t)
	dir := service.Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lonely-strip.png"), []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lonely-manifest.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeSprite(t, dir, "whole", []byte("png"), "{}")

	files := service.Files()
	if len(files.Sprites) != 1 || files.Sprites[0].ID != "whole" {
		t.Fatalf("sprites = %+v, want only the whole pair", files.Sprites)
	}
	// Each warning has to name the file that is MISSING — that is the
	// entire actionable content of it.
	if hits := warningsMentioning(files.Warnings, "lonely-strip.json"); len(hits) != 1 {
		t.Fatalf("warnings = %v, want one naming the absent lonely-strip.json", files.Warnings)
	}
	if hits := warningsMentioning(files.Warnings, "lonely-manifest.png"); len(hits) != 1 {
		t.Fatalf("warnings = %v, want one naming the absent lonely-manifest.png", files.Warnings)
	}
}

func TestFilesWarnsOncePerInvalidStem(t *testing.T) {
	service, _ := newService(t)
	dir := service.Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"My Sprite.png", "My Sprite.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"../escape.png", "-leading.png", strings.Repeat("a", 65) + ".png"} {
		// The path-escape case is written under its BASE name; the point is
		// that a stem like that can never reach the frontend as an id.
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(name)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	files := service.Files()
	if len(files.Sprites) != 0 {
		t.Fatalf("sprites = %+v, want none", files.Sprites)
	}
	if hits := warningsMentioning(files.Warnings, "My Sprite"); len(hits) != 1 {
		t.Fatalf("warnings = %v, want exactly one for the My Sprite pair", files.Warnings)
	}
	if len(files.Warnings) != 4 {
		t.Fatalf("warnings = %v, want one per invalid stem", files.Warnings)
	}
}

func TestFilesSkipsOversizedFilesAndSaysSo(t *testing.T) {
	service, _ := newService(t)
	dir := service.Dir()
	writeSprite(t, dir, "fat-strip", make([]byte, MaxSpritePNGBytes+1), "{}")
	writeSprite(t, dir, "fat-manifest", []byte("png"), strings.Repeat("x", MaxManifestBytes+1))
	writeSprite(t, dir, "fine", []byte("png"), "{}")

	files := service.Files()
	if len(files.Sprites) != 1 || files.Sprites[0].ID != "fine" {
		t.Fatalf("sprites = %+v, want only the in-bounds pair", files.Sprites)
	}
	if hits := warningsMentioning(files.Warnings, "fat-strip.png"); len(hits) != 1 {
		t.Fatalf("warnings = %v, want the oversized strip named", files.Warnings)
	}
	if hits := warningsMentioning(files.Warnings, "fat-manifest.json"); len(hits) != 1 {
		t.Fatalf("warnings = %v, want the oversized manifest named", files.Warnings)
	}
	// A file exactly AT the cap is fine — the extra byte is what makes it
	// over.
	writeSprite(t, dir, "at-the-cap", []byte("png"), strings.Repeat("x", MaxManifestBytes))
	if len(warningsMentioning(service.Files().Warnings, "at-the-cap")) != 0 {
		t.Fatal("a file exactly at the cap was rejected")
	}
}

func TestFilesStopsAtTheSpriteCountCap(t *testing.T) {
	service, _ := newService(t)
	dir := service.Dir()
	for index := range MaxSprites + 3 {
		writeSprite(t, dir, spriteID(index), []byte("png"), "{}")
	}

	files := service.Files()
	if len(files.Sprites) != MaxSprites {
		t.Fatalf("sprites = %d, want the cap %d", len(files.Sprites), MaxSprites)
	}
	if len(files.Warnings) != 1 || !strings.Contains(files.Warnings[0], "stopped after") {
		t.Fatalf("warnings = %v, want exactly one explaining the stop", files.Warnings)
	}
}

func TestFilesStopsAtTheAggregateByteCap(t *testing.T) {
	service, _ := newService(t)
	dir := service.Dir()
	// Strips at three quarters of the PER-FILE cap, enough of them to
	// overrun the aggregate well before the sprite-count cap does. Both
	// halves of every pair count toward the total, which is why the exact
	// number that fits is asserted as a property rather than a constant.
	chunk := make([]byte, 3<<20)
	written := MaxSpritesBytes/len(chunk) + 2
	if written >= MaxSprites {
		t.Fatalf("test setup would hit the sprite-count cap first (%d sprites)", written)
	}
	for index := range written {
		writeSprite(t, dir, spriteID(index), chunk, "{}")
	}

	files := service.Files()
	if len(files.Sprites) == 0 || len(files.Sprites) >= written {
		t.Fatalf("sprites = %d, want a partial listing out of %d", len(files.Sprites), written)
	}
	carried := 0
	for _, sprite := range files.Sprites {
		decoded, err := base64.StdEncoding.DecodeString(sprite.PNG)
		if err != nil {
			t.Fatal(err)
		}
		carried += len(decoded) + len(sprite.Manifest)
	}
	if carried > MaxSpritesBytes {
		t.Fatalf("listing carries %d bytes, over the %d cap", carried, MaxSpritesBytes)
	}
	if carried+len(chunk) <= MaxSpritesBytes {
		t.Fatalf("listing stopped at %d bytes with room for another strip under the %d cap", carried, MaxSpritesBytes)
	}
	if len(files.Warnings) != 1 || !strings.Contains(files.Warnings[0], "stopped at") {
		t.Fatalf("warnings = %v, want exactly one explaining the stop", files.Warnings)
	}
}

func TestFilesToleratesAMissingDirectoryAndOddEntries(t *testing.T) {
	service, _ := newService(t)

	// A boot that never ran is not an error — it is a fresh install.
	files := service.Files()
	if len(files.Sprites) != 0 || len(files.Warnings) != 0 {
		t.Fatalf("missing directory answered %+v, want an empty, warning-free listing", files)
	}

	dir := service.Dir()
	writeSprite(t, dir, "real", []byte("png"), "{}")
	// A DIRECTORY named like a sprite half is the user's filing decision,
	// not a broken sprite: skipped silently, and it must not make `real`
	// look half-written either.
	if err := os.MkdirAll(filepath.Join(dir, "folder.png"), 0o700); err != nil {
		t.Fatal(err)
	}
	files = service.Files()
	if len(files.Sprites) != 1 || files.Sprites[0].ID != "real" {
		t.Fatalf("sprites = %+v, want only the real pair", files.Sprites)
	}
	if len(files.Warnings) != 0 {
		t.Fatalf("a directory entry produced warnings: %v", files.Warnings)
	}
}

// A symlinked half is skipped silently and takes its pair with it: reading
// it would pull bytes from outside the directory the RPC claims to
// describe.
func TestFilesSkipsSymlinkedHalves(t *testing.T) {
	service, _ := newService(t)
	dir := service.Dir()
	writeSprite(t, dir, "real", []byte("png"), "{}")

	outside := filepath.Join(t.TempDir(), "elsewhere.png")
	if err := os.WriteFile(outside, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked.png")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "linked.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	files := service.Files()
	for _, sprite := range files.Sprites {
		if sprite.ID == "linked" {
			t.Fatal("a symlinked strip was read")
		}
	}
	// The surviving half warns like any other half-sprite — the user did
	// leave a lone .json in the directory.
	if hits := warningsMentioning(files.Warnings, "linked.png"); len(hits) != 1 {
		t.Fatalf("warnings = %v, want the lone sidecar reported", files.Warnings)
	}
}

// A boot that could not create the directory is remembered and retried
// from the next read, so the seed heals the instant the blocker is gone.
func TestBlockedBootHealsOnTheNextRead(t *testing.T) {
	service, configDir := newService(t)
	blocker := filepath.Join(configDir, DirName)
	if err := os.WriteFile(blocker, []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := service.EnsureBoot(); err == nil {
		t.Fatal("a blocked boot reported success")
	}

	// Blocked is degraded, not broken: the listing still answers.
	files := service.Files()
	if len(files.Sprites) != 0 {
		t.Fatalf("blocked listing = %+v, want empty", files.Sprites)
	}

	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if files = service.Files(); len(files.Warnings) != 0 {
		t.Fatalf("healed listing warns: %v", files.Warnings)
	}
	if _, err := os.Stat(filepath.Join(service.Dir(), ReferenceFileName)); err != nil {
		t.Fatalf("the reference was never seeded after unblocking: %v", err)
	}
}

func TestNewFallsBackToTheHomeDirectory(t *testing.T) {
	service, err := New("  ")
	if err != nil {
		t.Fatalf("New(empty): %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	if want := filepath.Join(home, ".agent-overflow", DirName); service.Dir() != want {
		t.Fatalf("Dir() = %q, want %q", service.Dir(), want)
	}
}

// spriteID builds a padded kebab id so the sorted listing order matches
// the creation order.
func spriteID(index int) string {
	return "sprite-" + string(rune('a'+index/26)) + string(rune('a'+index%26))
}

func runtimeSupportsUnixPerms() bool {
	return os.PathSeparator == '/'
}
