package assetwatch

import (
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/spinner"
)

const spinnerDebounce = 250 * time.Millisecond

// SpinnerWatcher watches the flat spinners directory and emits one debounced
// notification when either half of a sprite changes.
type SpinnerWatcher struct {
	core *watcher
}

// NewSpinnerWatcher arms a production spinners-directory watcher.
func NewSpinnerWatcher(dir string, emit func()) (*SpinnerWatcher, error) {
	return newSpinnerWatcher(dir, spinnerDebounce, emit)
}

func newSpinnerWatcher(dir string, debounce time.Duration, emit func()) (*SpinnerWatcher, error) {
	core, err := newWatcher("spinner watcher", dir, debounce, spinnerFileRelevant, emit)
	if err != nil {
		return nil, err
	}
	return &SpinnerWatcher{core: core}, nil
}

func (w *SpinnerWatcher) Close() error {
	if w == nil || w.core == nil {
		return nil
	}
	return w.core.Close()
}

func (w *SpinnerWatcher) relevant(path string) bool {
	return w != nil && w.core != nil && relevantInDir(w.core.dir, path, spinnerFileRelevant)
}

func spinnerFileRelevant(name string) bool {
	extension := strings.ToLower(filepath.Ext(name))
	if extension != ".png" && extension != ".json" {
		return false
	}
	return !strings.EqualFold(name, spinner.ReferenceFileName)
}
