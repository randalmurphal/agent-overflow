package uitrace

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAppendWritesJSONLines(t *testing.T) {
	tr, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	path, err := tr.Append([]string{
		`{"label":"chat.state","data":{"threadId":"thread-1"}}`,
		`{"label":"chat.dom"}`,
	})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	if path != tr.Path() {
		t.Fatalf("Append path = %q, want %q", path, tr.Path())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace file: %v", err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(got) != 2 {
		t.Fatalf("line count = %d, want 2; data=%q", len(got), data)
	}
	if got[0] != `{"label":"chat.state","data":{"threadId":"thread-1"}}` {
		t.Fatalf("first line = %q", got[0])
	}
}

func TestAppendUsesPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not stable on Windows")
	}
	dir := t.TempDir()
	traceDir := filepath.Join(dir, DirName)
	if err := os.MkdirAll(traceDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	tr, err := New(dir)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	if _, err := tr.Append([]string{`{"label":"chat.state"}`}); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	assertMode(t, traceDir, 0o700)
	assertMode(t, tr.Path(), 0o600)
}

func TestAppendEmptyBatchIsNoOp(t *testing.T) {
	tr, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	path, err := tr.Append(nil)
	if err != nil {
		t.Fatalf("Append(nil) returned error: %v", err)
	}
	if path != tr.Path() {
		t.Fatalf("Append(nil) path = %q, want %q", path, tr.Path())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no trace file on empty batch, stat err = %v", err)
	}
}

func TestAppendRejectsInvalidJSON(t *testing.T) {
	tr, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	_, err = tr.Append([]string{`{"label":`})
	if err == nil {
		t.Fatal("Append returned nil error")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("error = %q, want invalid JSON message", err)
	}
}

func TestAppendRejectsOversizedLine(t *testing.T) {
	tr, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	line := `{"value":"` + strings.Repeat("x", MaxLineBytes) + `"}`

	_, err = tr.Append([]string{line})
	if err == nil {
		t.Fatal("Append returned nil error")
	}
	if !strings.Contains(err.Error(), "max") {
		t.Fatalf("error = %q, want max size message", err)
	}
}

func TestAppendRejectsOversizedBatch(t *testing.T) {
	tr, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	line := `{"value":"` + strings.Repeat("x", 3000) + `"}`
	lines := make([]string, 0, MaxBatchLines)
	for i := 0; i < MaxBatchLines; i++ {
		lines = append(lines, line)
	}

	_, err = tr.Append(lines)
	if err == nil {
		t.Fatal("Append returned nil error")
	}
	if !strings.Contains(err.Error(), "batch") {
		t.Fatalf("error = %q, want batch size message", err)
	}
}

func TestAppendRejectsTooManyLines(t *testing.T) {
	tr, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	lines := make([]string, MaxBatchLines+1)
	for i := range lines {
		lines[i] = `{"x":1}`
	}

	_, err = tr.Append(lines)
	if err == nil {
		t.Fatal("Append returned nil error")
	}
	if !strings.Contains(err.Error(), "max") {
		t.Fatalf("error = %q, want max lines message", err)
	}
}

func TestAppendSkipsBlankLines(t *testing.T) {
	tr, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	path, err := tr.Append([]string{"", "   ", `{"a":1}`, "\r\n"})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace file: %v", err)
	}
	if strings.TrimSpace(string(data)) != `{"a":1}` {
		t.Fatalf("content = %q, want single JSON line", data)
	}
}

func TestAppendStripsTrailingNewlines(t *testing.T) {
	tr, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	path, err := tr.Append([]string{`{"a":1}` + "\r\n"})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace file: %v", err)
	}
	if string(data) != `{"a":1}`+"\n" {
		t.Fatalf("content = %q, want single normalized line", data)
	}
}

