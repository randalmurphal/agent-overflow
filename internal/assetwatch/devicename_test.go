package assetwatch

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/fswatch"
	"agent-overflow/internal/fswatch/fswatchtest"
)

// Both desktop processes replace the device-name file atomically. Each
// replacement reaches the production watcher as a change, the debounce
// that follows calls the callback the watcher was built with, and Close
// closes what it watches.
func TestDeviceNameWatcherSeesAtomicReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device-name.json")
	clock := &fswatchtest.Clock{}
	var relay *fswatchtest.Relay
	emitted := make(chan struct{}, 4)
	watcher, err := newDeviceNameWatcher(path, func() { emitted <- struct{}{} }, clock.After, func(w *fsnotify.Watcher) fswatch.Source {
		relay = fswatchtest.NewRelay(w)
		return relay.Source()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watcher.Close() })
	settle := func() {
		relay.Send(t, fsnotify.Event{Name: filepath.Join(filepath.Dir(path), "other.json"), Op: fsnotify.Write})
	}

	for _, name := range []string{"first", "second"} {
		armed := clock.Armed()
		if err := atomicfile.WriteJSON(path, map[string]string{"name": name}); err != nil {
			t.Fatal(err)
		}
		relay.WaitDelivered(t, path, fsnotify.Create)
		settle()
		if clock.Armed() <= armed {
			t.Fatalf("replacement %s started no debounce wait", name)
		}
	}
	clock.Fire(t, clock.Armed()-1)
	select {
	case <-emitted:
	case <-time.After(5 * time.Second):
		t.Fatal("the debounce never called the watcher's callback")
	}
	if err := watcher.Close(); err != nil {
		t.Fatal(err)
	}
	if !relay.Closed() {
		t.Fatal("Close left the watched directory's watcher open")
	}

	// The production constructor is wired to the same file: its temp file
	// and its neighbors are not content.
	production, err := NewDeviceNameWatcher(path, func() {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = production.Close() })
	if !production.core.relevant(path) {
		t.Fatal("the device-name file is not relevant")
	}
	for _, name := range []string{"device-name.json.tmp-2148294417", "other.json"} {
		if production.core.relevant(filepath.Join(filepath.Dir(path), name)) {
			t.Fatalf("%s is treated as the device-name file", name)
		}
	}
}
