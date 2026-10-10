package localimage

import (
	"bytes"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/attachment"
)

// logRecorder collects what a service logs.
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *logRecorder) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *logRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.lines)
}

// newTestService is a service over a fresh cache directory.
func newTestService(t *testing.T) *Service {
	t.Helper()
	return newService(filepath.Join(t.TempDir(), "images"), DiskCacheBytes, t.Logf)
}

// countingService is a service over dir that counts its derivations and
// records what it logs.
func countingService(dir string, diskBytes int64) (*Service, *atomic.Int32, *logRecorder) {
	logs := &logRecorder{}
	s := newService(dir, diskBytes, logs.logf)
	derivations := &atomic.Int32{}
	s.derive = func(key string, src []byte, mime string, maxWidth int) (attachment.Derived, error) {
		derivations.Add(1)
		return attachment.Derive(key, src, mime, maxWidth)
	}
	return s, derivations, logs
}

// derivativeCharge is what one 320 tier derivative of payload is charged
// against a disk bound.
func derivativeCharge(t *testing.T, payload []byte) int64 {
	t.Helper()
	derived, err := attachment.Derive(t.Name(), payload, "image/png", 300)
	if err != nil || !derived.Derived {
		t.Fatalf("derive probe = %v (derived %v)", err, derived.Derived)
	}
	return chargeFor(int64(len(derived.Data)))
}

func resolve(t *testing.T, s *Service, path, workspace string) Image {
	t.Helper()
	got, err := s.Resolve(path, workspace, 300)
	if err != nil {
		t.Fatalf("Resolve %s: %v", filepath.Base(path), err)
	}
	if !got.Derived || got.Width != 320 {
		t.Fatalf("Resolve %s = %+v, want a 320 wide derivative", filepath.Base(path), got)
	}
	return got
}

// storedFile is the disk store path the entry behind contentID names.
func storedFile(t *testing.T, s *Service, contentID string) string {
	t.Helper()
	held, ok := s.cache.Get(contentID)
	if !ok || held.Value.file == "" {
		t.Fatalf("content %q is not a stored derivative", contentID)
	}
	return held.Value.file
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", path, err)
	}
	return err == nil
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func assertPNG(t *testing.T, data []byte, width, height int) {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil || img.Bounds().Dx() != width || img.Bounds().Dy() != height {
		t.Fatalf("served %d bytes (%v), want a whole %dx%d png", len(data), err, width, height)
	}
}

// A second service over the same directory, which is the next process,
// serves the derivative the first one wrote without deriving it again, and
// the memory cache holds no derivative bytes for either.
func TestADerivativeIsReusedAcrossRestarts(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "images")
	workspace := t.TempDir()
	payload := pngBytes(t, 641, 480)
	path := writeFile(t, workspace, "shot.png", payload)

	first, firstDerivations, _ := countingService(dir, DiskCacheBytes)
	made := resolve(t, first, path, workspace)
	if got := firstDerivations.Load(); got != 1 {
		t.Fatalf("first service derived %d times, want 1", got)
	}
	content, served := readAll(t, first, made.ContentID)
	assertPNG(t, served, 320, 240)
	if _, isFile := content.Content.(*os.File); !isFile {
		t.Fatalf("a derivative is served from %T, want its file in the cache", content.Content)
	}
	resident := first.cache.Bytes()
	t.Logf("memory cache holds %d bytes after deriving a %d byte derivative", resident, len(served))
	if resident >= 1024 {
		t.Fatalf("memory cache holds %d bytes for a stored derivative, want only its metadata", resident)
	}

	second, secondDerivations, _ := countingService(dir, DiskCacheBytes)
	reused := resolve(t, second, path, workspace)
	if got := secondDerivations.Load(); got != 0 {
		t.Fatalf("the restarted service derived %d times, want the stored file reused", got)
	}
	_, again := readAll(t, second, reused.ContentID)
	if !bytes.Equal(again, served) {
		t.Fatal("the restarted service served different bytes")
	}
	made.ContentID, reused.ContentID = "", ""
	if reused != made {
		t.Fatalf("reused = %+v, want what the first service answered, %+v", reused, made)
	}
	t.Logf("derivations across the restart: %d", firstDerivations.Load()+secondDerivations.Load())
}

