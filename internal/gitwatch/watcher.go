package gitwatch

import (
	"context"
	"log"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/rjeczalik/notify"

	gitops "agent-overflow/internal/git"
)

const (
	// debounceWindow coalesces filesystem events into one git status
	// check. A burst of writes from an agent or build tool collapses to
	// exactly one refresh.
	debounceWindow = 250 * time.Millisecond

	// debounceMaxWait caps how long a continuous stream of fs events
	// can keep deferring a refresh. Without this bound, a long-running
	// process that writes a file every <250ms (e.g. an agent dumping
	// many files, or a dependency install mid-turn) would reset the trailing
	// edge forever and the UI would see stale git status until the
	// activity stopped. Trade-off: a refresh during heavy activity is
	// the price of liveness.
	debounceMaxWait = 1500 * time.Millisecond

	// pollFallbackInterval is the cadence used when a recursive fs
	// watcher cannot be installed (e.g. Linux inotify limit exhausted).
	// Slow enough not to thrash, fast enough that an external commit
	// surfaces in the UI within a few seconds.
	pollFallbackInterval = 3 * time.Second

	// notifyChannelSize bounds the per-watcher event queue. notify
	// drops events when full; the trailing-edge debounce coalesces a
	// burst regardless, so we only need enough headroom that the
	// queue doesn't overflow between drains. Sized for ~250 events
	// per debounce window. Grow this proportionally if debounceWindow
	// grows.
	notifyChannelSize = 64
)

// Subscription is a handle to a per-cwd update stream. Updates() returns
// a channel that closes when Close is called or the Manager shuts down.
// Use Initial() for the value at subscribe time — the channel does not
// echo it.
type Subscription struct {
	cwd     string
	initial gitops.GitStatus
	updates chan gitops.GitStatus
	closer  func()
	once    sync.Once
}

// Cwd returns the canonical workspace path this subscription tracks.
func (s *Subscription) Cwd() string { return s.cwd }

// Initial returns the GitStatus computed when Subscribe was called.
func (s *Subscription) Initial() gitops.GitStatus { return s.initial }

// Updates returns a receive-only channel of subsequent status changes.
// The channel closes after Close (or Manager.Close).
func (s *Subscription) Updates() <-chan gitops.GitStatus { return s.updates }

// Close releases the subscription. Idempotent.
func (s *Subscription) Close() {
	s.once.Do(func() {
		if s.closer != nil {
			s.closer()
		}
	})
}

// workspaceWatcher serializes git status checks and subscriber fan-out
// for one cwd. Run loop is the sole writer to lastStatus and to
// subscribers' Updates() channels.
type workspaceWatcher struct {
	cwd      string
	statusFn StatusFn

	ctx    context.Context
	cancel context.CancelFunc

	eventsCh  chan notify.EventInfo
	refreshCh chan struct{}
	done      chan struct{}

	// fallbackPolling is set in start() before the run goroutine begins
	// and never written after — safe to read from the run goroutine
	// without sync. A rebuild-time install failure switches to polling
	// via run-loop-local state instead (see run).
	fallbackPolling bool

	// rootsFn recomputes the normalized watch roots (nil disables
	// rebuilds — polling fallback and tests that inject static roots).
	// installFn is captured in start so rebuilds reuse the same seam
	// tests inject. stopFn unregisters eventsCh from notify (seamed so
	// tests can pin the teardown ordering); set once at construction,
	// never mutated, so all goroutines read it without sync.
	rootsFn   func() ([]gitops.WatchRoot, error)
	installFn func(roots []gitops.WatchRoot, ch chan<- notify.EventInfo) error
	stopFn    func(ch chan<- notify.EventInfo)

	// needsRebuild and forceReinstall are run-goroutine-local state,
	// set while draining events and consumed at the next refresh edge.
	// needsRebuild means the root set may be stale (ignore-rule or git
	// index/config edit, new directory under an ancestor root) and must
	// be recomputed. forceReinstall additionally bypasses the
	// roots-unchanged short-circuit: it is set when a current root's
	// directory was recreated (its notify watchpoint died with the
	// deletion and is never resurrected) or when the event queue was
	// full (drops may have hidden such an event) — identical roots
	// still need a fresh install to re-arm dead watchpoints.
	needsRebuild   bool
	forceReinstall bool

	mu          sync.Mutex
	subscribers []*Subscription
	lastStatus  gitops.GitStatus
	watchRoots  []gitops.WatchRoot
}

