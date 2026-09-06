package assetwatch

import (
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/atomicfile"
)

func TestDeviceNameWatcherSeesAtomicReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device-name.json")
	changed := make(chan struct{}, 4)
	watcher, err := NewDeviceNameWatcher(path, func() { changed <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	for _, name := range []string{"first", "second"} {
		if err := atomicfile.WriteJSON(path, map[string]string{"name": name}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-changed:
		case <-time.After(2 * time.Second):
			t.Fatal("atomic replacement not observed")
		}
	}
	if err := watcher.Close(); err != nil {
		t.Fatal(err)
	}
	if err := atomicfile.WriteJSON(path, map[string]string{"name": "closed"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
		t.Fatal("closed watcher emitted")
	case <-time.After(150 * time.Millisecond):
	}
}
