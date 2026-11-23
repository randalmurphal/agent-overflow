package workflowdefs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/fswatch"
	"agent-overflow/internal/fswatch/fswatchtest"
)

func TestWatcherStartsAWaitForDefinitionsAndDiscoversAProjectRename(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "workflows")
	projects := filepath.Join(root, "projects")
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	observed := startObservedWatcher(t, root)

	if err := os.WriteFile(filepath.Join(shared, "build.yaml"), []byte("id: build"), 0o600); err != nil {
		t.Fatal(err)
	}
	observed.expectQueued(t, 0, 1, "a shared definition write")

	outside := t.TempDir()
	incoming := filepath.Join(outside, "renamed-project")
	incomingWorkflows := filepath.Join(incoming, "workflows")
	if err := os.MkdirAll(incomingWorkflows, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(incomingWorkflows, "ship.yaml"), []byte("id: ship"), 0o600); err != nil {
		t.Fatal(err)
	}
	armed := observed.clock.Armed()
	if err := os.Rename(incoming, filepath.Join(projects, "renamed-project")); err != nil {
		t.Fatal(err)
	}
	observed.expectQueued(t, armed, 1, "a project renamed into place")

	// The rename's refresh must have watched the project's workflow tree.
	armed = observed.clock.Armed()
	if err := os.WriteFile(filepath.Join(projects, "renamed-project", "workflows", "ship.yaml"), []byte("id: changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	observed.expectQueued(t, armed, 1, "an edit inside the renamed project")

	armed = observed.clock.Armed()
	noise := filepath.Join(root, "store.db-wal")
	if err := os.WriteFile(noise, []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(noise, filepath.Join(root, "settings.json")); err != nil {
		t.Fatal(err)
	}
	observed.expectIgnored(t, armed, "data-root noise")
}

func TestWatcherDiscoversAWorkflowTreeRenamedIntoTheRoot(t *testing.T) {
	root := t.TempDir()
	observed := startObservedWatcher(t, root)

	staged := filepath.Join(t.TempDir(), "workflows")
	if err := os.MkdirAll(staged, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "ship.yaml"), []byte("id: ship"), 0o600); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(root, "workflows")
	if err := os.Rename(staged, shared); err != nil {
		t.Fatal(err)
	}
	observed.expectQueued(t, 0, 1, "a workflows tree renamed into the root")

	armed := observed.clock.Armed()
	if err := os.WriteFile(filepath.Join(shared, "ship.yaml"), []byte("id: changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	observed.expectQueued(t, armed, 1, "an edit inside the renamed tree")
}

// TestWatcherRewatchesAProjectRemovedAndCreatedAgain removes a watched
// project tree and creates it again under the same name. The removal must
// forget the old watches, or the refresh would take the new directories
// for watched ones and an edit inside them would go unseen.
func TestWatcherRewatchesAProjectRemovedAndCreatedAgain(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "projects", "app")
	workflows := filepath.Join(project, "workflows")
	if err := os.MkdirAll(workflows, 0o700); err != nil {
		t.Fatal(err)
	}
	observed := startObservedWatcher(t, root)

	if err := os.RemoveAll(project); err != nil {
		t.Fatal(err)
	}
	observed.sync(t)
	if err := os.MkdirAll(workflows, 0o700); err != nil {
		t.Fatal(err)
	}
	observed.sync(t)

	armed := observed.clock.Armed()
	if err := os.WriteFile(filepath.Join(workflows, "ship.yaml"), []byte("id: ship"), 0o600); err != nil {
		t.Fatal(err)
	}
	observed.expectQueued(t, armed, 1, "an edit inside the re-created project")
}

func TestWatcherRejectsAMissingRootOrAFileForATree(t *testing.T) {
	if _, err := NewWatcher(filepath.Join(t.TempDir(), "missing"), time.Millisecond, func() {}); err == nil {
		t.Fatal("missing root unexpectedly accepted")
	}
	invalidRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(invalidRoot, "workflows"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWatcher(invalidRoot, time.Millisecond, func() {}); err == nil {
		t.Fatal("workflow file in place of directory unexpectedly accepted")
	}
}

// observedWatcher runs a Watcher with a held debounce clock and reports
// each handled event path, so a test can tell when the loop has handled
// every event it caused and count the debounce waits those events started.
// The waits are never released: what the loop does with them is fswatch's.
type observedWatcher struct {
	watcher *Watcher
	clock   *fswatchtest.Clock
	handled chan string
	next    int
}

func startObservedWatcher(t *testing.T, root string) *observedWatcher {
	t.Helper()
	observed := &observedWatcher{clock: &fswatchtest.Clock{}, handled: make(chan string, 64)}
	watcher, err := newWatcher(root, fswatch.Config{Debounce: time.Hour, Emit: func() {}, After: observed.clock.After}, func(path string) {
		// Never block the loop on the report: Close waits for it. A dropped
		// path fails a waiting sync with a timeout naming its sentinel.
		select {
		case observed.handled <- path:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := watcher.Close(); err != nil {
			t.Errorf("close watcher: %v", err)
		}
	})
	observed.watcher = watcher
	return observed
}

// sync writes a sentinel file at the data root, which the watcher ignores,
// and waits until the loop has handled it. The kernel queues a change's
// events before the syscall returns, and the loop handles them in delivery
// order on one goroutine, so every event from an earlier change has been
// handled too.
func (o *observedWatcher) sync(t *testing.T) {
	t.Helper()
	o.next++
	sentinel := filepath.Join(o.watcher.root, fmt.Sprintf("%d.sentinel", o.next))
	if err := os.WriteFile(sentinel, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case path := <-o.handled:
			if path == sentinel {
				return
			}
		case <-deadline:
			t.Fatalf("the watch loop never handled sentinel %s", sentinel)
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
