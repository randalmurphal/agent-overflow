package assetwatch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"agent-overflow/internal/fswatch"
	"agent-overflow/internal/fswatch/fswatchtest"
)

// observedWatcher runs the shared core over a real directory with a held
// debounce clock, its fsnotify events reaching the loop through a relay.
// A test waits for the event it caused (relay.WaitDelivered), settles the
// loop, and reads from the clock whether a wait was started. The waits are
// never released: what the loop does with them is fswatch's.
type observedWatcher struct {
	core  *watcher
	clock *fswatchtest.Clock
	relay *fswatchtest.Relay
	// staging is a directory beside the watched one, where place writes a
	// file before renaming it in.
	staging string
}

func startObservedWatcher(t *testing.T, label, dir string, relevantName func(name string) bool) *observedWatcher {
	t.Helper()
	observed := &observedWatcher{clock: &fswatchtest.Clock{}, staging: t.TempDir()}
	config := fswatch.Config{Debounce: time.Hour, Emit: func() {}, After: observed.clock.After}
	core, err := newWatcher(label, dir, relevantName, config, observed.relaySource)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := core.Close(); err != nil {
			t.Errorf("close %s: %v", label, err)
		}
	})
	observed.core = core
	return observed
}

func (o *observedWatcher) relaySource(watcher *fsnotify.Watcher) fswatch.Source {
	o.relay = fswatchtest.NewRelay(watcher)
	return o.relay.Source()
}

func (o *observedWatcher) path(name string) string { return filepath.Join(o.core.dir, name) }

// place puts a new file named name into the watched directory by renaming
// it in from staging, so its arrival is a single create event and no later
// event for it can land in the next step of a test.
func (o *observedWatcher) place(t *testing.T, name, content string) {
	t.Helper()
	staged := filepath.Join(o.staging, name)
	if err := os.WriteFile(staged, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, o.path(name)); err != nil {
		t.Fatal(err)
	}
}

// settle hands the loop one event outside the watched directory, which no
// watcher takes for a change; when it has been taken, every event before
// it has been handled in full.
func (o *observedWatcher) settle(t *testing.T) {
	t.Helper()
	o.relay.Send(t, fsnotify.Event{Name: filepath.Join(filepath.Dir(o.core.dir), "settle"), Op: fsnotify.Write})
}

// expectChange waits for the op event on path, settles, and asserts that a
// debounce wait was started since armedBefore.
func (o *observedWatcher) expectChange(t *testing.T, armedBefore int, path string, op fsnotify.Op, why string) {
	t.Helper()
	o.relay.WaitDelivered(t, path, op)
	o.settle(t)
	if o.clock.Armed() <= armedBefore {
		t.Fatalf("%s started no debounce wait", why)
	}
}

// expectNoChange waits for the op event on each path, settles, and asserts
// that no debounce wait was started since armedBefore.
func (o *observedWatcher) expectNoChange(t *testing.T, armedBefore int, op fsnotify.Op, why string, paths ...string) {
	t.Helper()
	for _, path := range paths {
		o.relay.WaitDelivered(t, path, op)
	}
	o.settle(t)
	if armed := o.clock.Armed() - armedBefore; armed != 0 {
		t.Fatalf("%s started %d debounce waits, want none", why, armed)
	}
}

func jsonRelevant(name string) bool { return filepath.Ext(name) == ".json" }

func TestWatcherRejectsAMissingDirOrAFileInItsPlace(t *testing.T) {
	config := fswatch.Config{Debounce: time.Hour, Emit: func() {}}
	if _, err := newWatcher("test watcher", filepath.Join(t.TempDir(), "missing"), jsonRelevant, config, fswatch.FromWatcher); err == nil {
		t.Fatal("missing dir unexpectedly accepted")
	}
	notADir := filepath.Join(t.TempDir(), "themes")
	if err := os.WriteFile(notADir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newWatcher("test watcher", notADir, jsonRelevant, config, fswatch.FromWatcher); err == nil {
		t.Fatal("file in place of the dir unexpectedly accepted")
	}
}

// The options are checked before anything is acquired: a zero debounce is
// the error even for a directory that does not exist.
func TestWatcherChecksItsOptionsFirst(t *testing.T) {
	_, err := newWatcher("test watcher", filepath.Join(t.TempDir(), "missing"), jsonRelevant, fswatch.Config{Emit: func() {}}, fswatch.FromWatcher)
	if err == nil || err.Error() != "test watcher: debounce must be positive" {
		t.Fatalf("newWatcher = %v, want the debounce refused first", err)
	}
}

// Removing the watched directory kills the inotify watch silently: no
// error on the Errors channel, no further events, live reload dead for
// the rest of the process. The watcher must put the directory back and
// re-register, or `rm -rf themes/` would be a one-way door.
func TestWatcherRearmsAfterTheDirectoryIsRemoved(t *testing.T) {
	dir := t.TempDir()
	observed := startObservedWatcher(t, "test watcher", dir, jsonRelevant)

	armed := observed.clock.Armed()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	// The removal is itself a change the frontend has to see, and the
	// handler decides it only after the re-arm returned.
	observed.expectChange(t, armed, dir, fsnotify.Remove, "the directory's removal")
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("the directory was not re-created: %v", err)
	}

	// The new watch is live: a write into the fresh directory is seen.
	armed = observed.clock.Armed()
	observed.place(t, "after.json", "{}")
	observed.expectChange(t, armed, observed.path("after.json"), fsnotify.Create, "a write into the re-created directory")
}

// The watch is not recursive, but a rename can still name a path below the
// watched directory, and nothing there is content.
func TestWatcherIgnoresAPathBelowItsDirectory(t *testing.T) {
	core := &watcher{dir: filepath.Clean(t.TempDir()), relevantName: jsonRelevant}
	if !core.relevant(filepath.Join(core.dir, "tokyo-night.json")) {
		t.Fatal("a file in the directory is not relevant")
	}
	if core.relevant(filepath.Join(core.dir, "backup", "tokyo-night.json")) {
		t.Fatal("a file in a subdirectory is relevant")
	}
}