// The final name only ever appears by renaming a complete, synced temp
// file; a temp file a previous process left is removed at startup and never
// served; a failed rename leaves nothing behind.
func TestAnUnfinishedWriteIsNeverServed(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "images")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	payload := pngBytes(t, 641, 480)
	leftover := filepath.Join(dir, tempPrefix+"1234")
	if err := os.WriteFile(leftover, payload[:len(payload)/2], 0o600); err != nil {
		t.Fatalf("write leftover: %v", err)
	}
	// Not the store's: left alone, and logged once.
	notes := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notes, []byte("mine"), 0o600); err != nil {
		t.Fatalf("write notes: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	workspace := t.TempDir()
	path := writeFile(t, workspace, "shot.png", payload)

	s, derivations, logs := countingService(dir, DiskCacheBytes)
	if exists(t, leftover) {
		t.Fatal("the scan kept an unfinished write")
	}
	if !exists(t, notes) || !exists(t, filepath.Join(dir, "sub")) {
		t.Fatal("the scan removed entries that are not the store's")
	}
	if lines := logs.snapshot(); len(lines) != 1 || !strings.Contains(lines[0], "2 unrecognized") {
		t.Fatalf("scan logged %q, want one line naming the two unrecognized entries", lines)
	}

	renames := 0
	s.disk.rename = func(oldpath, newpath string) error {
		renames++
		if !strings.HasPrefix(filepath.Base(oldpath), tempPrefix) || filepath.Dir(oldpath) != dir {
			t.Errorf("renamed from %s, want a temp name in the cache directory", oldpath)
		}
		if exists(t, newpath) {
			t.Errorf("%s exists before the rename", newpath)
		}
		written, err := os.ReadFile(oldpath)
		if err != nil {
			t.Errorf("read the temp file: %v", err)
		}
		assertPNG(t, written, 320, 240)
		return os.Rename(oldpath, newpath)
	}
	got := resolve(t, s, path, workspace)
	if renames != 1 || derivations.Load() != 1 {
		t.Fatalf("renames = %d, derivations = %d, want one of each", renames, derivations.Load())
	}
	_, served := readAll(t, s, got.ContentID)
	assertPNG(t, served, 320, 240)

	// A rename that fails leaves neither name, and the derivative is
	// served from memory.
	failing, _, failingLogs := countingService(filepath.Join(t.TempDir(), "images"), DiskCacheBytes)
	failing.disk.rename = func(string, string) error { return os.ErrPermission }
	held := resolve(t, failing, path, workspace)
	if names := dirNames(t, failing.disk.dir); len(names) != 0 {
		t.Fatalf("a failed rename left %v", names)
	}
	content, fromMemory := readAll(t, failing, held.ContentID)
	if _, isFile := content.Content.(*os.File); isFile {
		t.Fatal("a derivative the store refused is served from a file")
	}
	assertPNG(t, fromMemory, 320, 240)
	if lines := failingLogs.snapshot(); len(lines) != 1 {
		t.Fatalf("a failed store logged %q, want one line", lines)
	}
}

// The bound evicts the least recently used file. A file a restarted service
// reused is newer than one it did not, in memory and in its mtime, which is
// what the next scan orders by.
func TestEvictionKeepsTheMostRecentlyUsedFiles(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "images")
	workspace := t.TempDir()
	payload := pngBytes(t, 641, 480)
	bound := 2 * derivativeCharge(t, payload)
	a := writeFile(t, workspace, "a.png", payload)
	b := writeFile(t, workspace, "b.png", payload)
	c := writeFile(t, workspace, "c.png", payload)

	first, _, _ := countingService(dir, bound)
	fileA := storedFile(t, first, resolve(t, first, a, workspace).ContentID)
	fileB := storedFile(t, first, resolve(t, first, b, workspace).ContentID)
	now := time.Now()
	for path, age := range map[string]time.Duration{fileA: 2 * time.Hour, fileB: time.Hour} {
		if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatalf("age %s: %v", path, err)
		}
	}

	second, derivations, _ := countingService(dir, bound)
	resolve(t, second, a, workspace)
	if derivations.Load() != 0 {
		t.Fatal("the restarted service derived a stored file again")
	}
	info, err := os.Stat(fileA)
	if err != nil {
		t.Fatalf("stat a: %v", err)
	}
	if !info.ModTime().After(now.Add(-time.Hour)) {
		t.Fatalf("a reused file kept its mtime %v, want it touched", info.ModTime())
	}
	fileC := storedFile(t, second, resolve(t, second, c, workspace).ContentID)
	if exists(t, fileB) {
		t.Fatal("the least recently used file survived the bound")
	}
	if !exists(t, fileA) || !exists(t, fileC) {
		t.Fatal("the bound evicted a recently used file")
	}
	if names := dirNames(t, dir); len(names) != 2 {
		t.Fatalf("cache holds %v, want two files", names)
	}
	if held := second.disk.bytes(); held > bound {
		t.Fatalf("cache charges %d bytes, over its %d byte bound", held, bound)
	}
}

