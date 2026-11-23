package assetwatch

import (
	"path/filepath"
	"testing"

	"agent-overflow/internal/atomicfile"
)

func TestDeviceNameWatcherSeesAtomicReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device-name.json")
	observed := startObservedWatcher(t, "device name watcher", filepath.Dir(path), deviceNameRelevant(path))
	for _, name := range []string{"first", "second"} {
		armed := observed.clock.Armed()
		if err := atomicfile.WriteJSON(path, map[string]string{"name": name}); err != nil {
			t.Fatal(err)
		}
		observed.expectQueued(t, armed, 1, "atomic replacement "+name)
	}

	// The production watcher is wired to the same file: its temp file and
	// its neighbors are not content.
	watcher, err := NewDeviceNameWatcher(path, func() {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watcher.Close() })
	if !watcher.core.relevant(path) {
		t.Fatal("the device-name file is not relevant")
	}
	for _, name := range []string{"device-name.json.tmp-2148294417", "other.json"} {
		if watcher.core.relevant(filepath.Join(filepath.Dir(path), name)) {
			t.Fatalf("%s is treated as the device-name file", name)
		}
	}
}