func TestAppendRotatesAtMaxFileBytes(t *testing.T) {
	dir := t.TempDir()
	tr, err := New(dir)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	// Pre-seed the trace file at MaxFileBytes so any non-empty append
	// pushes size+pending past the cap and forces rotation.
	if err := os.MkdirAll(filepath.Join(dir, DirName), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	bulk := strings.Repeat("a", MaxFileBytes)
	if err := os.WriteFile(tr.Path(), []byte(bulk), 0644); err != nil {
		t.Fatalf("seed trace file: %v", err)
	}

	if _, err := tr.Append([]string{`{"a":1}`}); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	rotated, err := os.ReadFile(tr.Path() + ".1")
	if err != nil {
		t.Fatalf("read rotated file: %v", err)
	}
	if len(rotated) != len(bulk) {
		t.Fatalf("rotated size = %d, want %d", len(rotated), len(bulk))
	}

	current, err := os.ReadFile(tr.Path())
	if err != nil {
		t.Fatalf("read current file: %v", err)
	}
	if string(current) != `{"a":1}`+"\n" {
		t.Fatalf("current file = %q, want single appended line", current)
	}
}

func TestAppendReplacesPreviousRotation(t *testing.T) {
	dir := t.TempDir()
	tr, err := New(dir)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(dir, DirName), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(tr.Path()+".1", []byte("stale"), 0644); err != nil {
		t.Fatalf("seed stale rotation: %v", err)
	}
	bulk := strings.Repeat("b", MaxFileBytes)
	if err := os.WriteFile(tr.Path(), []byte(bulk), 0644); err != nil {
		t.Fatalf("seed trace file: %v", err)
	}

	if _, err := tr.Append([]string{`{"a":1}`}); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	rotated, err := os.ReadFile(tr.Path() + ".1")
	if err != nil {
		t.Fatalf("read rotated file: %v", err)
	}
	if string(rotated) == "stale" {
		t.Fatal("stale rotation was not replaced")
	}
}

func TestNewRequiresConfigDir(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("New(\"\") returned nil error")
	}
}

func TestPathLayout(t *testing.T) {
	dir := t.TempDir()
	tr, err := New(dir)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	want := filepath.Join(dir, DirName, FileName)
	if tr.Path() != want {
		t.Fatalf("Path() = %q, want %q", tr.Path(), want)
	}
}

func TestBookmarkConcatenatesRotationAndCurrent(t *testing.T) {
	dir := t.TempDir()
	tr, err := New(dir)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, DirName), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Pre-seed rotation + current with distinct content so the bookmark
	// concatenation order is observable (older history first).
	if err := os.WriteFile(tr.Path()+".1", []byte(`{"seq":1}`+"\n"), 0644); err != nil {
		t.Fatalf("seed rotation: %v", err)
	}
	if err := os.WriteFile(tr.Path(), []byte(`{"seq":2}`+"\n"), 0644); err != nil {
		t.Fatalf("seed current: %v", err)
	}

	bookmark, err := tr.Bookmark(time.Date(2026, 5, 19, 20, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Bookmark returned error: %v", err)
	}
	want := filepath.Join(dir, DirName, BookmarkSubdir, "bug-report-20260519T203000Z.jsonl")
	if bookmark != want {
		t.Fatalf("bookmark path = %q, want %q", bookmark, want)
	}

	data, err := os.ReadFile(bookmark)
	if err != nil {
		t.Fatalf("read bookmark: %v", err)
	}
	wantContent := `{"seq":1}` + "\n" + `{"seq":2}` + "\n"
	if string(data) != wantContent {
		t.Fatalf("bookmark content = %q, want %q", data, wantContent)
	}
}

func TestBookmarkWhenOnlyCurrentExists(t *testing.T) {
	dir := t.TempDir()
	tr, err := New(dir)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, DirName), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(tr.Path(), []byte(`{"seq":1}`+"\n"), 0644); err != nil {
		t.Fatalf("seed current: %v", err)
	}

	bookmark, err := tr.Bookmark(time.Date(2026, 5, 19, 21, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Bookmark returned error: %v", err)
	}
	data, err := os.ReadFile(bookmark)
	if err != nil {
		t.Fatalf("read bookmark: %v", err)
	}
	if string(data) != `{"seq":1}`+"\n" {
		t.Fatalf("bookmark content = %q, want single line", data)
	}
}

func TestBookmarkWhenNoTraceExistsReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	tr, err := New(dir)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	bookmark, err := tr.Bookmark(time.Date(2026, 5, 19, 22, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Bookmark returned error: %v", err)
	}
	if bookmark != "" {
		t.Fatalf("bookmark path = %q, want empty string when no trace data exists", bookmark)
	}
	// No empty file should be left behind to pollute the bookmarks dir.
	bookmarkDir := filepath.Join(dir, DirName, BookmarkSubdir)
	entries, err := os.ReadDir(bookmarkDir)
	if err != nil {
		t.Fatalf("read bookmark dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("bookmark dir has %d entries, want 0", len(entries))
	}
}

func TestPruneBookmarksOlderThan(t *testing.T) {
	dir := t.TempDir()
	bookmarkDir := filepath.Join(dir, DirName, BookmarkSubdir)
	if err := os.MkdirAll(bookmarkDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	now := time.Now()
	cutoff := now.Add(-7 * 24 * time.Hour)

	files := []struct {
		name     string
		mtime    time.Time
		wantGone bool
		describe string
	}{
		{
			name:     "bug-report-20260101T120000Z.jsonl",
			mtime:    now.Add(-30 * 24 * time.Hour),
			wantGone: true,
			describe: "old bookmark: removed",
		},
		{
			name:     "bug-report-20260520T120000Z.jsonl",
			mtime:    now.Add(-1 * time.Hour),
			wantGone: false,
			describe: "recent bookmark: kept",
		},
		{
			name:     "bug-report-edge.jsonl",
			mtime:    cutoff, // mtime equal to cutoff is NOT strictly older
			wantGone: false,
			describe: "boundary mtime: kept (strict <)",
		},
		{
			name:     "not-a-bookmark.txt",
			mtime:    now.Add(-30 * 24 * time.Hour),
			wantGone: false,
			describe: "unrelated file: ignored",
		},
		{
			name:     "bug-report-no-extension",
			mtime:    now.Add(-30 * 24 * time.Hour),
			wantGone: false,
			describe: "missing .jsonl suffix: ignored",
		},
	}

	for _, f := range files {
		path := filepath.Join(bookmarkDir, f.name)
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", f.name, err)
		}
		if err := os.Chtimes(path, f.mtime, f.mtime); err != nil {
			t.Fatalf("chtimes %s: %v", f.name, err)
		}
	}

	got, err := PruneBookmarksOlderThan(dir, cutoff)
	if err != nil {
		t.Fatalf("PruneBookmarksOlderThan: %v", err)
	}

	wantRemoved := 0
	for _, f := range files {
		if f.wantGone {
			wantRemoved++
		}
	}
	if got != wantRemoved {
		t.Fatalf("removed count = %d, want %d", got, wantRemoved)
	}
	for _, f := range files {
		_, err := os.Stat(filepath.Join(bookmarkDir, f.name))
		switch {
		case f.wantGone && err == nil:
			t.Errorf("%s (%s): still present", f.name, f.describe)
		case !f.wantGone && err != nil:
			t.Errorf("%s (%s): missing: %v", f.name, f.describe, err)
		}
	}
}

func TestPruneBookmarksOlderThanMissingDir(t *testing.T) {
	n, err := PruneBookmarksOlderThan(t.TempDir(), time.Now())
	if err != nil {
		t.Fatalf("missing dir should be (0, nil): %v", err)
	}
	if n != 0 {
		t.Fatalf("missing dir count = %d, want 0", n)
	}
}

func TestPruneBookmarksOlderThanEmptyConfigDir(t *testing.T) {
	if _, err := PruneBookmarksOlderThan("", time.Now()); err == nil {
		t.Fatal("empty configDir should return an error to match New's contract")
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode %s = %o, want %o", path, got, want)
	}
}
