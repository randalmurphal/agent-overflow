package assetwatch

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

	"agent-overflow/internal/fswatch"
)

// watcher is the shared core behind every flat client-asset directory
// the app live-reloads: <configDir>/themes, <configDir>/spinners,
// <configDir>/sounds and <configDir>/prompts, and the device-name file's
// directory. One fsnotify watch on one directory, a self-write suppression
// ledger, and a re-arm for the directory itself being removed; the debounce
// and the event loop are fswatch's.
//
// The per-directory part is exactly one predicate — which BASE NAMES in
// that directory are content this app reads — so that is the only thing
// a caller supplies beyond the dir and the loop's debounce and emit.
// Everything else here is subtle enough that a second copy would drift:
// the suppression window's direction of failure, the non-destructive
// suppression consume, and the silent-death-on-directory-removal case
// each have a comment below explaining a tradeoff that was chosen once.
//
// ThemeWatcher, SpinnerWatcher, SoundWatcher, PromptWatcher and
// DeviceNameWatcher wrap this core so their capabilities remain distinct.
type watcher struct {
	// label prefixes every log line and error so a message names the
	// watcher a reader is looking for ("theme watcher: ...").
	label string
	dir   string
	// relevantName answers whether one BASE NAME in dir is content this
	// app reads. The dir check is the core's (see relevant); the
	// predicate never sees a path.
	relevantName func(name string) bool

	watcher *fsnotify.Watcher
	loop    *fswatch.Loop

	// suppressMu guards suppressed, which maps an absolute path to the
	// instant after which events for it are no longer ours.
	suppressMu sync.Mutex
	suppressed map[string]time.Time
	now        func() time.Time
}

// selfWriteWindow is how long after our own atomic write the
// resulting filesystem event is treated as ours and dropped.
//
// A window rather than an exact-event match because the rename lands as
// a CREATE on the destination name with no marker distinguishing it from
// a user's editor doing the same thing, and the event arrives
// asynchronously after os.Rename returns.
//
// One second is the tradeoff, in full: an external edit to a file we just
// wrote, landing inside one second of OUR write, is SWALLOWED — the
// frontend does not refetch, and it keeps showing what it just wrote
// until something else touches the directory. That is the deliberate
// direction to lose in. The competing race is real and constant (the
// fsnotify delivery for our own multi-event atomic write is unbounded in
// principle and routinely tens of milliseconds under load), while an
// external writer racing us inside the same second means the user changed
// the value in the UI and hand-edited the file at the same instant.
// Shrinking the window trades a rare swallowed edit for a routine
// self-echo loop; every self-echo is a full refetch plus a whole reapply.
const selfWriteWindow = time.Second

// rearmAttempts / rearmDelay bound the re-watch after the
// watched directory itself is removed or renamed. Short and few: the
// recreate either works immediately or the path is blocked by something a
// retry loop will not outlast.
const (
	rearmAttempts = 5
	rearmDelay    = 200 * time.Millisecond
)

// newWatcher arms a watch over dir and starts its loop. config carries
// the debounce and emit (and, in tests, a held clock); its label is set
// here. source turns the armed fsnotify watcher into what the loop reads:
// fswatch.FromWatcher in production, a relay in tests.
func newWatcher(label, dir string, relevantName func(name string) bool, config fswatch.Config, source func(*fsnotify.Watcher) fswatch.Source) (*watcher, error) {
	config.Label = label
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("%s: dir is required", label)
	}
	if relevantName == nil {
		return nil, fmt.Errorf("%s: relevance predicate is required", label)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("%s: inspect dir: %w", label, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s: %q is not a directory", label, dir)
	}
	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("%s: create watcher: %w", label, err)
	}
	result := &watcher{
		label: label, dir: filepath.Clean(dir), relevantName: relevantName, watcher: fsWatcher,
		suppressed: make(map[string]time.Time), now: time.Now,
	}
	// One directory, no recursion: these directories are flat by contract,
	// and a user's subdirectory holds nothing this app reads.
	if err := fsWatcher.Add(result.dir); err != nil {
		return nil, errors.Join(fmt.Errorf("%s: watch %s: %w", label, result.dir, err), fsWatcher.Close())
	}
	if result.loop, err = fswatch.Start(config, source(fsWatcher), result.handle); err != nil {
		return nil, err
	}
	return result, nil
}

// suppress marks path as written by this process, so the event it
// produces does not travel back to the frontend as an external change.
func (w *watcher) suppress(path string) {
	if w == nil || strings.TrimSpace(path) == "" {
		return
	}
	clean := filepath.Clean(path)
	w.suppressMu.Lock()
	defer w.suppressMu.Unlock()
	now := w.nowOrDefault()
	w.suppressed[clean] = now.Add(selfWriteWindow)
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
func (w *watcher) isSuppressed(path string) bool {
	clean := filepath.Clean(path)
	w.suppressMu.Lock()
	defer w.suppressMu.Unlock()
	expiry, marked := w.suppressed[clean]
	if !marked {
		return false
	}
	if expiry.Before(w.nowOrDefault()) {
		delete(w.suppressed, clean)
		return false
	}
	return true
}

// nowOrDefault keeps the suppression ledger usable on a bare value. The
// clock is injectable for tests; a watcher built by newWatcher always
// has one.
func (w *watcher) nowOrDefault() time.Time {
	if w.now == nil {
		return time.Now()
	}
	return w.now()
}

// relevant reports whether an event path names content this watcher's
// directory holds. Anything in a SUBDIRECTORY is excluded: the watch is not
// recursive, but a rename can still name a nested path.
func (w *watcher) relevant(path string) bool {
	if w.relevantName == nil {
		return false
	}
	clean := filepath.Clean(path)
	if filepath.Dir(clean) != filepath.Clean(w.dir) {
		return false
	}
	return w.relevantName(filepath.Base(clean))
}

// rearm re-creates the watched directory and re-registers the watch after
// it was removed or renamed out from under us.
//
// The old descriptor is dropped first: a RENAME leaves the watch attached
// to the moved inode (it is the same directory, just elsewhere), so
// without the Remove the re-Add would be a no-op against a path that is
// no longer being watched. A REMOVE has already dropped it, and Remove on
// an unwatched path is a harmless error.
//
// Runs on the loop goroutine, which is why the delay honours stop — the
// directory is gone, so there is nothing else for that goroutine to be
// doing, but a Close during the retry must not be held up by it.
func (w *watcher) rearm(stop <-chan struct{}) {
	var err error
	for attempt := range rearmAttempts {
		if attempt > 0 {
			select {
			case <-stop:
				return
			case <-time.After(rearmDelay):
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
	// indistinguishable from a broken content file.
	log.Printf("%s: %s went away and could not be re-watched, live reload is off until restart: %v", w.label, w.dir, err)
}

func (w *watcher) Close() error { return w.loop.Close() }

// handle reports whether one event changed content this directory holds.
func (w *watcher) handle(event fsnotify.Event, stop <-chan struct{}) bool {
	// The watched directory ITSELF going away kills the watch silently:
	// inotify stops delivering, fsnotify reports no error, and live reload
	// is dead for the rest of the process with nothing in the log.
	// `rm -rf themes/` is a plausible thing for a user or an agent to do
	// while iterating, and EnsureBoot only runs at boot, so nothing else
	// would ever put it back.
	if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 && filepath.Clean(event.Name) == w.dir {
		w.rearm(stop)
		// Whatever was in there is gone, which is a change the frontend
		// has to see even if the re-watch failed.
		return true
	}
	return w.relevant(event.Name) && !w.isSuppressed(event.Name)
}
