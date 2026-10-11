package cilog

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func logFiles(t *testing.T, dir string) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "*"+logExt))
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// A 5000-byte log is charged two blocks.
var fiveKB = strings.Repeat(strings.Repeat("x", 99)+"\n", 50)

// The cache keeps its published files under the bound, dropping the least
// recently used log (its file and metadata) first; a read counts as a use
// and a pinned log is never dropped.
func TestTheCacheEvictsTheLeastRecentlyUsedLog(t *testing.T) {
	t.Parallel()
	s, dir, logs := newTestStore(t, 3*2*cacheBlock)
	a, b, c, d := githubKey("1"), githubKey("2"), githubKey("3"), githubKey("4")
	fetchText(t, s, a, fiveKB, `"a"`)
	fetchText(t, s, b, fiveKB, `"b"`)
	fetchText(t, s, c, fiveKB, `"c"`)
	if s.Bytes() != 3*2*cacheBlock || len(logFiles(t, dir)) != 3 {
		t.Fatalf("three logs charge %d in %d files", s.Bytes(), len(logFiles(t, dir)))
	}
	// a is read, so b is now the oldest.
	if _, err := s.Lines(a, 0, 1, 100); err != nil {
		t.Fatal(err)
	}
	fetchText(t, s, d, fiveKB, `"d"`)
	if s.Has(b) || !s.Has(a) || !s.Has(c) || !s.Has(d) {
		t.Fatalf("after a fourth log: a %v b %v c %v d %v; want b dropped", s.Has(a), s.Has(b), s.Has(c), s.Has(d))
	}
	if s.Bytes() > 3*2*cacheBlock || len(logFiles(t, dir)) != 3 {
		t.Fatalf("cache holds %d bytes in %d files over its bound", s.Bytes(), len(logFiles(t, dir)))
	}
	if _, err := os.Stat(filepath.Join(dir, b.id()+metaExt)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the dropped log's metadata stayed: %v", err)
	}
	// A pinned log stays however old; the bound takes the others.
	s.Pin(c)
	fetchText(t, s, b, fiveKB, `"b"`)
	fetchText(t, s, githubKey("5"), fiveKB, `"e"`)
	if !s.Has(c) {
		t.Fatal("the pinned log was evicted")
	}
	s.Unpin(c)
	fetchText(t, s, githubKey("6"), fiveKB, `"f"`)
	fetchText(t, s, githubKey("7"), fiveKB, `"g"`)
	if s.Has(c) {
		t.Fatal("an unpinned old log stayed over the bound")
	}
	if lines := logs.snapshot(); len(lines) != 0 {
		t.Fatalf("eviction logged %q", lines)
	}
}

// A log larger than the whole bound stays published for the read that
// asked for it, until another log needs the room.
func TestALogLargerThanTheBoundStaysForItsRead(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, cacheBlock)
	key := githubKey("1")
	fetchText(t, s, key, fiveKB, `"a"`)
	if got := allLines(t, s, key); len(got) != 50 {
		t.Fatalf("read %d lines of the large log", len(got))
	}
	fetchText(t, s, githubKey("2"), "x\n", `"b"`)
	if s.Has(key) {
		t.Fatal("the large log stayed once another needed the room")
	}
}

// A read holds the version it reads: publishing a newer one, or evicting
// it, removes its file only once the read is done.
func TestAReadHoldsItsVersionAcrossAReplace(t *testing.T) {
	t.Parallel()
	s, dir, _ := newTestStore(t, CacheBytes)
	key := githubKey("1")
	fetchText(t, s, key, "old\n", `"1"`)
	_, held, err := s.acquire(key)
	if err != nil {
		t.Fatal(err)
	}
	fetchText(t, s, key, "new\n", `"2"`)
	if _, err := os.Stat(held.path); err != nil {
		t.Fatalf("the read's file is gone while it is held: %v", err)
	}
	if got := allLines(t, s, key); !slices.Equal(got, []string{"new"}) {
		t.Fatalf("lines after the replace = %q", got)
	}
	s.release(held)
	if _, err := os.Stat(held.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the replaced file stayed after its read: %v", err)
	}
	if files := logFiles(t, dir); len(files) != 1 {
		t.Fatalf("files %v, want the current version only", files)
	}
}

