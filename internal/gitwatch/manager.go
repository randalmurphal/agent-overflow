package gitwatch

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rjeczalik/notify"

	gitops "agent-overflow/internal/git"
)

// StatusFn returns the GitStatus for cwd under ctx: the watcher's
// lifetime, or the Subscribe caller's for the initial read. The Manager
// calls this on every debounce trailing edge and on each polling-fallback
// tick. Production wires gitops.Core.Status; tests can stub.
type StatusFn func(ctx context.Context, cwd string) (gitops.GitStatus, error)

// WatchRootsFn returns the filesystem roots that should trigger status
// refreshes for cwd: the pruned workspace subtrees and their ancestors,
// plus git metadata and global-ignore directories that can live outside
// cwd (linked worktrees keep commit/index/ref state under the main
// repo's .git). Normalization always adds cwd itself as a backstop.
type WatchRootsFn func(cwd string) ([]gitops.WatchRoot, error)

// ManagerConfig names the optional status/watch hooks so tests and app startup
// do not rely on positional nil function arguments.
type ManagerConfig struct {
	StatusFn     StatusFn
	FastStatusFn StatusFn
	WatchRootsFn WatchRootsFn
}

// Manager owns the per-cwd watchers and subscriber fan-out. One Manager
// per *App is the expected wiring. Safe for concurrent use.
type Manager struct {
	// statusFn is the full status function including network calls (e.g.
	// gh/glab PR lookup). Used by the watcher's debounce refresh where
	// the PR cache is warm and the call isn't on the RPC critical path.
	statusFn StatusFn

	// fastStatusFn is the fast variant that skips network calls (uses
	// cache-only for PR info). Used for the initial Subscribe fetch so
	// the RPC returns immediately. When nil, statusFn is used instead.
	// Must be set before the first Subscribe call.
	fastStatusFn StatusFn

	// seq orders every status this manager hands out (GitStatus.Seq).
	seq sequence

	mu       sync.Mutex
	watchers map[string]*workspaceWatcher // canonical cwd → watcher
	// creating holds, per canonical cwd, the Subscribes building a watcher
	// that is not registered yet, and the newest status Publish offered
	// meanwhile, which the registered watcher starts from.
	creating map[string]*pendingWatcher
	closed   bool

	// installFn lets tests force the polling-fallback path without
	// actually exhausting inotify limits. Production leaves it nil and
	// the watcher uses installNotifyWatcher.
	installFn func(roots []gitops.WatchRoot, ch chan<- notify.EventInfo) error
	// pollInterval lets tests observe the polling fallback without
	// waiting out pollFallbackInterval. Production leaves it zero.
	pollInterval time.Duration

	watchRootsFn WatchRootsFn
}

// NewManager constructs a Manager. StatusFn is required; FastStatusFn
// is optional (when nil, StatusFn is used for the initial subscribe
// fetch too — legacy behavior).
func NewManager(config ManagerConfig) *Manager {
	statusFn := config.StatusFn
	if statusFn == nil {
		panic("gitwatch: NewManager requires a non-nil StatusFn")
	}
	watchRootsFn := config.WatchRootsFn
	if watchRootsFn == nil {
		watchRootsFn = func(cwd string) ([]gitops.WatchRoot, error) {
			return []gitops.WatchRoot{{Path: cwd, Recursive: true}}, nil
		}
	}
	return &Manager{
		statusFn:     statusFn,
		fastStatusFn: config.FastStatusFn,
		watchers:     make(map[string]*workspaceWatcher),
		creating:     make(map[string]*pendingWatcher),
		watchRootsFn: watchRootsFn,
	}
}

// sequence hands out GitStatus.Seq positions. A position is taken before
// the status's reads begin, so a higher one never comes from a read that
// started earlier, whichever finishes first.
type sequence struct{ n atomic.Uint64 }

func (s *sequence) next() uint64 { return s.n.Add(1) }

// pendingWatcher is one cwd's entry in Manager.creating.
type pendingWatcher struct {
	refs    int
	offered gitops.GitStatus
}

