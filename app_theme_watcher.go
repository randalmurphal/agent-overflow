package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"agent-overflow/internal/theme"
)

// themeWatchDebounce matches the workflow-definitions watcher: an agent
// rewriting a theme file, or an editor doing its save dance, produces a
// burst of events that must collapse into one refetch.
const themeWatchDebounce = 250 * time.Millisecond

// themeSelfWriteWindow is how long after our own atomic write the
// resulting filesystem event is treated as ours and dropped.
//
// A window rather than an exact-event match because the rename lands as
// a CREATE on the destination name with no marker distinguishing it from
// a user's editor doing the same thing, and the event arrives
// asynchronously after os.Rename returns.
//
// One second is the tradeoff, in full: an external edit to appearance.json
// landing inside one second of OUR write is SWALLOWED — the frontend does
// not refetch, and it keeps showing the selection it just wrote until
// something else touches the directory. That is the deliberate direction
// to lose in. The competing race is real and constant (the fsnotify
// delivery for our own multi-event atomic write is unbounded in principle
// and routinely tens of milliseconds under load), while an external writer
// racing us inside the same second means the user changed the appearance
// in the UI and hand-edited the file at the same instant. Shrinking the
// window trades a rare swallowed edit for a routine self-echo loop; every
// self-echo is a full GetThemeFiles plus a whole-theme reapply.
const themeSelfWriteWindow = time.Second

// themeRearmAttempts / themeRearmDelay bound the re-watch after the themes
// directory itself is removed or renamed. Short and few: the recreate
// either works immediately or the path is blocked by something a retry
// loop will not outlast.
const (
	themeRearmAttempts = 5
	themeRearmDelay    = 200 * time.Millisecond
)

// themeWatcher watches <configDir>/themes for theme-file changes and
// calls emit (debounced) so the frontend refetches. The agent-edit loop
// in docs/specs/theme-system.md §7 is this watcher.
type themeWatcher struct {
	dir       string
	debounce  time.Duration
	emit      func()
	watcher   *fsnotify.Watcher
	done      chan struct{}
	closeOnce sync.Once
	waitGroup sync.WaitGroup

	// suppressMu guards suppressed, which maps an absolute path to the
	// instant after which events for it are no longer ours.
	suppressMu sync.Mutex
	suppressed map[string]time.Time
	now        func() time.Time
}

func newThemeWatcher(dir string, debounce time.Duration, emit func()) (*themeWatcher, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("theme watcher: dir is required")
	}
	if debounce <= 0 {
		return nil, errors.New("theme watcher: debounce must be positive")
	}
	if emit == nil {
		return nil, errors.New("theme watcher: emit callback is required")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("theme watcher: inspect dir: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("theme watcher: %q is not a directory", dir)
	}
	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("theme watcher: create watcher: %w", err)
	}
	watcher := &themeWatcher{
		dir: filepath.Clean(dir), debounce: debounce, emit: emit, watcher: fsWatcher,
		done: make(chan struct{}), suppressed: make(map[string]time.Time), now: time.Now,
	}
	// One directory, no recursion: themes/ is flat by contract, and a
	// user's subdirectory holds nothing this app reads.
	if err := fsWatcher.Add(watcher.dir); err != nil {
		return nil, errors.Join(fmt.Errorf("theme watcher: watch %s: %w", watcher.dir, err), fsWatcher.Close())
	}
	watcher.waitGroup.Add(1)
	go watcher.loop()
	return watcher, nil
}

// suppress marks path as written by this process, so the event it
// produces does not travel back to the frontend as an external change.
func (w *themeWatcher) suppress(path string) {
	if w == nil || strings.TrimSpace(path) == "" {
		return
	}
	clean := filepath.Clean(path)
	w.suppressMu.Lock()
	defer w.suppressMu.Unlock()
	now := w.now()
	w.suppressed[clean] = now.Add(themeSelfWriteWindow)
	for candidate, expiry := range w.suppressed {
		if candidate != clean && expiry.Before(now) {
			delete(w.suppressed, candidate)
		}
	}
}