// A bound lowered between runs applies at the scan, oldest first.
func TestTheScanEnforcesTheBound(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "images")
	workspace := t.TempDir()
	payload := pngBytes(t, 641, 480)
	charge := derivativeCharge(t, payload)
	first, _, _ := countingService(dir, DiskCacheBytes)
	fileA := storedFile(t, first, resolve(t, first, writeFile(t, workspace, "a.png", payload), workspace).ContentID)
	fileB := storedFile(t, first, resolve(t, first, writeFile(t, workspace, "b.png", payload), workspace).ContentID)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(fileB, old, old); err != nil {
		t.Fatalf("age b: %v", err)
	}

	second, _, _ := countingService(dir, charge)
	if exists(t, fileB) || !exists(t, fileA) {
		t.Fatalf("after a scan under a one-file bound: a %v, b %v; want only the newer a", exists(t, fileA), exists(t, fileB))
	}
	if held := second.disk.bytes(); held != charge {
		t.Fatalf("cache charges %d bytes, want %d", held, charge)
	}
}

// A new version of a source is a new file; the old version is no longer
// used and is the first the bound evicts.
func TestAChangedSourceGetsANewFile(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "images")
	workspace := t.TempDir()
	payload := pngBytes(t, 641, 480)
	path := writeFile(t, workspace, "shot.png", payload)
	other := writeFile(t, workspace, "other.png", payload)
	s, derivations, _ := countingService(dir, 2*derivativeCharge(t, payload))

	before := storedFile(t, s, resolve(t, s, path, workspace).ContentID)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	touched := info.ModTime().Add(time.Second)
	if err := os.Chtimes(path, touched, touched); err != nil {
		t.Fatalf("touch: %v", err)
	}
	after := storedFile(t, s, resolve(t, s, path, workspace).ContentID)
	if after == before || derivations.Load() != 2 {
		t.Fatalf("a new mtime reused %s (derivations %d), want a new file", before, derivations.Load())
	}
	if !exists(t, before) || !exists(t, after) {
		t.Fatal("both versions should be stored until the bound needs room")
	}
	resolve(t, s, other, workspace)
	if exists(t, before) || !exists(t, after) {
		t.Fatal("the bound kept the old version over the current one")
	}
}