// Subscribe begins delivering status updates for cwd. Multiple
// subscriptions to the same cwd share one underlying watcher via
// refcount. The returned Subscription's Initial() carries the status at
// subscribe time so callers can render synchronously without waiting
// for the first fs event; Updates() yields subsequent changes.
//
// Fast path: if a watcher already exists for cwd, the new subscriber
// uses that watcher's cached lastStatus as Initial() — no synchronous
// statusFn call. If that cached status has branch+forge but no PR URL,
// Subscribe still asks the watcher for an async full refresh so remote
// PR/MR creation can become visible without a local filesystem event.
//
// Slow path: when there's no existing watcher, the watches are installed
// before the initial fetch runs, so no change after the fetch goes
// unreported; a failed fetch removes them again. ctx bounds that initial
// fetch only; the watcher lives until its last subscriber leaves.
func (m *Manager) Subscribe(ctx context.Context, cwd string) (*Subscription, error) {
	if cwd == "" {
		return nil, errors.New("gitwatch: empty cwd")
	}
	abs, canon, err := canonicalize(cwd)
	if err != nil {
		return nil, err
	}
	if err := rejectSystemPath(abs, canon); err != nil {
		return nil, err
	}

	// Fast path: piggy-back on an existing watcher's cached status.
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("gitwatch: manager closed")
	}
	if w, ok := m.watchers[canon]; ok {
		sub, initial := w.addSubscriberFromSnapshot()
		m.mu.Unlock()
		sub.closer = func() { m.releaseSubscriber(canon, sub) }
		if m.shouldRefreshMissingPR(initial) {
			w.requestRefresh()
		}
		return sub, nil
	}
	pending := m.creating[canon]
	if pending == nil {
		pending = &pendingWatcher{}
		m.creating[canon] = pending
	}
	pending.refs++
	m.mu.Unlock()
	// Registration releases pending in the critical section that registers
	// the watcher, so a Publish lands either in pending.offered or on the
	// registered watcher; every earlier exit releases it here.
	defer func() {
		if pending != nil {
			m.mu.Lock()
			m.endCreatingLocked(canon, pending)
			m.mu.Unlock()
		}
	}()

	// Slow path, outside the lock: installing watches and statusFn
	// (which shells out to git) can take tens of milliseconds, and
	// holding m.mu here would serialise unrelated Subscribe / Close
	// calls behind them.
	watchRoots, err := m.watchRoots(canon)
	if err != nil {
		return nil, err
	}
	// The rebuild closure re-runs the full compute+normalize pipeline so
	// a boundary change (.gitignore edit, new directory beside a pruned
	// subtree) yields roots in exactly the shape the initial install used.
	rootsFn := func() ([]gitops.WatchRoot, error) { return m.watchRoots(canon) }
	w := newWorkspaceWatcher(canon, m.statusFn, m.fastStatusFn, watchRoots, rootsFn, &m.seq)
	if m.pollInterval > 0 {
		w.pollInterval = m.pollInterval
	}
	// The watches go in before the initial status is read
	// (workspaceWatcher.install).
	w.install(m.installFn)
	// Use fastStatusFn when available — it skips the network PR lookup
	// so the RPC returns immediately on cold cache.
	initialFn := m.fastStatusFn
	if initialFn == nil {
		initialFn = m.statusFn
	}
	seq := m.seq.next()
	initial, err := initialFn(ctx, canon)
	if err != nil {
		w.discard()
		return nil, err
	}
	initial.Seq = seq

	m.mu.Lock()
	offered := m.endCreatingLocked(canon, pending)
	pending = nil
	if m.closed {
		m.mu.Unlock()
		w.discard()
		return nil, errors.New("gitwatch: manager closed")
	}
	// Race recheck: a concurrent Subscribe may have created the
	// watcher while we were fetching. If so, prefer its lastStatus
	// (the freshest broadcast) over our own fetch.
	if existing, ok := m.watchers[canon]; ok {
		sub, initial := existing.addSubscriberFromSnapshot()
		m.mu.Unlock()
		w.discard()
		sub.closer = func() { m.releaseSubscriber(canon, sub) }
		if m.shouldRefreshMissingPR(initial) {
			existing.requestRefresh()
		}
		return sub, nil
	}
	// A status Publish offered while no watcher was registered began after
	// this read did, so it is the one to publish.
	if offered.Seq > initial.Seq {
		initial = offered
	}
	// The subscriber is registered before the run loop starts, so the
	// loop's first broadcast reaches it.
	sub := w.addSubscriber(initial)
	w.start(initial)
	m.watchers[canon] = w
	m.mu.Unlock()
	sub.closer = func() { m.releaseSubscriber(canon, sub) }

	// When the fast path returned without PR info and the branch has a
	// recognised forge, request a full refresh from the watcher. The
	// watcher's statusFn (the full variant) will do the network PR
	// lookup, populate the cache, and broadcast the updated status to
	// all subscribers. This keeps the network call off the Subscribe
	// RPC critical path.
	if m.shouldRefreshMissingPR(initial) {
		w.requestRefresh()
	}

	return sub, nil
}

// endCreatingLocked releases one Subscribe's hold on creating[canon] and
// returns the status Publish offered meanwhile (zero Seq when none).
// m.mu must be held.
func (m *Manager) endCreatingLocked(canon string, pending *pendingWatcher) gitops.GitStatus {
	pending.refs--
	if pending.refs == 0 && m.creating[canon] == pending {
		delete(m.creating, canon)
	}
	return pending.offered
}

