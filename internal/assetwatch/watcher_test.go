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
// debounce clock and an event source the test feeds, so the test states the
// exact events and reads from the clock which ones the watcher took for a
// change. The waits are never released: what the loop does with them is
// fswatch's.
type observedWatcher struct {
	core  *watcher
	clock *fswatchtest.Clock
	feed  *fswatchtest.Feed
}

func startObservedWatcher(t *testing.T, label, dir string, relevantName func(name string) bool) *observedWatcher {
	t.Helper()
	observed := &observedWatcher{clock: &fswatchtest.Clock{}}
	config := fswatch.Config{Debounce: time.Hour, Emit: func() {}, After: observed.clock.After}
	core, err := newWatcher(label, dir, relevantName, config, func(w *fsnotify.Watcher) fswatch.Source {
		observed.feed = fswatchtest.NewFeed(w)
		return observed.feed.Source()
	})
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

func (o *observedWatcher) path(name string) string { return filepath.Join(o.core.dir, name) }

// event is an op on name in the watched directory.
func (o *observedWatcher) event(name string, op fsnotify.Op) fsnotify.Event {
	return fsnotify.Event{Name: o.path(name), Op: op}
}

// change hands the watcher event, then one event outside its directory,
// which no watcher takes for a change; when that one is taken, event was
// handled in full. It reports whether event started a debounce wait.
func (o *observedWatcher) change(t *testing.T, event fsnotify.Event) bool {
	t.Helper()
	armed := o.clock.Armed()
	o.feed.Send(t, event)
	o.feed.Send(t, fsnotify.Event{Name: filepath.Join(filepath.Dir(o.core.dir), "settle"), Op: fsnotify.Write})
	return o.clock.Armed() > armed
}

// expectChanges asserts that each event is a change, and expectNoChanges
// that none is.
func (o *observedWatcher) expectChanges(t *testing.T, why string, events ...fsnotify.Event) {
	t.Helper()
	for _, event := range events {
		if !o.change(t, event) {
			t.Fatalf("%s: %v started no debounce wait", why, event)
		}
	}
}

func (o *observedWatcher) expectNoChanges(t *testing.T, why string, events ...fsnotify.Event) {
	t.Helper()
	for _, event := range events {
		if o.change(t, event) {
			t.Fatalf("%s: %v started a debounce wait", why, event)
		}
	}
}

// emitted waits for one call of a watcher's callback.
func emitted(t *testing.T, emits <-chan struct{}, why string) {
	t.Helper()
	select {
	case <-emits:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never reached the watcher's callback", why)
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
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	// The removal is itself a change the frontend has to see, and the
	// handler decides it only after the re-arm returned.
	for _, op := range []fsnotify.Op{fsnotify.Remove, fsnotify.Rename} {
		observed.expectChanges(t, "the directory's "+op.String(), fsnotify.Event{Name: dir, Op: op})
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("the directory was not re-created after its %v: %v", op, err)
		}
	}
}

// The re-armed watch is live: after the directory is removed for real, a
// file placed in the re-created directory reaches the callback. Waiting
// for the callback, not for a particular event, keeps this independent of
// how a platform reports the removal.
func TestWatcherWatchesTheDirectoryItRecreated(t *testing.T) {
	dir := t.TempDir()
	emits := make(chan struct{}, 16)
	config := fswatch.Config{Debounce: 10 * time.Millisecond, Emit: func() { emits <- struct{}{} }}
	core, err := newWatcher("test watcher", dir, jsonRelevant, config, fswatch.FromWatcher)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	// The handler re-creates the directory before it reports the change.
	emitted(t, emits, "the directory's removal")
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("the directory was not re-created: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "after.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	emitted(t, emits, "a file written into the re-created directory")
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
