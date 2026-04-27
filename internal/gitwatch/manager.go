package gitwatch

import (
	"errors"
	"sync"

	"github.com/rjeczalik/notify"

	gitops "agent-overflow/internal/git"
)

// StatusFn returns the GitStatus for cwd. The Manager calls this on
// every debounce trailing edge and on each polling-fallback tick.
// Production wires gitops.Core.Status; tests can stub.
type StatusFn func(cwd string) (gitops.GitStatus, error)

// Manager owns the per-cwd watchers and subscriber fan-out. One Manager
// per *App is the expected wiring. Safe for concurrent use.
type Manager struct {
	statusFn StatusFn

	mu       sync.Mutex
	watchers map[string]*workspaceWatcher // canonical cwd → watcher
	closed   bool

	// installFn lets tests force the polling-fallback path without
	// actually exhausting inotify limits. Production leaves it nil and
	// the watcher uses installNotifyWatcher.
	installFn func(cwd string, ch chan<- notify.EventInfo) error
}

// NewManager constructs a Manager. statusFn is required.
func NewManager(statusFn StatusFn) *Manager {
	if statusFn == nil {
		panic("gitwatch: NewManager requires a non-nil StatusFn")
	}
	return &Manager{
		statusFn: statusFn,
		watchers: make(map[string]*workspaceWatcher),
	}
}

// Subscribe begins delivering status updates for cwd. Multiple
// subscriptions to the same cwd share one underlying watcher via
// refcount. The returned Subscription's Initial() carries the status at
// subscribe time so callers can render synchronously without waiting
// for the first fs event; Updates() yields subsequent changes.
//
// Fast path: if a watcher already exists for cwd, the new subscriber
// uses that watcher's cached lastStatus as Initial() — no statusFn
// call. This matters because statusFn shells out to git (and on the
// production path, also runs `gh pr list`), and watchers are commonly
// shared across panes pointing at the same workspace.
//
// Slow path: when there's no existing watcher, the initial fetch runs
// BEFORE the watcher is installed so a bad path / non-repo / git binary
// error fails fast without leaving a stray watcher behind.
func (m *Manager) Subscribe(cwd string) (*Subscription, error) {
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
		sub := w.addSubscriber(w.snapshotStatus())
		m.mu.Unlock()
		sub.closer = func() { m.releaseSubscriber(canon, sub) }
		return sub, nil
	}
	m.mu.Unlock()

	// Slow path: fetch initial outside the lock (statusFn shells out
	// to git and can take tens of milliseconds; holding m.mu here
	// would serialise unrelated Subscribe / Close calls behind it).
	initial, err := m.statusFn(canon)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("gitwatch: manager closed")
	}
	// Race recheck: a concurrent Subscribe may have created the
	// watcher while we were fetching. If so, prefer its lastStatus
	// (the freshest broadcast) over our own fetch.
	if w, ok := m.watchers[canon]; ok {
		sub := w.addSubscriber(w.snapshotStatus())
		m.mu.Unlock()
		sub.closer = func() { m.releaseSubscriber(canon, sub) }
		return sub, nil
	}
	w := newWorkspaceWatcher(canon, m.statusFn, initial)
	w.start(m.installFn)
	m.watchers[canon] = w
	sub := w.addSubscriber(initial)
	m.mu.Unlock()
	sub.closer = func() { m.releaseSubscriber(canon, sub) }
	return sub, nil
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