// Publish hands a client a status of cwd read outside its watcher. It
// takes a Seq, then runs read, then offers the result to the watcher,
// which publishes it unless a status whose reads began later is already
// published, and asks the watcher to refresh. It returns the workspace's
// published status after the offer, or read's status stamped with its
// Seq when nothing could publish it: nobody watches cwd, or a Suppress
// hold is taken. A held offer publishes nothing; the watcher broadcasts
// its next published status even when equal in content to the last (see
// publishLocked). A change the offer published counts as observed by that
// refresh for missed-event detection (see run).
func (m *Manager) Publish(cwd string, read func() (gitops.GitStatus, error)) (gitops.GitStatus, error) {
	if cwd == "" {
		return gitops.GitStatus{}, errors.New("gitwatch: empty cwd")
	}
	seq := m.seq.next()
	status, err := read()
	if err != nil {
		return gitops.GitStatus{}, err
	}
	status.Seq = seq
	_, canon, err := canonicalize(cwd)
	if err != nil {
		// A path that read but will not canonicalize has no watcher: the
		// caller's answer stands, and no other client hears of it.
		log.Printf("gitwatch: publish for %q reached no watcher: %v", cwd, err)
		return status, nil
	}
	// Under m.mu, as Subscribe registers a watcher: the offer lands on the
	// registered watcher or in creating, never between them. offer does
	// not block.
	m.mu.Lock()
	defer m.mu.Unlock()
	if w := m.watchers[canon]; w != nil {
		return w.offer(status), nil
	}
	if pending := m.creating[canon]; pending != nil && status.Seq > pending.offered.Seq {
		pending.offered = status
	}
	return status, nil
}

// RequestRefresh asks the watcher for cwd (if one exists) to re-run
// StatusFn and broadcast the result to every subscriber, for a change no
// status read has shown yet (a PR the forge was just asked about). A
// caller holding a status it read hands it out through Publish instead.
// A requested refresh that observes an on-disk change the fs watches never
// reported is evidence of dead watchpoints (see run).
//
// No watcher for cwd is a valid state — nobody is subscribed — so it is
// a silent no-op rather than an error.
func (m *Manager) RequestRefresh(cwd string) {
	if cwd == "" {
		return
	}
	_, canon, err := canonicalize(cwd)
	if err != nil {
		// A path that will not canonicalize is a caller bug or a workspace
		// that vanished, and either way the refresh every other client was
		// counting on is not happening. No-watcher below is the only silent
		// outcome here.
		log.Printf("gitwatch: refresh request for %q dropped: %v", cwd, err)
		return
	}
	m.mu.Lock()
	w := m.watchers[canon]
	m.mu.Unlock()
	if w != nil {
		w.requestRefresh()
	}
}

// Suppress stops the watcher for cwd from computing and broadcasting status
// until the returned resume runs or the watcher stops. It is for a caller
// about to delete the workspace: the states git reports while the directory
// goes (every file deleted, then no repository) describe nothing a
// subscriber can act on. resume is idempotent; it re-enables broadcasting
// and requests a refresh. With no watcher for cwd there is nothing to hold
// and resume does nothing; a watcher created afterwards is not held.
func (m *Manager) Suppress(cwd string) (resume func()) {
	noop := func() {}
	if cwd == "" {
		return noop
	}
	_, canon, err := canonicalize(cwd)
	if err != nil {
		// Nothing canonicalizes to a path that is already gone, and no
		// watcher can be keyed by it either.
		return noop
	}
	m.mu.Lock()
	w := m.watchers[canon]
	m.mu.Unlock()
	if w == nil {
		return noop
	}
	return w.suppress()
}

func (m *Manager) shouldRefreshMissingPR(status gitops.GitStatus) bool {
	// A pending lookup (OpenPRLookupPending, which may carry the branch's
	// last known PR) needs the refresh to answer at all; a warm "no PR" is
	// re-checked too, because attach is the remote-change hook: creating
	// an MR does not touch the local filesystem, while refreshCh
	// coalescing and the PR cache keep repeated attaches from turning into
	// a request storm.
	return m.fastStatusFn != nil &&
		status.Branch != "" &&
		status.Forge != "" &&
		(status.OpenPRLookupPending || status.OpenPRURL == "")
}

func (m *Manager) watchRoots(cwd string) ([]gitops.WatchRoot, error) {
	roots, err := m.watchRootsFn(cwd)
	if err != nil {
		return nil, err
	}
	return normalizeWatchRoots(cwd, roots)
}

func (m *Manager) releaseSubscriber(cwd string, sub *Subscription) {
	m.mu.Lock()
	w, ok := m.watchers[cwd]
	if !ok {
		m.mu.Unlock()
		return
	}
	remaining := w.removeSubscriber(sub)
	if remaining == 0 {
		delete(m.watchers, cwd)
	}
	m.mu.Unlock()
	// Stop outside the manager lock — w.stop() blocks on the run
	// goroutine exiting, which can briefly contend on w.mu (NOT m.mu),
	// but holding m.mu the whole time would serialise unrelated
	// Subscribe / Close calls behind every teardown.
	if remaining == 0 {
		w.stop()
	}
}

// Close tears down every watcher. Safe to call multiple times. Blocks
// until all watcher goroutines exit so callers can rely on no further
// Updates() emissions after this returns.
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	watchers := make([]*workspaceWatcher, 0, len(m.watchers))
	for _, w := range m.watchers {
		watchers = append(watchers, w)
	}
	m.watchers = nil
	m.mu.Unlock()
	for _, w := range watchers {
		w.stop()
	}
}
