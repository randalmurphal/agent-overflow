package assetwatch

import (
	"path/filepath"
	"time"
)

// DeviceNameWatcher observes atomic replacement by either desktop process.
// It shares the directory watcher/re-arm lifecycle with appearance assets.
type DeviceNameWatcher struct{ core *watcher }

func NewDeviceNameWatcher(path string, emit func()) (*DeviceNameWatcher, error) {
	core, err := newWatcher("device name watcher", filepath.Dir(path), 100*time.Millisecond, func(name string) bool { return name == filepath.Base(path) }, emit)
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
