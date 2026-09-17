package assetwatch

import (
	"path/filepath"
	"strings"
	"time"
)

const soundDebounce = 250 * time.Millisecond

// SoundWatcher watches the flat sounds directory and emits one debounced
// notification when a custom notification cue is added, replaced or removed.
type SoundWatcher struct {
	core *watcher
}

// NewSoundWatcher arms a production sounds-directory watcher.
func NewSoundWatcher(dir string, emit func()) (*SoundWatcher, error) {
	return newSoundWatcher(dir, soundDebounce, emit)
}

func newSoundWatcher(dir string, debounce time.Duration, emit func()) (*SoundWatcher, error) {
	core, err := newWatcher("sound watcher", dir, debounce, soundFileRelevant, emit)
	if err != nil {
		return nil, err
	}
	return &SoundWatcher{core: core}, nil
}

// Suppress marks path as written by this process so its asynchronous
// filesystem events do not echo back to the frontend.
//
// SpinnerWatcher has no equivalent because nothing in the app writes a
// sprite; this directory has PutSoundFile and DeleteSoundFile, whose caller
// already knows what it just did and would otherwise pay a full listing
// refetch to be told again.
func (w *SoundWatcher) Suppress(path string) {
	if w == nil || w.core == nil {
		return
	}
	w.core.suppress(path)
}

func (w *SoundWatcher) Close() error {
	if w == nil || w.core == nil {
		return nil
	}
	return w.core.Close()
}

func (w *SoundWatcher) relevant(path string) bool {
	return w != nil && w.core != nil && relevantInDir(w.core.dir, path, soundFileRelevant)
}

// soundFileRelevant admits cue files only. The seeded reference page is not
// one, so it needs no exclusion by name the way themes' schema and spinners'
// SPINNERS.md do.
func soundFileRelevant(name string) bool {
	return strings.EqualFold(filepath.Ext(name), ".wav")
}
