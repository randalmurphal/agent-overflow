package assetwatch

import (
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/fswatch"
	"agent-overflow/internal/fswatch/fswatchtest"
)

// Both desktop processes replace the device-name file atomically: a temp
// file renamed onto it. Each event naming the file starts a debounce wait,
// the temp file and the neighbors start none, the last wait calls the
// watcher's callback, and Close closes what it watches.
func TestDeviceNameWatcherTakesItsFileAndCallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device-name.json")
	clock := &fswatchtest.Clock{}
	var feed *fswatchtest.Feed
	emits := make(chan struct{}, 4)
	watcher, err := newDeviceNameWatcher(path, func() { emits <- struct{}{} }, clock.After, func(w *fsnotify.Watcher) fswatch.Source {
		feed = fswatchtest.NewFeed(w)
		return feed.Source()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watcher.Close() })
	observed := &observedWatcher{core: watcher.core, clock: clock, feed: feed}

	observed.expectNoChanges(t, "the temp file and a neighbor",
		observed.event("device-name.json.tmp-2148294417", fsnotify.Create), observed.event("device-name.json.tmp-2148294417", fsnotify.Rename),
		observed.event("other.json", fsnotify.Write))
	observed.expectChanges(t, "replacements of the device-name file",
		observed.event("device-name.json", fsnotify.Create), observed.event("device-name.json", fsnotify.Create),
		observed.event("device-name.json", fsnotify.Remove))
	clock.Fire(t, clock.Armed()-1)
	emitted(t, emits, "the debounce")
	if err := watcher.Close(); err != nil {
		t.Fatal(err)
	}
	if !feed.Closed() {
		t.Fatal("Close left the watched directory's watcher open")
	}
}

// The public constructor wires the caller's callback: a real atomic
// replacement reaches it, however the platform reports the replacement.
func TestNewDeviceNameWatcherCallsBackOnAnAtomicReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device-name.json")
	if err := atomicfile.WriteJSON(path, map[string]string{"name": "first"}); err != nil {
		t.Fatal(err)
	}
	emits := make(chan struct{}, 4)
	watcher, err := NewDeviceNameWatcher(path, func() { emits <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watcher.Close() })
	if err := atomicfile.WriteJSON(path, map[string]string{"name": "second"}); err != nil {
		t.Fatal(err)
	}
	emitted(t, emits, "the replacement")
}
