package assetwatch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/fswatch"
	"agent-overflow/internal/fswatch/fswatchtest"
)

// observedWatcher runs the shared core with a held debounce clock and a
// sentinel hook, so a test can tell when the loop has handled every event
// it caused and count the debounce waits those events started. The waits
// are never released: what the loop does with them is fswatch's, and this
// package's tests assert only which events start one.
type observedWatcher struct {
	core      *watcher
	clock     *fswatchtest.Clock
	sentinels chan string
	next      int
}

// sentinelExtension names files no watcher in this package treats as
// content. The test predicate reports them before declaring them
// irrelevant.
const sentinelExtension = ".sentinel"

func startObservedWatcher(t *testing.T, label, dir string, relevantName func(name string) bool) *observedWatcher {
	t.Helper()
	observed := &observedWatcher{
		clock:     &fswatchtest.Clock{},
		sentinels: make(chan string, 64),
	}
	observe := func(name string) bool {
		if strings.HasSuffix(name, sentinelExtension) {
			// Never block the loop: Close waits for it. A dropped sentinel
			// fails the waiting test with a timeout naming it.
			select {
			case observed.sentinels <- name:
			default:
			}
			return false
		}
		return relevantName(name)
	}
	core, err := newWatcher(label, dir, observe, fswatch.Config{Debounce: time.Hour, Emit: func() {}, After: observed.clock.After})
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

func (o *observedWatcher) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(o.core.dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// sync writes a sentinel file and waits until the loop has handled it. The
// kernel queues a change's events before the syscall returns, and the loop
// handles them in delivery order on one goroutine, so every event from an
// earlier change has been handled too.
func (o *observedWatcher) sync(t *testing.T) {
	t.Helper()
	o.next++
	name := fmt.Sprintf("%d%s", o.next, sentinelExtension)
	o.write(t, name, "")
	deadline := time.After(5 * time.Second)
	for {
		select {
		case seen := <-o.sentinels:
			if seen == name {
				return
			}
		case <-deadline:
			t.Fatalf("the watch loop never handled sentinel %s", name)
		}
	}
}

// expectQueued syncs and asserts that the changes since armedBefore started
// at least minimum debounce waits.
func (o *observedWatcher) expectQueued(t *testing.T, armedBefore, minimum int, why string) {
	t.Helper()
	o.sync(t)
	if armed := o.clock.Armed() - armedBefore; armed < minimum {
		t.Fatalf("%s started %d debounce waits, want at least %d", why, armed, minimum)
	}
}

// expectIgnored syncs and asserts that the changes since armedBefore
// started no debounce wait.
func (o *observedWatcher) expectIgnored(t *testing.T, armedBefore int, why string) {
	t.Helper()
	o.sync(t)
	if armed := o.clock.Armed() - armedBefore; armed != 0 {
		t.Fatalf("%s started %d debounce waits, want none", why, armed)
	}
}

func jsonRelevant(name string) bool { return filepath.Ext(name) == ".json" }

func TestWatcherRejectsAMissingDirOrAFileInItsPlace(t *testing.T) {
	config := fswatch.Config{Debounce: time.Hour, Emit: func() {}}
	if _, err := newWatcher("test watcher", filepath.Join(t.TempDir(), "missing"), jsonRelevant, config); err == nil {
		t.Fatal("missing dir unexpectedly accepted")
	}
	notADir := filepath.Join(t.TempDir(), "themes")
	if err := os.WriteFile(notADir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newWatcher("test watcher", notADir, jsonRelevant, config); err == nil {
		t.Fatal("file in place of the dir unexpectedly accepted")
	}
}

// Removing the watched directory kills the inotify watch silently: no
// error on the Errors channel, no further events, live reload dead for
// the rest of the process. The watcher must put the directory back and
// re-register, or `rm -rf themes/` would be a one-way door.
func TestWatcherRearmsAfterTheDirectoryIsRemoved(t *testing.T) {
	dir := t.TempDir()
	observed := startObservedWatcher(t, "test watcher", dir, jsonRelevant)
	observed.sync(t)

	armed := observed.clock.Armed()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	// The removal is itself a change the frontend has to see, and the
	// handler starts its wait only after the re-arm returned.
	observed.clock.WaitArmed(t, armed+1)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("the directory was not re-created: %v", err)
	}

	// The new watch is live: a write into the fresh directory is seen.
	armed = observed.clock.Armed()
	observed.write(t, "after.json", "{}")
	observed.expectQueued(t, armed, 1, "a write into the re-created directory")
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
