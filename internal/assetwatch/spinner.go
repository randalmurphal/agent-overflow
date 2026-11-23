package assetwatch

import (
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/fswatch"
)

const spinnerDebounce = 250 * time.Millisecond

// SpinnerWatcher watches the flat spinners directory and emits one debounced
// notification when either half of a sprite changes.
type SpinnerWatcher struct {
	core *watcher
}

// NewSpinnerWatcher arms a production spinners-directory watcher.
func NewSpinnerWatcher(dir string, emit func()) (*SpinnerWatcher, error) {
	core, err := newWatcher("spinner watcher", dir, spinnerFileRelevant, fswatch.Config{Debounce: spinnerDebounce, Emit: emit}, fswatch.FromWatcher)
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

// spinnerFileRelevant admits both halves of a sprite. The generated
// reference page is a .md file, so the extension keeps it out.
func spinnerFileRelevant(name string) bool {
	extension := strings.ToLower(filepath.Ext(name))
	return extension == ".png" || extension == ".json"
}
