package design

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

func newWorkDir(t *testing.T) (*WorkDirManager, *store.Store, string) {
	t.Helper()
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// design_snapshots has FK to threads; threads has FK to projects.
	now := time.Now().UnixMilli()
	if err := s.CreateProject(store.Project{
		ID:        "p1",
		Path:      t.TempDir(),
		Name:      "p1",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := s.CreateThread(store.Thread{
		ID:              "t1",
		ProjectID:       "p1",
		ProjectPath:     "/tmp",
		Title:           "design",
		Provider:        "claude",
		Model:           "claude-sonnet-4-6",
		Mode:            "design",
		ReasoningEffort: "medium",
		CreatedAt:       now,
		UpdatedAt:       now,
	}); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	base := t.TempDir()
	return NewWorkDirManager(base, s), s, base
}

func TestWorkDir_EnsureThreadCreatesLayoutAndSeedsIndex(t *testing.T) {
	m, _, base := newWorkDir(t)

	if err := m.EnsureThread("t1"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}
	for _, sub := range []string{"main", "options", "snapshots"} {
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
	m, _, base := newWorkDir(t)
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
	m, _, _ := newWorkDir(t)
	cases := []string{"", " ", ".", "..", "a/b", `c\d`}
	for _, id := range cases {
		err := m.EnsureThread(id)
		if err == nil {
			t.Fatalf("EnsureThread(%q) error = nil, want error", id)
		}
	}
}

func TestWorkDir_ListOptionsReturnsLexicallySortedDirNames(t *testing.T) {
	m, _, _ := newWorkDir(t)
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
	m, _, _ := newWorkDir(t)
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
	m, _, _ := newWorkDir(t)
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
	m, _, _ := newWorkDir(t)
	if _, err := m.ListOptions("t1", ""); err == nil {
		t.Fatal("ListOptions(blank set) error = nil, want error")
	}
}

func TestWorkDir_ListOptionsSkipsDotfiles(t *testing.T) {
	m, _, base := newWorkDir(t)
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
	m, _, base := newWorkDir(t)
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
	m, _, _ := newWorkDir(t)
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
	m, _, _ := newWorkDir(t)
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
	m, _, _ := newWorkDir(t)
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
	m, _, _ := newWorkDir(t)
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
	m, _, _ := newWorkDir(t)
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
	m, _, _ := newWorkDir(t)
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

func TestWorkDir_OptionsPathSanitizesAndRejectsEscape(t *testing.T) {
	m, _, base := newWorkDir(t)
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

func TestWorkDir_SnapshotCopiesMainAndPersistsRow(t *testing.T) {
	m, s, _ := newWorkDir(t)
	if err := m.EnsureThread("t1"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}
	mainPath, _ := m.MainPath("t1")
	if err := os.WriteFile(filepath.Join(mainPath, "style.css"), []byte("body{}"), 0o644); err != nil {
		t.Fatalf("seed style: %v", err)
	}

	snap, err := m.Snapshot("t1", SnapshotSpec{Label: "v1", Auto: false})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.ID == "" {
		t.Fatal("snapshot ID empty")
	}
	if snap.Label != "v1" {
		t.Fatalf("Label = %q, want v1", snap.Label)
	}
	if snap.Auto {
		t.Fatal("Auto = true, want false")
	}
	if snap.CreatedAt == 0 {
		t.Fatal("CreatedAt = 0")
	}

	// Snapshot dir holds a copy of style.css.
	snapStyle := filepath.Join(snap.DirPath, "style.css")
	body, err := os.ReadFile(snapStyle)
	if err != nil {
		t.Fatalf("read snap style: %v", err)
	}
	if string(body) != "body{}" {
		t.Fatalf("snap style = %q", string(body))
	}

	// Persisted row matches.
	got, err := s.GetDesignSnapshot("t1", snap.ID)
	if err != nil {
		t.Fatalf("GetDesignSnapshot: %v", err)
	}
	if got.ID != snap.ID || got.Label != "v1" || got.DirPath != snap.DirPath {
		t.Fatalf("row mismatch: %+v vs %+v", got, snap)
	}
}

func TestWorkDir_RestoreFromSnapshotIsAtomicOnContents(t *testing.T) {
	m, _, _ := newWorkDir(t)
	if err := m.EnsureThread("t1"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}
	mainPath, _ := m.MainPath("t1")
	if err := os.WriteFile(filepath.Join(mainPath, "v1.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatalf("seed v1: %v", err)
	}
	v1, err := m.Snapshot("t1", SnapshotSpec{Label: "v1"})
	if err != nil {
		t.Fatalf("Snapshot v1: %v", err)
	}

	// Replace main with v2 contents (different file, no v1.txt).
	_ = os.Remove(filepath.Join(mainPath, "v1.txt"))
	if err := os.WriteFile(filepath.Join(mainPath, "v2.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatalf("seed v2: %v", err)
	}

	if err := m.RestoreFromSnapshot("t1", v1.ID); err != nil {
		t.Fatalf("RestoreFromSnapshot: %v", err)
	}
	// v1.txt is back.
	body, err := os.ReadFile(filepath.Join(mainPath, "v1.txt"))
	if err != nil {
		t.Fatalf("v1.txt missing after restore: %v", err)
	}
	if string(body) != "v1" {
		t.Fatalf("v1.txt = %q", string(body))
	}
	// v2.txt is gone — restore replaced main wholesale.
	if _, err := os.Stat(filepath.Join(mainPath, "v2.txt")); !os.IsNotExist(err) {
		t.Fatalf("v2.txt still present after restore: %v", err)
	}
	// Original snapshot dir untouched.
	if _, err := os.Stat(filepath.Join(v1.DirPath, "v1.txt")); err != nil {
		t.Fatalf("snapshot dir lost: %v", err)
	}
}

func TestWorkDir_RestoreFromUnknownSnapshotErrors(t *testing.T) {
	m, _, _ := newWorkDir(t)
	if err := m.EnsureThread("t1"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}
	if err := m.RestoreFromSnapshot("t1", "no-such-id"); err == nil {
		t.Fatal("RestoreFromSnapshot(unknown) = nil, want error")
	}
}

func TestWorkDir_PruneSnapshotsKeepsManualAndCapsAutos(t *testing.T) {
	m, s, _ := newWorkDir(t)
	if err := m.EnsureThread("t1"); err != nil {
		t.Fatalf("EnsureThread: %v", err)
	}

	// One manual snapshot — must survive the prune regardless.
	manual, err := m.Snapshot("t1", SnapshotSpec{Label: "manual"})
	if err != nil {
		t.Fatalf("manual snapshot: %v", err)
	}

	// Create SnapshotRetentionLimit + 5 auto snapshots. Sleep 1ms between
	// each so created_at values are distinct and the newest-first ordering
	// in PruneSnapshots is deterministic.
	for i := 0; i < SnapshotRetentionLimit+5; i++ {
		if _, err := m.Snapshot("t1", SnapshotSpec{Auto: true}); err != nil {
			t.Fatalf("auto snapshot %d: %v", i, err)
		}
		time.Sleep(time.Millisecond)
	}

	pruned, err := m.PruneSnapshots("t1")
	if err != nil {
		t.Fatalf("PruneSnapshots: %v", err)
	}
	if len(pruned) != 5 {
		t.Fatalf("pruned %d, want 5", len(pruned))
	}
	// Manual must still be there.
	if _, err := s.GetDesignSnapshot("t1", manual.ID); err != nil {
		t.Fatalf("manual snapshot lost: %v", err)
	}
	// Verify count: manual + 20 auto kept = 21.
	got, err := s.ListDesignSnapshots("t1")
	if err != nil {
		t.Fatalf("ListDesignSnapshots: %v", err)
	}
	if len(got) != SnapshotRetentionLimit+1 {
		t.Fatalf("post-prune count = %d, want %d", len(got), SnapshotRetentionLimit+1)
	}
}

func TestWorkDir_WipeRemovesEntireThreadTree(t *testing.T) {
	m, _, base := newWorkDir(t)
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
