package design

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newWorkDir(t *testing.T) (*WorkDirManager, string) {
	t.Helper()
	base := t.TempDir()
	return NewWorkDirManager(base), base
}

func TestWorkDir_EnsureThreadCreatesLayoutAndSeedsIndex(t *testing.T) {
	m, base := newWorkDir(t)

	if err := m.EnsureThread("t1"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}
	for _, sub := range []string{"main", "options"} {
		info, err := os.Stat(filepath.Join(base, "t1", sub))
		if err != nil {
			t.Fatalf("missing subdir %s: %v", sub, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", sub)
		}
	}
	idx := filepath.Join(base, "t1", "main", "index.html")
	body, err := os.ReadFile(idx)
	if err != nil {
		t.Fatalf("read seeded index.html: %v", err)
	}
	if !strings.Contains(string(body), "<title>Design preview</title>") {
		t.Fatalf("seeded index.html missing title; got %q", string(body)[:40])
	}
}

func TestWorkDir_EnsureThreadIsIdempotentAndDoesNotClobberIndex(t *testing.T) {
	m, base := newWorkDir(t)
	if err := m.EnsureThread("t1"); err != nil {
		t.Fatalf("first EnsureThread: %v", err)
	}
	idx := filepath.Join(base, "t1", "main", "index.html")
	if err := os.WriteFile(idx, []byte("user-edit"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := m.EnsureThread("t1"); err != nil {
		t.Fatalf("second EnsureThread: %v", err)
	}
	body, err := os.ReadFile(idx)
	if err != nil {
		t.Fatalf("read after second ensure: %v", err)
	}
	if string(body) != "user-edit" {
		t.Fatalf("EnsureThread clobbered existing index.html: %q", string(body))
	}
}

func TestWorkDir_EnsureThreadRejectsBlankOrTraversalSegments(t *testing.T) {
	m, _ := newWorkDir(t)
	cases := []string{"", " ", ".", "..", "a/b", `c\d`}
	for _, id := range cases {
		err := m.EnsureThread(id)
		if err == nil {
			t.Fatalf("EnsureThread(%q) error = nil, want error", id)
		}
	}
}

func TestWorkDir_ListOptionsReturnsLexicallySortedDirNames(t *testing.T) {
	m, _ := newWorkDir(t)
	if err := m.EnsureThread("t1"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}
	// Create three option dirs out of order, each with index.html so
	// the index-presence gate doesn't filter them out.
	for _, opt := range []string{"C", "A", "B"} {
		dir, err := m.OptionsPath("t1", "set1", opt)
		if err != nil {
			t.Fatalf("OptionsPath %s: %v", opt, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<x/>"), 0o644); err != nil {
			t.Fatalf("write index.html for %s: %v", opt, err)
		}
	}
	got, err := m.ListOptions("t1", "set1")
	if err != nil {
		t.Fatalf("ListOptions: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %v, want 3 entries", got)
	}
	// os.ReadDir guarantees lexically sorted entries.
	want := []string{"A", "B", "C"}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("got[%d] = %q, want %q", i, got[i], w)
		}
	}
}

// TestWorkDir_ListOptionsSkipsDirWithoutIndexHTML pins the load-bearing
// behavior for the panel UX: a freshly-mkdir'd option dir whose
// index.html has not landed yet must NOT appear in ListOptions, so the
// frontend's iframe grid never renders blank tiles for empty dirs. The
// regression we're guarding against: agent runs `mkdir A B C`, the
// watcher's debounce window emits one options-update, the frontend
// races to display three iframes — http.FileServer returns either 404
// or a directory listing for index-less paths, and the user sees three
// white boxes.
func TestWorkDir_ListOptionsSkipsDirWithoutIndexHTML(t *testing.T) {
	m, _ := newWorkDir(t)
	if err := m.EnsureThread("t1"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}
	// A has index.html, B is just a dir, C has index.html.
	for _, opt := range []string{"A", "B", "C"} {
		dir, err := m.OptionsPath("t1", "set1", opt)
		if err != nil {
			t.Fatalf("OptionsPath %s: %v", opt, err)
		}
		if opt == "B" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<x/>"), 0o644); err != nil {
			t.Fatalf("write index.html for %s: %v", opt, err)
		}
	}
	got, err := m.ListOptions("t1", "set1")
	if err != nil {
		t.Fatalf("ListOptions: %v", err)
	}
	if len(got) != 2 || got[0] != "A" || got[1] != "C" {
		t.Fatalf("got %v, want [A C] (B has no index.html, must be filtered)", got)
	}
}

func TestWorkDir_ListOptionsReturnsEmptyForMissingSet(t *testing.T) {
	m, _ := newWorkDir(t)
	if err := m.EnsureThread("t1"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}
	got, err := m.ListOptions("t1", "no-such-set")
	if err != nil {
		t.Fatalf("ListOptions on missing set: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

func TestWorkDir_ListOptionsRejectsBlankSetID(t *testing.T) {
	m, _ := newWorkDir(t)
	if _, err := m.ListOptions("t1", ""); err == nil {
		t.Fatal("ListOptions(blank set) error = nil, want error")
	}
}

func TestWorkDir_ListOptionsSkipsDotfiles(t *testing.T) {
	m, base := newWorkDir(t)
	if err := m.EnsureThread("t1"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}
	// Force a dotfile dir into options/set1/. Real agents shouldn't
	// emit one, but defense in depth.
	setDir := filepath.Join(base, "t1", "options", "set1")
	if err := os.MkdirAll(filepath.Join(setDir, ".hidden"), 0o755); err != nil {
		t.Fatalf("mkdir hidden: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(setDir, "A"), 0o755); err != nil {
		t.Fatalf("mkdir A: %v", err)
	}
	if err := os.WriteFile(filepath.Join(setDir, "A", "index.html"), []byte("<x/>"), 0o644); err != nil {
		t.Fatalf("write A index: %v", err)
	}
	got, err := m.ListOptions("t1", "set1")
	if err != nil {
		t.Fatalf("ListOptions: %v", err)
	}
	if len(got) != 1 || got[0] != "A" {
		t.Fatalf("got %v, want [A]", got)
	}
}

func TestWorkDir_ListOptionsSkipsRegularFiles(t *testing.T) {
	m, base := newWorkDir(t)
	if err := m.EnsureThread("t1"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}
	setDir := filepath.Join(base, "t1", "options", "set1")
	if err := os.MkdirAll(setDir, 0o755); err != nil {
		t.Fatalf("mkdir set: %v", err)
	}
	if err := os.WriteFile(filepath.Join(setDir, "stray.html"), []byte("nope"), 0o644); err != nil {
		t.Fatalf("write stray: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(setDir, "A"), 0o755); err != nil {
		t.Fatalf("mkdir A: %v", err)
	}
	if err := os.WriteFile(filepath.Join(setDir, "A", "index.html"), []byte("<x/>"), 0o644); err != nil {
		t.Fatalf("write A index: %v", err)
	}
	got, err := m.ListOptions("t1", "set1")
	if err != nil {
		t.Fatalf("ListOptions: %v", err)
	}
	if len(got) != 1 || got[0] != "A" {
		t.Fatalf("got %v, want [A]", got)
	}
}

// TestWorkDir_LatestUnpickedOptionSetReturnsMostRecent pins the
// load-bearing hydration behavior: the frontend asks the backend on
// pane mount which option set to render, and the answer is the most
// recently touched set with index.html-bearing options and no
// .picked marker. Without this, a refresh / app restart loses the
// in-memory activeOptionSet and the user is left looking at the
// empty main/ placeholder despite their pending picker still
// existing on disk.
func TestWorkDir_LatestUnpickedOptionSetReturnsMostRecent(t *testing.T) {
	m, _ := newWorkDir(t)
	if err := m.EnsureThread("t1"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}
	// Older set: setA with one option.
	dirA, err := m.OptionsPath("t1", "setA", "X")
	if err != nil {
		t.Fatalf("OptionsPath setA: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirA, "index.html"), []byte("<x/>"), 0o644); err != nil {
		t.Fatalf("write A: %v", err)
	}

	// Bump mtime gap so the test isn't flaky on FS timestamp
	// resolution. 50 ms is enough on every FS we run on.
	time.Sleep(50 * time.Millisecond)

	// Newer set: setB with two options.
	for _, opt := range []string{"P", "Q"} {
		d, err := m.OptionsPath("t1", "setB", opt)
		if err != nil {
			t.Fatalf("OptionsPath setB/%s: %v", opt, err)
		}
		if err := os.WriteFile(filepath.Join(d, "index.html"), []byte("<x/>"), 0o644); err != nil {
			t.Fatalf("write %s: %v", opt, err)
		}
	}

	setID, opts, err := m.LatestUnpickedOptionSet("t1")
	if err != nil {
		t.Fatalf("LatestUnpickedOptionSet: %v", err)
	}
	if setID != "setB" {
		t.Fatalf("setID = %q, want setB (newer mtime)", setID)
	}
	if len(opts) != 2 || opts[0] != "P" || opts[1] != "Q" {
		t.Fatalf("opts = %v, want [P Q]", opts)
	}
}

// TestWorkDir_LatestUnpickedOptionSetSkipsPickedSets pins the
// dismissal semantics: once MarkOptionSetPicked writes the .picked
// marker, that set must drop out of the "active" projection so a
// refresh after the user picks doesn't re-render the same picker.
func TestWorkDir_LatestUnpickedOptionSetSkipsPickedSets(t *testing.T) {
	m, _ := newWorkDir(t)
	if err := m.EnsureThread("t1"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}
	for _, set := range []string{"setA", "setB"} {
		d, err := m.OptionsPath("t1", set, "X")
		if err != nil {
			t.Fatalf("OptionsPath %s: %v", set, err)
		}
		if err := os.WriteFile(filepath.Join(d, "index.html"), []byte("<x/>"), 0o644); err != nil {
			t.Fatalf("write index %s: %v", set, err)
		}
	}
	// Mtime ordering: setA created first, setB created later; without
	// any .picked, setB wins.
	if err := m.MarkOptionSetPicked("t1", "setB"); err != nil {
		t.Fatalf("MarkOptionSetPicked setB: %v", err)
	}

	setID, _, err := m.LatestUnpickedOptionSet("t1")
	if err != nil {
		t.Fatalf("LatestUnpickedOptionSet: %v", err)
	}
	if setID != "setA" {
		t.Fatalf("setID = %q, want setA (setB is picked)", setID)
	}

	// Pick setA too — both picked, no active set.
	if err := m.MarkOptionSetPicked("t1", "setA"); err != nil {
		t.Fatalf("MarkOptionSetPicked setA: %v", err)
	}
	setID, opts, err := m.LatestUnpickedOptionSet("t1")
	if err != nil {
		t.Fatalf("LatestUnpickedOptionSet (both picked): %v", err)
	}
	if setID != "" || opts != nil {
		t.Fatalf("after both picked, got setID=%q opts=%v, want empty", setID, opts)
	}
}

// TestWorkDir_LatestUnpickedOptionSetSkipsEmptySet pins that a set
// dir with no index.html-bearing options does NOT count as the
// active set — the agent has just mkdir'd but not yet written, and
// the frontend would render blank iframes if we promoted it.
func TestWorkDir_LatestUnpickedOptionSetSkipsEmptySet(t *testing.T) {
	m, _ := newWorkDir(t)
	if err := m.EnsureThread("t1"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}
	// setA: dir only, no index.html.
	if _, err := m.OptionsPath("t1", "setA", "X"); err != nil {
		t.Fatalf("OptionsPath setA: %v", err)
	}
	// setB: dir AND index.html.
	dirB, err := m.OptionsPath("t1", "setB", "Y")
	if err != nil {
		t.Fatalf("OptionsPath setB: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "index.html"), []byte("<x/>"), 0o644); err != nil {
		t.Fatalf("write B index: %v", err)
	}

	setID, opts, err := m.LatestUnpickedOptionSet("t1")
	if err != nil {
		t.Fatalf("LatestUnpickedOptionSet: %v", err)
	}
	if setID != "setB" {
		t.Fatalf("setID = %q, want setB (setA has no index.html)", setID)
	}
	if len(opts) != 1 || opts[0] != "Y" {
		t.Fatalf("opts = %v, want [Y]", opts)
	}
}

func TestWorkDir_LatestUnpickedOptionSetReturnsEmptyForFreshThread(t *testing.T) {
	m, _ := newWorkDir(t)
	if err := m.EnsureThread("t1"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}
	setID, opts, err := m.LatestUnpickedOptionSet("t1")
	if err != nil {
		t.Fatalf("LatestUnpickedOptionSet: %v", err)
	}
	if setID != "" || opts != nil {
		t.Fatalf("got setID=%q opts=%v, want empty", setID, opts)
	}
}

func TestWorkDir_MarkOptionSetPickedRequiresExistingSet(t *testing.T) {
	m, _ := newWorkDir(t)
	if err := m.EnsureThread("t1"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}
	// Marking a set that doesn't exist must error so a frontend bug
	// doesn't silently leave a marker file dangling under a fabricated
	// path.
	if err := m.MarkOptionSetPicked("t1", "ghost"); err == nil {
		t.Fatal("MarkOptionSetPicked(ghost) error = nil, want error")
	}
}

func TestWorkDir_MarkOptionSetPickedIsIdempotent(t *testing.T) {
	m, _ := newWorkDir(t)
	if err := m.EnsureThread("t1"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}
	d, err := m.OptionsPath("t1", "setA", "X")
	if err != nil {
		t.Fatalf("OptionsPath: %v", err)
	}
	if err := os.WriteFile(filepath.Join(d, "index.html"), []byte("<x/>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := m.MarkOptionSetPicked("t1", "setA"); err != nil {
			t.Fatalf("MarkOptionSetPicked iter %d: %v", i, err)
		}
	}
	picked, err := m.IsOptionSetPicked("t1", "setA")
	if err != nil {
		t.Fatalf("IsOptionSetPicked: %v", err)
	}
	if !picked {
		t.Fatal("IsOptionSetPicked = false after marking, want true")
	}
}

func TestWorkDir_ListMainFilesReturnsTopLevelRegularFiles(t *testing.T) {
	m, base := newWorkDir(t)
	if err := m.EnsureThread("t1"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}
	mainDir := filepath.Join(base, "t1", "main")
	for _, name := range []string{"app.js", "style.css", "index.html"} {
		if err := os.WriteFile(filepath.Join(mainDir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	got, err := m.ListMainFiles("t1")
	if err != nil {
		t.Fatalf("ListMainFiles: %v", err)
	}
	want := []string{"app.js", "index.html", "style.css"}
	if len(got) != len(want) {
		t.Fatalf("ListMainFiles returned %d entries (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i, name := range want {
		if got[i] != name {
			t.Fatalf("ListMainFiles[%d] = %q, want %q (full %v)", i, got[i], name, got)
		}
	}
}

func TestWorkDir_ListMainFilesSkipsDotfilesAndSubdirs(t *testing.T) {
	m, base := newWorkDir(t)
	if err := m.EnsureThread("t1"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}
	mainDir := filepath.Join(base, "t1", "main")
	if err := os.WriteFile(filepath.Join(mainDir, "a.html"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write a.html: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mainDir, ".hidden"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write dotfile: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(mainDir, "components"), 0o755); err != nil {
		t.Fatalf("mkdir components: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mainDir, "components", "nested.html"), []byte("y"), 0o644); err != nil {
		t.Fatalf("write nested.html: %v", err)
	}
	got, err := m.ListMainFiles("t1")
	if err != nil {
		t.Fatalf("ListMainFiles: %v", err)
	}
	// EnsureThread already seeded index.html, so we expect that + a.html.
	want := []string{"a.html", "index.html"}
	if len(got) != len(want) {
		t.Fatalf("ListMainFiles returned %v, want %v", got, want)
	}
	for i, name := range want {
		if got[i] != name {
			t.Fatalf("ListMainFiles[%d] = %q, want %q (full %v)", i, got[i], name, got)
		}
	}
}

func TestWorkDir_ListMainFilesReturnsEmptyForMissingMain(t *testing.T) {
	m, _ := newWorkDir(t)
	got, err := m.ListMainFiles("t1")
	if err != nil {
		t.Fatalf("ListMainFiles on un-ensured thread: %v", err)
	}
	if got == nil {
		t.Fatal("ListMainFiles returned nil; expected empty slice for caller-side json marshal")
	}
	if len(got) != 0 {
		t.Fatalf("ListMainFiles on missing main = %v, want empty", got)
	}
}

// TestWorkDir_ListMainFilesSkipsSymlinks pins the security-relevant
// invariant that ListMainFiles uses entry.Type() (which doesn't follow
// symlinks) rather than Info().Mode() (which does on some
// filesystems). A future "tighten this up" refactor that swaps the
// two would silently start listing symlink targets in the manifest;
// this test fails before that lands. The threat is information
// disclosure: the manifest is interpolated into a chat-thread message
// body and would otherwise leak whatever the symlink points at.
func TestWorkDir_ListMainFilesSkipsSymlinks(t *testing.T) {
	m, base := newWorkDir(t)
	if err := m.EnsureThread("t1"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}
	mainDir := filepath.Join(base, "t1", "main")
	if err := os.WriteFile(filepath.Join(mainDir, "real.html"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write real.html: %v", err)
	}
	// Write a symlink that targets a path outside the workdir; the
	// filter should drop it regardless of whether the target exists.
	target := filepath.Join(t.TempDir(), "outside.html")
	if err := os.WriteFile(target, []byte("leak"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(mainDir, "leak.html")); err != nil {
		t.Skipf("symlink unsupported on this filesystem: %v", err)
	}
	got, err := m.ListMainFiles("t1")
	if err != nil {
		t.Fatalf("ListMainFiles: %v", err)
	}
	for _, name := range got {
		if name == "leak.html" {
			t.Fatalf("ListMainFiles included symlink %q in %v", name, got)
		}
	}
	// Real file + seeded index.html should both be present.
	want := map[string]bool{"index.html": true, "real.html": true}
	for _, name := range got {
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("ListMainFiles missing %v (full result %v)", want, got)
	}
}

func TestWorkDir_ListMainFilesRejectsBlankThreadID(t *testing.T) {
	m, _ := newWorkDir(t)
	if _, err := m.ListMainFiles("  "); err == nil {
		t.Fatal("ListMainFiles with blank thread id returned nil error")
	}
}

func TestWorkDir_OptionsPathSanitizesAndRejectsEscape(t *testing.T) {
	m, base := newWorkDir(t)
	if err := m.EnsureThread("t1"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}
	got, err := m.OptionsPath("t1", "setA", "B")
	if err != nil {
		t.Fatalf("OptionsPath happy path: %v", err)
	}
	want := filepath.Join(base, "t1", "options", "setA", "B")
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	// Slash / dot-dot in either segment is rejected at sanitize time.
	for _, c := range []struct{ set, opt string }{
		{"", "B"},
		{"setA", ""},
		{"..", "B"},
		{"setA", ".."},
		{"set/A", "B"},
		{`set\A`, "B"},
	} {
		if _, err := m.OptionsPath("t1", c.set, c.opt); err == nil {
			t.Fatalf("OptionsPath(%q,%q) error = nil, want error", c.set, c.opt)
		}
	}
}

func TestWorkDir_WipeRemovesEntireThreadTree(t *testing.T) {
	m, base := newWorkDir(t)
	if err := m.EnsureThread("t1"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}
	if err := m.Wipe("t1"); err != nil {
		t.Fatalf("Wipe: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "t1")); !os.IsNotExist(err) {
		t.Fatalf("thread dir still present after wipe: %v", err)
	}
	// Wipe again on missing tree is a no-op.
	if err := m.Wipe("t1"); err != nil {
		t.Fatalf("repeat Wipe: %v", err)
	}
}

func TestWorkDir_SanitizeSegmentRejectsTraversal(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"alpha", "alpha"},
		{"  alpha  ", "alpha"},
		{"", ""},
		{".", ""},
		{"..", ""},
		{"foo/bar", ""},
		{`foo\bar`, ""},
		{"with-dashes", "with-dashes"},
	}
	for _, c := range cases {
		if got := sanitizeSegment(c.in); got != c.want {
			t.Errorf("sanitizeSegment(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
