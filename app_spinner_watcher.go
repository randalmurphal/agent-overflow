package main

import (
	"log"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/spinner"
)

// spinnerWatchDebounce matches the themes watcher. A sprite lands as TWO
// files (the strip and its manifest), so the burst this collapses is not
// just an editor's save dance — it is the user dropping a pair in, where
// a refetch between the two halves would report a half-written sprite the
// user is in the middle of completing.
const spinnerWatchDebounce = 250 * time.Millisecond

// spinnerWatcher watches <configDir>/spinners so a sprite dropped into
// the directory reaches the composer without a restart. Same defined-type
// arrangement over dirWatcher as themeWatcher — the relevance rule below
// is the whole difference.
//
// Nothing in this process writes into the spinners directory except the
// boot-time SPINNERS.md refresh, which the relevance rule excludes by
// name, so no suppression call exists here. The capability still lives in
// the shared core because themes needs it.
type spinnerWatcher dirWatcher

func newSpinnerWatcher(dir string, debounce time.Duration, emit func()) (*spinnerWatcher, error) {
	watcher, err := newDirWatcher("spinner watcher", dir, debounce, spinnerFileRelevant, emit)
	if err != nil {
		return nil, err
	}
	return (*spinnerWatcher)(watcher), nil
}

func (w *spinnerWatcher) Close() error { return (*dirWatcher)(w).Close() }

// relevant reports whether an event path is a spinner-directory file this
// app reads.
func (w *spinnerWatcher) relevant(path string) bool {
	return relevantInDir(w.dir, path, spinnerFileRelevant)
}

// spinnerFileRelevant is the spinners-directory rule, over a base name.
// Both halves of a sprite pair count — a manifest edited on its own
// changes the animation just as much as a new strip does — and the
// generated authoring reference does not, since the frontend never reads
// it and the backend rewrites it at boot.
//
// The atomic-write temp names that reference refresh produces
// (`SPINNERS.md.tmp-NNNNN`) fall out here by construction: their
// extension is `.tmp-NNNNN`, neither `.png` nor `.json`.
func spinnerFileRelevant(name string) bool {
	extension := strings.ToLower(filepath.Ext(name))
	if extension != ".png" && extension != ".json" {
		return false
	}
	return !strings.EqualFold(name, spinner.ReferenceFileName)
}

// startSpinnerWatcher arms the spinners-directory watcher. A watcher that
// cannot start is logged and skipped rather than failing boot: live
// reload is a convenience on top of GetSpinnerFiles, and the app is fully
// usable without it (built-in sprites ship with the frontend).
func (a *App) startSpinnerWatcher(dir string) {
	watcher, err := newSpinnerWatcher(dir, spinnerWatchDebounce, func() {
		a.emit("spinner:changed", nil)
	})
	if err != nil {
		log.Printf("spinner watcher unavailable: %v", err)
		return
	}
	a.spinnerWatcher = watcher
}
