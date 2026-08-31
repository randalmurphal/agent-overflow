package assetwatch

import (
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/theme"
)

const themeDebounce = 250 * time.Millisecond

// themeSelfWriteWindow names the shared suppression window for the tests that
// pin the themes directory's self-write behavior.
const themeSelfWriteWindow = selfWriteWindow

// ThemeWatcher watches the flat themes directory and emits one debounced
// notification when content the frontend reads changes.
type ThemeWatcher struct {
	core *watcher
}

// NewThemeWatcher arms a production themes-directory watcher.
func NewThemeWatcher(dir string, emit func()) (*ThemeWatcher, error) {
	return newThemeWatcher(dir, themeDebounce, emit)
}

func newThemeWatcher(dir string, debounce time.Duration, emit func()) (*ThemeWatcher, error) {
	core, err := newWatcher("theme watcher", dir, debounce, themeFileRelevant, emit)
	if err != nil {
		return nil, err
	}
	return &ThemeWatcher{core: core}, nil
}

// Suppress marks path as written by this process so its asynchronous
// filesystem events do not echo back to the frontend.
func (w *ThemeWatcher) Suppress(path string) {
	if w == nil || w.core == nil {
		return
	}
	w.core.suppress(path)
}

func (w *ThemeWatcher) Close() error {
	if w == nil || w.core == nil {
		return nil
	}
	return w.core.Close()
}

func (w *ThemeWatcher) relevant(path string) bool {
	return w != nil && w.core != nil && relevantInDir(w.core.dir, path, themeFileRelevant)
}

func themeFileRelevant(name string) bool {
	if !strings.EqualFold(filepath.Ext(name), ".json") {
		return false
	}
	return !strings.EqualFold(name, theme.SchemaFileName)
}