// A cache directory deleted while the store runs (a user clearing it) is
// made again by the next download, with the index emptied: nothing it
// named survived. A read of a log that went with it says so, and nothing
// is logged.
func TestADeletedCacheDirectoryIsRecreated(t *testing.T) {
	t.Parallel()
	s, dir, logs := newTestStore(t, CacheBytes)
	a, b := githubKey("1"), gitlabKey("2")
	fetchText(t, s, a, "a\n", `"a"`)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	fetchText(t, s, b, "b\n", `"b"`)
	if got := allLines(t, s, b); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("log after the delete = %q", got)
	}
	if s.Has(a) {
		t.Fatal("the index still holds a log whose file was deleted")
	}
	if _, err := s.Lines(a, 0, 1, 100); !errors.Is(err, ErrNoLog) {
		t.Fatalf("read of the deleted log = %v", err)
	}
	if files := logFiles(t, dir); len(files) != 1 || s.Bytes() != chargeFor(2) {
		t.Fatalf("recreated directory holds %v charging %d", files, s.Bytes())
	}
	// A read of a log whose file alone went missing drops it.
	if err := os.Remove(logFiles(t, dir)[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lines(b, 0, 1, 100); !errors.Is(err, ErrLogGone) || s.Has(b) {
		t.Fatalf("read of a missing file = %v, still held %v", err, s.Has(b))
	}
	if lines := logs.snapshot(); len(lines) != 0 {
		t.Fatalf("a deleted directory logged %q", lines)
	}
}

// The logs a previous process kept are served after a restart: their
// lines and sections, revalidated by their ETag. An unfinished download
// and a file without metadata are removed.
func TestAReopenedStoreServesItsLogs(t *testing.T) {
	t.Parallel()
	s, dir, _ := newTestStore(t, CacheBytes)
	gh, gl := githubKey("1"), gitlabKey("2")
	fixture := loadFixtureJob(t, "svelte-check")
	fetchText(t, s, gh, utf8BOM+fixture.Log, `"gh"`)
	fetchText(t, s, gl, testTrace, `"gl"`)
	wantGH, _ := s.Index(gh, fixture.job())
	wantGL, _ := s.Index(gl, Job{Name: "unit", Status: "failed"})
	if err := os.WriteFile(filepath.Join(dir, partPrefix+"left"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "0123-9.log"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	logs := &logSink{}
	reopened := Open(dir, CacheBytes, logs.logf)
	gotGH, err := reopened.Index(gh, fixture.job())
	if err != nil || !slices.Equal(gotGH.Sections, wantGH.Sections) || gotGH.Version != wantGH.Version {
		t.Fatalf("reopened GitHub index = %+v, %v", gotGH, err)
	}
	gotGL, err := reopened.Index(gl, Job{Name: "unit", Status: "failed"})
	if err != nil || !slices.Equal(gotGL.Sections, wantGL.Sections) {
		t.Fatalf("reopened GitLab index = %+v, %v", gotGL, err)
	}
	if got, want := allLines(t, reopened, gl), allLines(t, s, gl); !slices.Equal(got, want) {
		t.Fatalf("reopened lines differ")
	}
	forge := &fakeForge{}
	forge.set(testTrace, `"gl"`)
	if result, err := reopened.Fetch(t.Context(), gl, forge.source()); err != nil || result.Changed || forge.lastRequest().ETag != `"gl"` {
		t.Fatalf("revalidation after a restart = %+v, %v, %+v", result, err, forge.lastRequest())
	}
	if files := logFiles(t, dir); len(files) != 2 || len(partFiles(t, dir)) != 0 {
		t.Fatalf("after the reopen: logs %v, parts %v", files, partFiles(t, dir))
	}
	if lines := logs.snapshot(); len(lines) != 0 {
		t.Fatalf("reopen logged %q", lines)
	}
}

// A file the store cannot remove is reported, once per cause, and its
// record still goes so the bound does not loop on it.
func TestACleanupFailureIsReported(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root removes files from a read-only directory")
	}
	s, dir, logs := newTestStore(t, 2*cacheBlock)
	a, b := githubKey("1"), githubKey("2")
	s.Pin(a)
	s.Pin(b)
	fetchText(t, s, a, fiveKB, `"a"`)
	fetchText(t, s, b, fiveKB, `"b"`)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	s.Unpin(a)
	if s.Has(a) || s.Bytes() != 2*cacheBlock {
		t.Fatalf("the unremovable log is still charged: has %v, %d bytes", s.Has(a), s.Bytes())
	}
	lines := logs.snapshot()
	if len(lines) != 2 || !strings.Contains(lines[0], "remove CI log: ") || !strings.Contains(lines[1], "remove CI log metadata: ") {
		t.Fatalf("logged %q, want the failed removals of the file and its metadata", lines)
	}
	// The same cause again is not logged again.
	s.Unpin(b)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s.Pin(b)
	fetchText(t, s, githubKey("3"), fiveKB, `"c"`)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	s.Unpin(b)
	if got := logs.snapshot(); len(got) != 2 {
		t.Fatalf("a repeated cause logged again: %q", got)
	}
}
