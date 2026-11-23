package assetwatch

import (
	"path/filepath"
	"time"

	"agent-overflow/internal/fswatch"
)

// DeviceNameWatcher observes atomic replacement by either desktop process.
// It shares the directory watcher/re-arm lifecycle with appearance assets.
type DeviceNameWatcher struct{ core *watcher }

const deviceNameDebounce = 100 * time.Millisecond

// deviceNameRelevant admits the device-name file itself, which an atomic
// write replaces by renaming its temp file onto it.
func deviceNameRelevant(path string) func(name string) bool {
	base := filepath.Base(path)
	return func(name string) bool { return name == base }
}

func NewDeviceNameWatcher(path string, emit func()) (*DeviceNameWatcher, error) {
	core, err := newWatcher("device name watcher", filepath.Dir(path), deviceNameRelevant(path), fswatch.Config{Debounce: deviceNameDebounce, Emit: emit})
	if err != nil {
		return nil, err
	}
	return &DeviceNameWatcher{core: core}, nil
}
func (w *DeviceNameWatcher) Close() error {
	if w == nil || w.core == nil {
		return nil
	}
	return w.core.Close()
}
