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

func TestVerifyProcessIdentityRejectsPartialDestructiveEvidence(t *testing.T) {
	for name, identity := range map[string]ProcessIdentity{
		"missing start time": {Executable: "/bin/agent-overflow", Namespace: "linux"},
		"missing executable": {StartTime: "123", Namespace: "linux"},
		"missing namespace":  {StartTime: "123", Executable: "/bin/agent-overflow"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := VerifyProcessIdentity(999999, identity); err == nil {
				t.Fatal("accepted incomplete process identity")
			}
		})
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

func TestCanonicalPathResolvesExistingSymlinkedAncestorForMissingRoot(t *testing.T) {
	realParent := t.TempDir()
	linkParent := filepath.Join(t.TempDir(), "parent-link")
	if err := os.Symlink(realParent, linkParent); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	wanted := filepath.Join(linkParent, "new-root")
	got, err := CanonicalPath(wanted)
	if err != nil {
		t.Fatalf("CanonicalPath: %v", err)
	}
	canonicalParent, err := filepath.EvalSymlinks(realParent)
	if err != nil {
		t.Fatalf("resolve real parent: %v", err)
	}
	if want := filepath.Join(canonicalParent, "new-root"); got != want {
		t.Fatalf("CanonicalPath(%q) = %q, want %q", wanted, got, want)
	}
}

func TestNormalizeSystemPathDoesNotResolveCallerSymlink(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(root, "caller-link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got := NormalizeSystemPath(filepath.Join(link, "child"))
	if filepath.Base(filepath.Dir(got)) != "caller-link" {
		t.Fatalf("NormalizeSystemPath resolved caller-controlled link: %q", got)
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

func TestListDoesNotMarkForeignNamespacePIDStale(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "registry")
	row := newRow("dddddddd", 1)
	row.PIDNamespace = "pid:[foreign]"
	if err := WriteIn(dir, row); err != nil {
		t.Fatal(err)
	}
	rows, err := ListIn(dir, func(int) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Stale {
		t.Fatalf("foreign namespace row = %+v, want live/unknown", rows)
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

func TestCanonicalValidationRejectsIDAndDataDirMismatch(t *testing.T) {
	root := t.TempDir()
	if err := ValidatePaths("deadbeef", root, filepath.Join(root, "agent-overflow")); err == nil {
		t.Fatal("ValidatePaths accepted an id for another root")
	}
	if err := ValidatePaths(ID(root), root, filepath.Join(t.TempDir(), "agent-overflow")); err == nil {
		t.Fatal("ValidatePaths accepted a data dir outside the root")
	}
}

func TestProcessIdentityMatchesCurrentProcess(t *testing.T) {
	identity, err := CaptureProcessIdentity(os.Getpid())
	if err != nil {
		t.Skipf("process identity unavailable: %v", err)
	}
	if identity.StartTime == "" || identity.Executable == "" || identity.Namespace == "" {
		t.Fatalf("incomplete process identity: %+v", identity)
	}
	if err := VerifyProcessIdentity(os.Getpid(), identity); err != nil {
		t.Fatalf("VerifyProcessIdentity: %v", err)
	}
	identity.StartTime += "-reused"
	if err := VerifyProcessIdentity(os.Getpid(), identity); err == nil {
		t.Fatal("VerifyProcessIdentity accepted a changed birth marker")
	}
}

func TestSameLifecycleRejectsRestartNonce(t *testing.T) {
	a := Identity{IdentityVersion: IdentityVersion, ID: "0123abcd", Mode: ModeHarness, BootNonce: "first"}
	b := a
	b.BootNonce = "second"
	if a.SameLifecycle(b) {
		t.Fatal("different boot nonce treated as one lifecycle")
	}
}

func TestCurrentLauncherIdentityRequiresAllFields(t *testing.T) {
	identity := Identity{IdentityVersion: IdentityVersion, BootNonce: "boot", LauncherPid: 42}
	if err := identity.Validate("/tmp/root", "/tmp/root/agent-overflow"); err == nil {
		t.Fatal("accepted current identity with incomplete launcher identity")
	}
}

func TestCurrentLauncherIdentityRejectsFieldsWithoutPID(t *testing.T) {
	identity := Identity{IdentityVersion: IdentityVersion, BootNonce: "boot", LauncherProfile: string(ModePerf)}
	if err := identity.Validate("/tmp/root", "/tmp/root/agent-overflow"); err == nil {
		t.Fatal("accepted launcher identity fields without a launcher pid")
	}
}

func TestCurrentLauncherIdentityValidatesCrossBoundaryFields(t *testing.T) {
	root := t.TempDir()
	identity := Identity{
		IdentityVersion:          IdentityVersion,
		BootNonce:                "boot",
		LauncherPid:              42,
		LauncherProcessStartTime: "123",
		LauncherExecutablePath:   `C:\\Agent\\agent-overflow.exe`,
		LauncherProfile:          string(ModePerf),
		LauncherDataRoot:         root,
		LauncherWebviewProfile:   `C:\\Agent\\webview-perf`,
		LauncherPIDNamespace:     "windows",
	}
	if err := identity.Validate(root, filepath.Join(root, "agent-overflow")); err != nil {
		t.Fatalf("valid cross-boundary launcher identity rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Identity){
		"birth marker": func(i *Identity) { i.LauncherProcessStartTime = "not-a-number" },
		"namespace":    func(i *Identity) { i.LauncherPIDNamespace = "linux" },
		"profile":      func(i *Identity) { i.LauncherProfile = "prod" },
		"data root":    func(i *Identity) { i.LauncherDataRoot = filepath.Join(root, "other") },
	} {
		copy := identity
		mutate(&copy)
		if err := copy.Validate(root, filepath.Join(root, "agent-overflow")); err == nil {
			t.Errorf("accepted launcher identity with changed %s", name)
		}
	}
}

func TestIsAbsolutePathAcceptsWindowsLauncherPaths(t *testing.T) {
	for _, path := range []string{`C:\\Agent\\agent-overflow.exe`, `\\\\server\\share\\webview`, "/tmp/webview"} {
		if !IsAbsolutePath(path) {
			t.Errorf("IsAbsolutePath(%q) = false", path)
		}
	}
	for _, path := range []string{"C:relative.exe", "relative/webview", ""} {
		if IsAbsolutePath(path) {
			t.Errorf("IsAbsolutePath(%q) = true", path)
		}
	}
}

func TestSameLifecycleRejectsLauncherPIDChange(t *testing.T) {
	a := Identity{IdentityVersion: IdentityVersion, ID: "0123abcd", BootNonce: "boot", LauncherPid: 1}
	b := a
	b.LauncherPid = 2
	if a.SameLifecycle(b) {
		t.Fatal("different launcher pid treated as one lifecycle")
	}
}
