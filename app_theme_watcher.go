package main

import (
	"log"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/theme"
)

// themeWatchDebounce matches the workflow-definitions watcher: an agent
// rewriting a theme file, or an editor doing its save dance, produces a
// burst of events that must collapse into one refetch.
const themeWatchDebounce = 250 * time.Millisecond

// themeSelfWriteWindow is the shared self-write suppression window, named
// here because this is the one directory the app writes into itself
// (appearance.json). The tradeoff it encodes is documented at
// dirWatchSelfWriteWindow.
const themeSelfWriteWindow = dirWatchSelfWriteWindow

// themeWatcher watches <configDir>/themes for theme-file changes and
// calls emit (debounced) so the frontend refetches. The agent-edit loop
// in docs/architecture/theme-system.md §7 is this watcher.
//
// It is a defined type over dirWatcher, not a wrapper: the fields, the
// event loop, the debounce and the suppression ledger are that struct's
// (see the comment there), and what belongs to THEMES is the relevance
// rule below.
type themeWatcher dirWatcher

func newThemeWatcher(dir string, debounce time.Duration, emit func()) (*themeWatcher, error) {
	watcher, err := newDirWatcher("theme watcher", dir, debounce, themeFileRelevant, emit)
	if err != nil {
		return nil, err
	}
	return (*themeWatcher)(watcher), nil
}

// core returns the shared watcher this one is.
func (w *themeWatcher) core() *dirWatcher { return (*dirWatcher)(w) }

// suppress marks path as written by this process, so the event it
// produces does not travel back to the frontend as an external change.
func (w *themeWatcher) suppress(path string) { w.core().suppress(path) }

// isSuppressed reports whether path is still inside its self-write window.
func (w *themeWatcher) isSuppressed(path string) bool { return w.core().isSuppressed(path) }

func (w *themeWatcher) Close() error { return w.core().Close() }

// relevant reports whether an event path is a theme-directory JSON file
// this app reads.
func (w *themeWatcher) relevant(path string) bool {
	return relevantInDir(w.dir, path, themeFileRelevant)
}

// themeFileRelevant is the themes-directory rule, over a base name. It
// excludes:
//   - the atomic-write temp files (`<base>.tmp-NNNNN`), whose extension is
//     not `.json` at all, so they fall out here by construction;
//   - the generated reference artifacts, which the frontend never reads
//     and the backend rewrites at boot.
func themeFileRelevant(name string) bool {
	if !strings.EqualFold(filepath.Ext(name), ".json") {
		return false
	}
	return !strings.EqualFold(name, theme.SchemaFileName)
}

// startThemeWatcher arms the themes-directory watcher. A watcher that
// cannot start is logged and skipped rather than failing boot: live
// reload is a convenience on top of the RPC, and the app is fully usable
// without it (the frontend still reads themes on demand).
func (a *App) startThemeWatcher(dir string) {
	watcher, err := newThemeWatcher(dir, themeWatchDebounce, func() {
		a.emit(eventchan.ThemeChanged, nil)
	})
	if err != nil {
		log.Printf("theme watcher unavailable: %v", err)
		return
	}
	a.themeWatcher = watcher
}

// suppressThemeWatch marks a path as written by this process. Nil-safe
// so the App's write path does not care whether the watcher started.
func (a *App) suppressThemeWatch(path string) {
	if a.themeWatcher == nil {
		return
	}
	a.themeWatcher.suppress(path)
}