// A derivative deleted after it was resolved fails Open, which the route
// answers 404; the client's re-mint resolves again, which derives and
// stores it again instead of answering the same dead id.
func TestOpenAfterTheFileWasDeletedDerivesAgain(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "images")
	workspace := t.TempDir()
	path := writeFile(t, workspace, "shot.png", pngBytes(t, 641, 480))
	s, derivations, _ := countingService(dir, DiskCacheBytes)

	got := resolve(t, s, path, workspace)
	file := storedFile(t, s, got.ContentID)
	if err := os.Remove(file); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := s.Open(got.ContentID); err == nil {
		t.Fatal("Open served a derivative whose file was deleted")
	}
	again := resolve(t, s, path, workspace)
	if derivations.Load() != 2 {
		t.Fatalf("derivations = %d, want the deleted file derived again", derivations.Load())
	}
	if again.ContentID == got.ContentID {
		t.Fatal("the resolve after the failed Open answered the dead id")
	}
	_, served := readAll(t, s, again.ContentID)
	assertPNG(t, served, 320, 240)
	if !exists(t, file) {
		t.Fatal("the derivative was not stored again")
	}
}

// A memory entry whose file the bound evicted is not answered: the resolve
// derives again, so the client never fetches an id that would 404.
func TestResolveDerivesAgainWhatTheBoundEvicted(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "images")
	workspace := t.TempDir()
	payload := pngBytes(t, 641, 480)
	a := writeFile(t, workspace, "a.png", payload)
	b := writeFile(t, workspace, "b.png", payload)
	s, derivations, _ := countingService(dir, derivativeCharge(t, payload))

	first := resolve(t, s, a, workspace)
	resolve(t, s, b, workspace)
	again := resolve(t, s, a, workspace)
	if derivations.Load() != 3 || again.ContentID == first.ContentID {
		t.Fatalf("derivations = %d, same id %v; want a evicted by b and derived again", derivations.Load(), again.ContentID == first.ContentID)
	}
	_, served := readAll(t, s, again.ContentID)
	assertPNG(t, served, 320, 240)
}

// A cache directory that cannot be created or written keeps every
// derivative served from memory, and the cause is logged once however many
// derivatives it refuses.
func TestAnUnwritableCacheServesFromMemoryAndLogsOnce(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	payload := pngBytes(t, 641, 480)
	a := writeFile(t, workspace, "a.png", payload)
	b := writeFile(t, workspace, "b.png", payload)

	notADir := writeFile(t, t.TempDir(), "file", []byte("x"))
	replaced := filepath.Join(t.TempDir(), "images")
	cases := map[string]func(t *testing.T) (*Service, *logRecorder){
		"no directory": func(*testing.T) (*Service, *logRecorder) {
			s, _, logs := countingService("", DiskCacheBytes)
			return s, logs
		},
		"directory under a file": func(*testing.T) (*Service, *logRecorder) {
			s, _, logs := countingService(filepath.Join(notADir, "images"), DiskCacheBytes)
			return s, logs
		},
		"directory replaced by a file after the scan": func(t *testing.T) (*Service, *logRecorder) {
			s, _, logs := countingService(replaced, DiskCacheBytes)
			if err := os.Remove(replaced); err != nil {
				t.Fatalf("remove: %v", err)
			}
			if err := os.WriteFile(replaced, []byte("x"), 0o600); err != nil {
				t.Fatalf("replace: %v", err)
			}
			return s, logs
		},
	}
	for name, open := range cases {
		s, logs := open(t)
		var heldBytes int
		for _, path := range []string{a, b} {
			got := resolve(t, s, path, workspace)
			content, served := readAll(t, s, got.ContentID)
			if _, isFile := content.Content.(*os.File); isFile {
				t.Fatalf("%s: served from a file", name)
			}
			assertPNG(t, served, 320, 240)
			heldBytes += len(served)
		}
		if lines := logs.snapshot(); len(lines) != 1 {
			t.Fatalf("%s: logged %q, want one line", name, lines)
		}
		if resident := s.cache.Bytes(); resident < int64(heldBytes) {
			t.Fatalf("%s: memory cache holds %d bytes, want the %d derivative bytes", name, resident, heldBytes)
		}
	}
}
