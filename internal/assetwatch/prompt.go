package assetwatch

import (
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/fswatch"
)

const promptDebounce = 250 * time.Millisecond

// PromptWatcher watches the flat <configDir>/prompts directory, which holds
// one system prompt override text per file, and emits one debounced
// notification when a prompt file is added, edited or removed. It does not
// suppress the app's own writes: those are an added entry's empty file and
// a removed entry's deletion, and the reload they cause changes nothing.
type PromptWatcher struct {
	core *watcher
}

// NewPromptWatcher arms a production prompts-directory watcher.
func NewPromptWatcher(dir string, emit func()) (*PromptWatcher, error) {
	core, err := newWatcher("prompt watcher", dir, promptFileRelevant, fswatch.Config{Debounce: promptDebounce, Emit: emit})
	if err != nil {
		return nil, err
	}
	return &PromptWatcher{core: core}, nil
}

func (w *PromptWatcher) Close() error {
	if w == nil || w.core == nil {
		return nil
	}
	return w.core.Close()
}

// promptFileRelevant admits prompt texts. A .md file no entry names is
// still relevant here: the settings reload it triggers is cheap, and only
// the settings service knows which ids are live.
func promptFileRelevant(name string) bool {
	return strings.EqualFold(filepath.Ext(name), ".md")
}