// isSuppressed reports whether path is still inside its self-write
// window. Consuming is deliberately non-destructive: one atomic write
// produces several events (create, write, chmod, rename), and dropping
// the mark on the first would let the rest through.
func (w *themeWatcher) isSuppressed(path string) bool {
	clean := filepath.Clean(path)
	w.suppressMu.Lock()
	defer w.suppressMu.Unlock()
	expiry, marked := w.suppressed[clean]
	if !marked {
		return false
	}
	if expiry.Before(w.now()) {
		delete(w.suppressed, clean)
		return false
	}
	return true
}

// relevant reports whether an event path is a theme-directory JSON file
// this app reads. It excludes:
//   - the atomic-write temp files (`.theme-*.tmp`), which are ours by
//     construction and never a theme;
//   - the generated reference artifacts, which the frontend never reads
//     and the backend rewrites at boot;
//   - anything in a subdirectory, since the watch is not recursive but
//     a rename can still name a nested path.
func (w *themeWatcher) relevant(path string) bool {
	clean := filepath.Clean(path)
	if filepath.Dir(clean) != w.dir {
		return false
	}
	name := filepath.Base(clean)
	if !strings.EqualFold(filepath.Ext(name), ".json") {
		return false
	}
	return !strings.EqualFold(name, theme.SchemaFileName)
}

// rearm re-creates the themes directory and re-registers the watch after
// the directory was removed or renamed out from under it.
//
// The old descriptor is dropped first: a RENAME leaves the watch attached
// to the moved inode (it is the same directory, just elsewhere), so
// without the Remove the re-Add would be a no-op against a path that is
// no longer being watched. A REMOVE has already dropped it, and Remove on
// an unwatched path is a harmless error.
//
// Runs on the watch goroutine, which is why the delay honours w.done —
// the directory is gone, so there is nothing else for that goroutine to
// be doing, but a Close during the retry must not be held up by it.
func (w *themeWatcher) rearm() {
	var err error
	for attempt := range themeRearmAttempts {
		if attempt > 0 {
			select {
			case <-w.done:
				return
			case <-time.After(themeRearmDelay):
			}
		}
		if err = os.MkdirAll(w.dir, 0o700); err != nil {
			continue
		}
		_ = w.watcher.Remove(w.dir)
		if err = w.watcher.Add(w.dir); err == nil {
			return
		}
	}
	// Loud on the final failure only: live reload is dead for this
	// process and the symptom (edits do nothing) is otherwise
	// indistinguishable from a broken theme file.
	log.Printf("theme watcher: %s went away and could not be re-watched, live reload is off until restart: %v", w.dir, err)
}

func (w *themeWatcher) Close() error {
	var closeErr error
	w.closeOnce.Do(func() {
		close(w.done)
		closeErr = w.watcher.Close()
		w.waitGroup.Wait()
	})
	return closeErr
}

func (w *themeWatcher) loop() {
	defer w.waitGroup.Done()
	var timer *time.Timer
	var timerChannel <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	queueEmit := func() {
		if timer == nil {
			timer = time.NewTimer(w.debounce)
			timerChannel = timer.C
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(w.debounce)
		timerChannel = timer.C
	}

	for {
		select {
		case <-w.done:
			return
		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			// The watched directory ITSELF going away kills the watch
			// silently: inotify stops delivering, fsnotify reports no
			// error, and live reload is dead for the rest of the process
			// with nothing in the log. `rm -rf themes/` is a plausible
			// thing for a user or an agent to do while iterating, and
			// EnsureBoot only runs at boot, so nothing else would ever
			// put it back.
			if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 && filepath.Clean(event.Name) == w.dir {
				w.rearm()
				// Whatever was in there is gone, which is a change the
				// frontend has to see even if the re-watch failed.
				queueEmit()
				continue
			}
			if !w.relevant(event.Name) || w.isSuppressed(event.Name) {
				continue
			}
			queueEmit()
		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("theme watcher: %v", err)
		case <-timerChannel:
			timerChannel = nil
			w.emit()
		}
	}
}

// startThemeWatcher arms the themes-directory watcher. A watcher that
// cannot start is logged and skipped rather than failing boot: live
// reload is a convenience on top of the RPC, and the app is fully usable
// without it (the frontend still reads themes on demand).
func (a *App) startThemeWatcher(dir string) {
	watcher, err := newThemeWatcher(dir, themeWatchDebounce, func() {
		a.emit("theme:changed", nil)
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