func newWorkspaceWatcher(cwd string, statusFn StatusFn, initial gitops.GitStatus, watchRoots []gitops.WatchRoot, rootsFn func() ([]gitops.WatchRoot, error)) *workspaceWatcher {
	ctx, cancel := context.WithCancel(context.Background())
	return &workspaceWatcher{
		cwd:        cwd,
		statusFn:   statusFn,
		ctx:        ctx,
		cancel:     cancel,
		eventsCh:   make(chan notify.EventInfo, notifyChannelSize),
		refreshCh:  make(chan struct{}, 1),
		done:       make(chan struct{}),
		lastStatus: initial,
		watchRoots: append([]gitops.WatchRoot(nil), watchRoots...),
		rootsFn:    rootsFn,
		stopFn:     func(ch chan<- notify.EventInfo) { notify.Stop(ch) },
		// The initial roots were computed before notify was installed;
		// a directory created inside that window is invisible to both.
		// Starting flagged makes the first refresh edge revalidate —
		// equality-gated, so the common case is one cheap recompute and
		// no reinstall.
		needsRebuild: true,
	}
}

// start installs the fs watcher and spawns the run loop. Falls back
// transparently to polling on installation failure (the most common
// cause is the user's inotify watch limit being exhausted on Linux).
// The fallback uses the same Updates() channel, so callers see a
// uniform interface.
func (w *workspaceWatcher) start(installFn func(roots []gitops.WatchRoot, ch chan<- notify.EventInfo) error) {
	if installFn == nil {
		installFn = installNotifyWatcher
	}
	w.installFn = installFn
	if err := installFn(w.currentWatchRoots(), w.eventsCh); err != nil {
		log.Printf("gitwatch: fs watch unavailable for %s roots=%v (%v); falling back to %s polling",
			w.cwd, w.watchRoots, err, pollFallbackInterval)
		w.fallbackPolling = true
	}
	go w.run()
}

// currentWatchRoots returns the live roots slice. Callers must treat it
// as immutable: setWatchRoots replaces the slice wholesale (and stores
// its own copy), so a returned snapshot is never mutated under a reader.
func (w *workspaceWatcher) currentWatchRoots() []gitops.WatchRoot {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.watchRoots
}

func (w *workspaceWatcher) setWatchRoots(roots []gitops.WatchRoot) {
	roots = slices.Clone(roots)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.watchRoots = roots
}

// installNotifyWatcher attaches watchers rooted at each path. rjeczalik/notify
// uses the path/... glob suffix for recursive watches on Linux/Darwin/Windows.
func installNotifyWatcher(roots []gitops.WatchRoot, ch chan<- notify.EventInfo) error {
	for _, root := range roots {
		path := root.Path
		if root.Recursive {
			path = filepath.Join(path, "...")
		}
		if err := notify.Watch(path, ch, notify.All); err != nil {
			notify.Stop(ch)
			return err
		}
	}
	return nil
}

// stop cancels the run loop and unregisters the fs watcher. Blocks
// until run exits so callers can rely on no further channel sends.
func (w *workspaceWatcher) stop() {
	w.cancel()
	// notify.Stop on an unregistered channel is a no-op, so we can call
	// it unconditionally — including in the polling-fallback path.
	w.stopFn(w.eventsCh)
	<-w.done
}

// run is the watcher's single goroutine: it owns the debounce timer,
// the polling tick (when active), and the subscriber fan-out.
func (w *workspaceWatcher) run() {
	defer close(w.done)
	// Runs before done closes (defers are LIFO): a stop() racing a
	// rebuild can have its notify.Stop land between the rebuild's Stop
	// and reinstall, leaving the reinstalled watches registered forever.
	// stop() blocks on done, so by the time it returns this final Stop
	// has happened after any in-flight reinstall completed.
	defer w.stopFn(w.eventsCh)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("gitwatch: panic in run loop for %s: %v", w.cwd, r)
		}
		w.broadcastClose()
	}()

	debounce := time.NewTimer(debounceWindow)
	if !debounce.Stop() {
		<-debounce.C
	}
	debounceArmed := false
	// firstEventAt anchors the start of the current event burst so the
	// debounce can be force-fired after debounceMaxWait, preventing
	// continuous fs activity from starving the refresh.
	var firstEventAt time.Time

	var pollTicker *time.Ticker
	var pollCh <-chan time.Time
	startPolling := func() {
		if pollTicker != nil {
			return
		}
		pollTicker = time.NewTicker(pollFallbackInterval)
		pollCh = pollTicker.C
	}
	stopPolling := func() {
		if pollTicker == nil {
			return
		}
		pollTicker.Stop()
		pollTicker = nil
		pollCh = nil
	}
	defer stopPolling()
	if w.fallbackPolling {
		startPolling()
	}

	// refreshEdge is every path that runs statusFn off fs events: apply
	// a pending watch-root rebuild first so the refresh observes the
	// world the new roots describe. A rebuild whose reinstall fails
	// leaves the watcher blind, so it escalates to polling; a later
	// reinstall that succeeds proves the watches are live again, so the
	// now-redundant ticker stops.
	refreshEdge := func() {
		switch w.maybeRebuildWatches() {
		case rebuildLostWatches:
			startPolling()
		case rebuildReinstalled:
			stopPolling()
		}
		w.refresh()
	}

	for {
		select {
		case <-w.ctx.Done():
			return
		case ev := <-w.eventsCh:
			roots := w.currentWatchRoots()
			w.inspectEvent(ev, roots)
			// Drain any other events queued in this tick to avoid
			// repeatedly resetting the timer when a burst arrives.
			// drainNotify also watches for a (near-)full queue — the
			// dropped-events pessimism — sampling every dequeue so a
			// producer outpacing the drain itself can't slip past a
			// single top-of-loop check.
			w.drainNotify(roots)
			now := time.Now()
			if !debounceArmed {
				firstEventAt = now
			}
			// Honor the max-wait ceiling: if the burst has already run
			// past debounceMaxWait, refresh now and rearm a fresh
			// debounce window for whatever comes next.
			if debounceArmed && now.Sub(firstEventAt) >= debounceMaxWait {
				if !debounce.Stop() {
					select {
					case <-debounce.C:
					default:
					}
				}
				debounceArmed = false
				refreshEdge()
				continue
			}
			if debounceArmed {
				if !debounce.Stop() {
					select {
					case <-debounce.C:
					default:
					}
				}
			}
			debounce.Reset(debounceWindow)
			debounceArmed = true
		case <-debounce.C:
			debounceArmed = false
			refreshEdge()
		case <-pollCh:
			w.refresh()
		case <-w.refreshCh:
			w.refresh()
		}
	}
}

