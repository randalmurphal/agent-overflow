package instanceinfo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIDIsStableShortAndPerDataRoot(t *testing.T) {
	root := t.TempDir()
	first := ID(root)
	if len(first) != idHexLen {
		t.Fatalf("ID(%q) = %q, want %d chars", root, first, idHexLen)
	}
	if !ValidID(first) {
		t.Fatalf("ID(%q) = %q is not lowercase hex", root, first)
	}
	if again := ID(root); again != first {
		t.Fatalf("ID is not stable: %q then %q", first, again)
	}
	if other := ID(t.TempDir()); other == first {
		t.Fatalf("two data roots produced one id %q", first)
	}
}

func TestIDCanonicalizesSpellingsOfOneRoot(t *testing.T) {
	root := t.TempDir()
	// A relative spelling, a trailing slash, and a doubled separator all
	// name the same directory; an id keyed by the raw string would hand
	// one instance three registry rows.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	want := ID(root)
	for _, spelling := range []string{".", root + string(filepath.Separator), filepath.Join(root, "sub", "..")} {
		if got := ID(spelling); got != want {
			t.Errorf("ID(%q) = %q, want %q", spelling, got, want)
		}
	}
}

func TestIDResolvesSymlinkedRoot(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got, want := ID(link), ID(real); got != want {
		t.Fatalf("ID through symlink = %q, want %q", got, want)
	}
}

func TestIDOfMissingPathFallsBackToLexical(t *testing.T) {
	// The first boot on a fresh data root computes its id before the
	// directory exists; it must match what the same path yields later.
	root := filepath.Join(t.TempDir(), "not-created-yet")
	before := ID(root)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if after := ID(root); after != before {
		t.Fatalf("id changed once the root existed: %q then %q", before, after)
	}
}

func newRow(id string, pid int) Row {
	return Row{
		Identity: Identity{
			ID:        id,
			Mode:      ModeHarness,
			Window:    true,
			Worktree:  "/repo",
			StartedAt: time.Now().UTC().Format(time.RFC3339),
			// A launcher-hosted instance is the case the field exists for;
			// the round-trip test compares whole rows, so a field that
			// failed to survive the file would fail there.
			LauncherPid: 4242,
		},
		PID:      pid,
		Port:     4321,
		DataRoot: "/tmp/root",
		DataDir:  "/tmp/root/agent-overflow",
		Version:  "dev",
	}
}

func TestWriteListRemoveRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "registry")
	row := newRow("0123abcd", os.Getpid())
	if err := WriteIn(dir, row); err != nil {
		t.Fatalf("WriteIn: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "0123abcd.json"))
	if err != nil {
		t.Fatalf("stat row: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("row mode = %v, want 0600", perm)
	}
	if dirInfo, err := os.Stat(dir); err != nil {
		t.Fatalf("stat registry dir: %v", err)
	} else if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("registry dir mode = %v, want 0700", perm)
	}

	got, err := ListIn(dir, nil)
	if err != nil {
		t.Fatalf("ListIn: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListIn returned %d rows, want 1", len(got))
	}
	if got[0].Stale {
		t.Error("row for our own live pid reported stale")
	}
	if got[0].Row != row {
		t.Errorf("round-tripped row = %+v, want %+v", got[0].Row, row)
	}

	if err := RemoveIn(dir, row.ID); err != nil {
		t.Fatalf("RemoveIn: %v", err)
	}
	if got, err := ListIn(dir, nil); err != nil || len(got) != 0 {
		t.Fatalf("after RemoveIn: rows=%v err=%v, want empty", got, err)
	}
	// A second removal is what a reader's stale prune racing graceful
	// shutdown looks like; both are right, neither is an error.
	if err := RemoveIn(dir, row.ID); err != nil {
		t.Fatalf("second RemoveIn: %v", err)
	}
}

func TestListMarksDeadPidsStaleAndSortsNewestFirst(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "registry")
	old := newRow("aaaaaaaa", 4242)
	old.StartedAt = "2020-01-01T00:00:00Z"
	recent := newRow("bbbbbbbb", os.Getpid())
	recent.StartedAt = "2030-01-01T00:00:00Z"
	for _, row := range []Row{old, recent} {
		if err := WriteIn(dir, row); err != nil {
			t.Fatalf("WriteIn %s: %v", row.ID, err)
		}
	}

	alive := func(pid int) bool { return pid == os.Getpid() }
	got, err := ListIn(dir, alive)
	if err != nil {
		t.Fatalf("ListIn: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListIn returned %d rows, want 2", len(got))
	}
	if got[0].ID != recent.ID {
		t.Errorf("first row = %q, want the newest (%q)", got[0].ID, recent.ID)
	}
	if got[0].Stale {
		t.Error("live row reported stale")
	}
	if !got[1].Stale {
		t.Error("row for a dead pid was not reported stale")
	}
	if got[1].Path != filepath.Join(dir, old.ID+".json") {
		t.Errorf("Path = %q, want the file the row was read from", got[1].Path)
	}
}

func TestListSkipsJunkAndMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "registry")
	if rows, err := ListIn(dir, nil); err != nil || rows != nil {
		t.Fatalf("missing registry: rows=%v err=%v, want (nil, nil)", rows, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write junk: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatalf("write non-json: %v", err)
	}
	healthy := newRow("cccccccc", os.Getpid())
	if err := WriteIn(dir, healthy); err != nil {
		t.Fatalf("WriteIn: %v", err)
	}
	got, err := ListIn(dir, nil)
	if err != nil {
		t.Fatalf("ListIn: %v", err)
	}
	if len(got) != 1 || got[0].ID != healthy.ID {
		t.Fatalf("ListIn = %+v, want only the healthy row", got)
	}
}

func TestWriteRefusesAnIDThatIsNotAnID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "registry")
	for _, id := range []string{"", "../escape", "0123abc", "0123ABCD", "0123abcd/x", "zzzzzzzz"} {
		if err := WriteIn(dir, newRow(id, 1)); err == nil {
			t.Errorf("WriteIn accepted id %q", id)
		}
		if err := RemoveIn(dir, id); err == nil {
			t.Errorf("RemoveIn accepted id %q", id)
		}
	}
}

func TestRowJSONShapeIsFlat(t *testing.T) {
	// Readers (the W2 CLI, other tools) parse this by field name; the
	// embedded Identity must flatten rather than nest.
	data, err := json.Marshal(newRow("0123abcd", 7))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"id", "pid", "mode", "window", "port", "dataRoot", "dataDir", "worktree", "version", "startedAt", "launcherPid"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("registry row is missing %q: %s", key, data)
		}
	}
	if _, ok := raw["token"]; ok {
		t.Errorf("registry row carries a token; it must not: %s", data)
	}

	// launcherPid is omitempty: every instance that is NOT hosted by the
	// Windows launcher has none, and a reader must see its absence rather
	// than a zero it could mistake for a pid.
	unhosted := newRow("0123abcd", 7)
	unhosted.LauncherPid = 0
	data, err = json.Marshal(unhosted)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "launcherPid") {
		t.Errorf("row with no launcher still spells launcherPid: %s", data)
	}
}

func TestProcessAliveOnOurselvesAndOnNobody(t *testing.T) {
	if !ProcessAlive(os.Getpid()) {
		t.Error("ProcessAlive says this process is dead")
	}
	if ProcessAlive(0) || ProcessAlive(-1) {
		t.Error("ProcessAlive accepted a non-pid")
	}
}
