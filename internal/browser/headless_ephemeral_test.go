package browser

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The sweep is tested against a PREDICATE rather than real pids: what is
// worth pinning is which directories it will delete, and a test that had
// to arrange a real dead process would be pinning the operating system.

// ephemeralFixture lays out one temp root full of candidate directories.
func ephemeralFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mark := func(name string, owner string) {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(dir, "chromium"), 0o700); err != nil {
			t.Fatalf("lay out %s: %v", name, err)
		}
		if owner != "" {
			if err := os.WriteFile(filepath.Join(dir, ephemeralOwnerFile), []byte(owner), 0o600); err != nil {
				t.Fatalf("mark %s: %v", name, err)
			}
		}
	}
	mark(ephemeralDirPrefix+"dead", "4242")
	mark(ephemeralDirPrefix+"live", "777")
	mark(ephemeralDirPrefix+"unmarked", "")
	mark(ephemeralDirPrefix+"garbled", "not-a-pid\n")
	mark("someone-elses-tempdir", "4242")
	if err := os.WriteFile(filepath.Join(root, ephemeralDirPrefix+"a-file"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write the decoy file: %v", err)
	}
	return root
}

func remaining(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

// TestTheSweepRemovesOnlyTheRootsOfDeadOwners. A backend that was killed
// or lost power ran no Dispose, so its ephemeral profile, a whole
// Chromium cookie jar, is still in the temp directory. The marker is what
// makes reclaiming it safe: two backends share that directory, and a sweep
// that went by NAME would delete the live session of the other one.
func TestTheSweepRemovesOnlyTheRootsOfDeadOwners(t *testing.T) {
	root := ephemeralFixture(t)
	var asked []int
	sweepEphemeralRoots(root, func(pid int) bool {
		asked = append(asked, pid)
		return pid == 777
	}, func(string, ...any) {})

	want := []string{
		ephemeralDirPrefix + "a-file",
		ephemeralDirPrefix + "garbled",
		ephemeralDirPrefix + "live",
		ephemeralDirPrefix + "unmarked",
		"someone-elses-tempdir",
	}
	got := remaining(t, root)
	if len(got) != len(want) {
		t.Fatalf("after the sweep the root holds %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("after the sweep the root holds %v, want %v", got, want)
		}
	}
	// A directory this process cannot attribute is never even asked
	// about, which is what keeps "cannot say" from resolving to delete.
	sort.Ints(asked)
	if len(asked) != 2 || asked[0] != 777 || asked[1] != 4242 {
		t.Fatalf("the sweep asked about %v, want only the two readable markers", asked)
	}
}

// An empty root sweeps nothing, which is what keeps a unit test's engine
// off the machine's real temp directory.
func TestTheSweepWithNoRootTouchesNothing(t *testing.T) {
	sweepEphemeralRoots("", func(int) bool {
		t.Fatal("the sweep read a process table with no root to sweep")
		return false
	}, func(string, ...any) {})
}

// The marker round-trips, which is the whole contract between a run that
// creates a root and the later run that reclaims it.
func TestTheEphemeralOwnerMarkerRoundTrips(t *testing.T) {
	root := t.TempDir()
	if err := writeEphemeralOwner(root); err != nil {
		t.Fatalf("write the marker: %v", err)
	}
	pid, ok := readEphemeralOwner(root)
	if !ok || pid != os.Getpid() {
		t.Fatalf("read back %d/%v, want this process %d", pid, ok, os.Getpid())
	}
	if !ownerAlive(pid) {
		t.Fatal("this process reports itself dead")
	}
}

// A profile created ephemerally is marked, so the run that follows a crash
// can attribute it. Without this the sweep has nothing to act on and the
// whole mechanism is inert.
func TestAnEphemeralProfileMarksItsRoot(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)

	if profile.ephemeralRoot == "" {
		t.Fatal("an ephemeral profile got no root of its own")
	}
	if filepath.Dir(profile.ephemeralRoot) != engine.tempRoot {
		t.Fatalf("the ephemeral root %q is not under the engine's temp root %q", profile.ephemeralRoot, engine.tempRoot)
	}
	pid, ok := readEphemeralOwner(profile.ephemeralRoot)
	if !ok || pid != os.Getpid() {
		t.Fatalf("the root's owner marker reads %d/%v, want this process %d", pid, ok, os.Getpid())
	}
}