// requestRefresh asks the run loop to do a full statusFn refresh on
// its next iteration. Non-blocking; coalesces with any pending request.
// Used by the Manager to trigger a follow-up refresh after the initial
// fast subscribe returns without PR info.
func (w *workspaceWatcher) requestRefresh() {
	select {
	case w.refreshCh <- struct{}{}:
	default:
	}
}

// refresh re-reads git status, dedups against lastStatus, and broadcasts
// to subscribers. Status fetch errors are logged and skipped; lastStatus
// stays as-is so we don't mistake a transient error for a real change.
//
// The broadcast runs UNDER w.mu so a concurrent removeSubscriber can't
// close(s.updates) on a sub we're about to send to — sending on a
// closed channel panics even from a non-blocking select. The sends are
// themselves non-blocking (buffer size 1, supersede-on-overflow), so
// the lock is held for microseconds bounded by len(subscribers).
func (w *workspaceWatcher) refresh() {
	status, err := w.statusFn(w.cwd)
	if err != nil {
		log.Printf("gitwatch: status fetch for %s: %v", w.cwd, err)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if status.Equal(w.lastStatus) {
		return
	}
	w.lastStatus = status
	for _, s := range w.subscribers {
		// Subscription channels are sized 1 with supersede-on-overflow:
		// the latest status is always more useful than a queued older
		// one, so we drain any pending value before sending. Both selects
		// are non-blocking, so the run loop never stalls.
		select {
		case <-s.updates:
		default:
		}
		select {
		case s.updates <- status:
		default:
		}
	}
}

func (w *workspaceWatcher) addSubscriber(initial gitops.GitStatus) *Subscription {
	sub := &Subscription{
		cwd:     w.cwd,
		initial: initial,
		updates: make(chan gitops.GitStatus, 1),
	}
	w.mu.Lock()
	w.subscribers = append(w.subscribers, sub)
	w.mu.Unlock()
	return sub
}

// addSubscriberFromSnapshot seeds a subscriber with the watcher's latest
// status while holding w.mu, so a refresh cannot broadcast a newer status
// between the snapshot and registration.
func (w *workspaceWatcher) addSubscriberFromSnapshot() (*Subscription, gitops.GitStatus) {
	w.mu.Lock()
	defer w.mu.Unlock()
	initial := w.lastStatus
	sub := &Subscription{
		cwd:     w.cwd,
		initial: initial,
		updates: make(chan gitops.GitStatus, 1),
	}
	w.subscribers = append(w.subscribers, sub)
	return sub, initial
}

func (w *workspaceWatcher) removeSubscriber(target *Subscription) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, s := range w.subscribers {
		if s == target {
			w.subscribers = append(w.subscribers[:i], w.subscribers[i+1:]...)
			close(s.updates)
			break
		}
	}
	return len(w.subscribers)
}

func (w *workspaceWatcher) broadcastClose() {
	w.mu.Lock()
	subs := w.subscribers
	w.subscribers = nil
	w.mu.Unlock()
	for _, s := range subs {
		close(s.updates)
	}
}
