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
	// Create three option dirs out of order.
	for _, opt := range []string{"C", "A", "B"} {
		if _, err := m.OptionsPath("t1", "set1", opt); err != nil {
			t.Fatalf("OptionsPath %s: %v", opt, err)
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
	got, err := m.ListOptions("t1", "set1")
	if err != nil {
		t.Fatalf("ListOptions: %v", err)
	}
	if len(got) != 1 || got[0] != "A" {
		t.Fatalf("got %v, want [A]", got)
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
