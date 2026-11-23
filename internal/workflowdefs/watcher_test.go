package workflowdefs

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

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

	// Database and settings churn at the data root is not a definition.
	noise := filepath.Join(root, "store.db-wal")
	observed.place(t, noise, "wal")
	if err := os.Rename(noise, filepath.Join(root, "settings.json")); err != nil {
		t.Fatal(err)
	}
	observed.expectNoChange(t, 0, "data-root noise",
		delivery{noise, fsnotify.Create}, delivery{noise, fsnotify.Rename}, delivery{filepath.Join(root, "settings.json"), fsnotify.Create})

	armed := observed.clock.Armed()
	definition := filepath.Join(shared, "build.yaml")
	observed.place(t, definition, "id: build")
	observed.expectChange(t, armed, definition, fsnotify.Create, "a shared definition")

	outside := t.TempDir()
	incoming := filepath.Join(outside, "renamed-project")
	if err := os.MkdirAll(filepath.Join(incoming, "workflows"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(incoming, "workflows", "ship.yaml"), []byte("id: ship"), 0o600); err != nil {
		t.Fatal(err)
	}
	armed = observed.clock.Armed()
	project := filepath.Join(projects, "renamed-project")
	if err := os.Rename(incoming, project); err != nil {
		t.Fatal(err)
	}
	observed.expectChange(t, armed, project, fsnotify.Create, "a project renamed into place")

	// The rename's refresh must have watched the project's workflow tree:
	// otherwise this edit never reaches the loop.
	armed = observed.clock.Armed()
	edited := filepath.Join(project, "workflows", "ship.yaml")
	if err := os.WriteFile(edited, []byte("id: changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	observed.expectChange(t, armed, edited, fsnotify.Write, "an edit inside the renamed project")
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
	observed.expectChange(t, 0, shared, fsnotify.Create, "a workflows tree renamed into the root")

	armed := observed.clock.Armed()
	edited := filepath.Join(shared, "ship.yaml")
	if err := os.WriteFile(edited, []byte("id: changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	observed.expectChange(t, armed, edited, fsnotify.Write, "an edit inside the renamed tree")
}

// TestWatcherRewatchesAProjectRemovedAndCreatedAgain removes a watched
// project tree and creates it again under the same name, one directory at
// a time. The removal must forget the old watches, or the refresh would
// take the new directories for watched ones and nothing inside them would
// reach the loop.
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
	observed.relay.WaitDelivered(t, project, fsnotify.Remove)
	observed.settle(t)

	armed := observed.clock.Armed()
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	observed.expectChange(t, armed, project, fsnotify.Create, "the project created again")
	armed = observed.clock.Armed()
	if err := os.Mkdir(workflows, 0o700); err != nil {
		t.Fatal(err)
	}
	observed.expectChange(t, armed, workflows, fsnotify.Create, "its workflows directory created again")
	armed = observed.clock.Armed()
	definition := filepath.Join(workflows, "ship.yaml")
	observed.place(t, definition, "id: ship")
	observed.expectChange(t, armed, definition, fsnotify.Create, "a definition in the re-created project")
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

// The options are checked before anything is acquired: a zero debounce is
// the error even for a root that does not exist, before any tree is walked.
func TestWatcherChecksItsOptionsFirst(t *testing.T) {
	_, err := NewWatcher(filepath.Join(t.TempDir(), "missing"), 0, func() {})
	if err == nil || err.Error() != "workflow definitions watcher: debounce must be positive" {
		t.Fatalf("NewWatcher = %v, want the debounce refused first", err)
	}
}

// observedWatcher runs a Watcher over a real root with a held debounce
// clock, its fsnotify events reaching the loop through a relay. A test
// waits for the event it caused, settles the loop, and reads from the
// clock whether a wait was started. The waits are never released: what
// the loop does with them is fswatch's.
type observedWatcher struct {
	watcher *Watcher
	clock   *fswatchtest.Clock
	relay   *fswatchtest.Relay
	// staging is a directory beside the root, where place writes a file
	// before renaming it in.
	staging string
}

// delivery is one event a test waits for.
type delivery struct {
	path string
	op   fsnotify.Op
}

func startObservedWatcher(t *testing.T, root string) *observedWatcher {
	t.Helper()
	observed := &observedWatcher{clock: &fswatchtest.Clock{}, staging: t.TempDir()}
	config := fswatch.Config{Debounce: time.Hour, Emit: func() {}, After: observed.clock.After}
	watcher, err := newWatcher(root, config, func(w *fsnotify.Watcher) fswatch.Source {
		observed.relay = fswatchtest.NewRelay(w)
		return observed.relay.Source()
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

// place puts a new file at path by renaming it in from staging, so its
// arrival is a single create event and no later event for it can land in
// the next step of a test.
func (o *observedWatcher) place(t *testing.T, path, content string) {
	t.Helper()
	staged := filepath.Join(o.staging, filepath.Base(path))
	if err := os.WriteFile(staged, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, path); err != nil {
		t.Fatal(err)
	}
}

// settle hands the loop one event outside the root, which the watcher
// never takes for a change; when it has been taken, every event before it
// has been handled in full.
func (o *observedWatcher) settle(t *testing.T) {
	t.Helper()
	o.relay.Send(t, fsnotify.Event{Name: filepath.Join(filepath.Dir(o.watcher.root), "settle"), Op: fsnotify.Write})
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

// expectNoChange waits for each delivery, settles, and asserts that no
// debounce wait was started since armedBefore.
func (o *observedWatcher) expectNoChange(t *testing.T, armedBefore int, why string, deliveries ...delivery) {
	t.Helper()
	for _, d := range deliveries {
		o.relay.WaitDelivered(t, d.path, d.op)
	}
	o.settle(t)
	if armed := o.clock.Armed() - armedBefore; armed != 0 {
		t.Fatalf("%s started %d debounce waits, want none", why, armed)
	}
}
