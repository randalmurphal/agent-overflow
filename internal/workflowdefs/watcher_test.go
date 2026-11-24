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

func TestWatcherTakesOnlyDefinitionTreesForAChange(t *testing.T) {
	root := t.TempDir()
	observed := startObservedWatcher(t, root)

	// Database and settings churn at the data root is not a definition.
	observed.expectNoChanges(t, "data-root noise",
		fsnotify.Event{Name: filepath.Join(root, "store.db-wal"), Op: fsnotify.Create},
		fsnotify.Event{Name: filepath.Join(root, "store.db-wal"), Op: fsnotify.Rename},
		fsnotify.Event{Name: filepath.Join(root, "settings.json"), Op: fsnotify.Create},
		fsnotify.Event{Name: filepath.Join(root, "projects", "app", "notes.md"), Op: fsnotify.Write})
	observed.expectChanges(t, "definition trees",
		fsnotify.Event{Name: filepath.Join(root, "workflows", "build.yaml"), Op: fsnotify.Create},
		fsnotify.Event{Name: filepath.Join(root, "projects", "app"), Op: fsnotify.Create},
		fsnotify.Event{Name: filepath.Join(root, "projects", "app", "workflows", "nested", "ship.yaml"), Op: fsnotify.Write})
}

// A tree that appears under the root, whole, is watched by the time its
// event is handled: a shared workflows tree and a project's.
func TestWatcherWatchesATreeThatAppears(t *testing.T) {
	root := t.TempDir()
	observed := startObservedWatcher(t, root)
	shared := filepath.Join(root, "workflows", "nested")
	project := filepath.Join(root, "projects", "renamed-project")
	for _, dir := range []string{shared, filepath.Join(project, "workflows", "nested")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	observed.expectChanges(t, "the trees appearing",
		fsnotify.Event{Name: filepath.Join(root, "workflows"), Op: fsnotify.Create},
		fsnotify.Event{Name: project, Op: fsnotify.Create})
	observed.expectWatched(t, shared, filepath.Join(root, "projects"), project, filepath.Join(project, "workflows", "nested"))
}

// A watched project tree removed and created again under the same name is
// watched again. The removal must forget the old watches, or the refresh
// would take the new directories for watched ones.
func TestWatcherRewatchesAProjectRemovedAndCreatedAgain(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "projects", "app")
	workflows := filepath.Join(project, "workflows")
	if err := os.MkdirAll(workflows, 0o700); err != nil {
		t.Fatal(err)
	}
	observed := startObservedWatcher(t, root)
	observed.expectWatched(t, project, workflows)

	if err := os.RemoveAll(project); err != nil {
		t.Fatal(err)
	}
	observed.expectChanges(t, "the project's removal", fsnotify.Event{Name: project, Op: fsnotify.Remove})
	observed.expectNotWatched(t, project, workflows)

	if err := os.MkdirAll(workflows, 0o700); err != nil {
		t.Fatal(err)
	}
	observed.expectChanges(t, "the project created again", fsnotify.Event{Name: project, Op: fsnotify.Create})
	observed.expectWatched(t, project, workflows)
}

// The watches the handler adds are live: a project renamed in, whole, and
// then an edit inside it each reach the callback. Each change leaves its
// name on disk and the test waits for the callback, not for a particular
// event, so this holds however a platform reports them.
func TestWatcherCallsBackForATreeRenamedIn(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	emits := make(chan struct{}, 16)
	watcher, err := NewWatcher(root, 10*time.Millisecond, func() { emits <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watcher.Close() })

	incoming := filepath.Join(t.TempDir(), "renamed-project")
	if err := os.MkdirAll(filepath.Join(incoming, "workflows"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(incoming, "workflows", "ship.yaml"), []byte("id: ship"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(incoming, filepath.Join(root, "projects", "renamed-project")); err != nil {
		t.Fatal(err)
	}
	emitted(t, emits, "the project renamed in")
	edited := filepath.Join(root, "projects", "renamed-project", "workflows", "ship.yaml")
	if err := os.WriteFile(edited, []byte("id: changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	emitted(t, emits, "an edit inside the renamed project")
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
// clock and an event source the test feeds, so the test states the exact
// events and reads from the clock which ones the watcher took for a change.
type observedWatcher struct {
	watcher *Watcher
	clock   *fswatchtest.Clock
	feed    *fswatchtest.Feed
}

func startObservedWatcher(t *testing.T, root string) *observedWatcher {
	t.Helper()
	observed := &observedWatcher{clock: &fswatchtest.Clock{}}
	config := fswatch.Config{Debounce: time.Hour, Emit: func() {}, After: observed.clock.After}
	watcher, err := newWatcher(root, config, func(w *fsnotify.Watcher) fswatch.Source {
		observed.feed = fswatchtest.NewFeed(w)
		return observed.feed.Source()
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

// change hands the watcher event, then one event outside the root, which
// the watcher never takes for a change; when that one is taken, event was
// handled in full. It reports whether event started a debounce wait.
func (o *observedWatcher) change(t *testing.T, event fsnotify.Event) bool {
	t.Helper()
	armed := o.clock.Armed()
	o.feed.Send(t, event)
	o.feed.Send(t, fsnotify.Event{Name: filepath.Join(filepath.Dir(o.watcher.root), "settle"), Op: fsnotify.Write})
	return o.clock.Armed() > armed
}

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

// expectWatched and expectNotWatched read the watch set. The handler
// keeps it on the loop goroutine, and a settled loop is parked waiting for
// the next event, so reading it here races with nothing.
func (o *observedWatcher) expectWatched(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		if _, ok := o.watcher.watched[filepath.Clean(dir)]; !ok {
			t.Fatalf("%s is not watched", dir)
		}
	}
}

func (o *observedWatcher) expectNotWatched(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		if _, ok := o.watcher.watched[filepath.Clean(dir)]; ok {
			t.Fatalf("%s is still watched", dir)
		}
	}
}

// emitted waits for one call of the watcher's callback.
func emitted(t *testing.T, emits <-chan struct{}, why string) {
	t.Helper()
	select {
	case <-emits:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never reached the watcher's callback", why)
	}
}
