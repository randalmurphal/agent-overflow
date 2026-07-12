package replay

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// defaultQueueSize is how many un-written records we buffer per
	// manager before dropping. Sized for bursty provider output (thousands
	// of tokens per second) without holding too much in memory.
	defaultQueueSize = 4096

	// defaultIdleTimeout is how long a writer may sit without writes
	// before the reaper closes it. Five minutes matches the task spec.
	defaultIdleTimeout = 5 * time.Minute

	// reaperInterval is how often the cleanup goroutine scans for idle
	// writers. Chosen to be short enough that a user restarting a thread
	// doesn't block on the old writer, and long enough that the loop
	// doesn't add meaningful CPU cost.
	reaperInterval = 30 * time.Second
)

// Manager owns a set of per-thread Writers plus a goroutine that drains a
// bounded queue of records. The manager is the only API triage and app code
// should use — they enqueue records via Enqueue, the manager writes them on
// a background goroutine.
//
// Lifecycle: while disabled the manager owns no goroutines. Toggling to
// enabled starts a fresh drain+reap pair bound to a new context; toggling
// back to disabled cancels that context and waits for both loops to exit.
// Construction with Enabled:false therefore has zero goroutine footprint,
// matching the "no runtime cost when off" contract in ManagerConfig.
type Manager struct {
	rootDir string

	enabled atomic.Bool

	mu      sync.Mutex
	writers map[string]*Writer // key: threadID

	// queueSize / writerCfg / idleTimeout are immutable after
	// construction; the queue is rebuilt each time we enable (see
	// startLoops) so a disabled period cannot leak buffered records.
	queueSize   int
	writerCfg   WriterConfig
	idleTimeout time.Duration

	dropHook func()

	// lifecycleMu serializes enable/disable transitions and Shutdown
	// against Enqueue. Writers (SetEnabled, Shutdown) hold the write
	// lock when swapping queue/cancel so an Enqueue holding the read
	// lock sees a consistent (queue != nil) view for the duration of
	// its channel send. Many Enqueues can run in parallel; toggles
	// block them for the duration of the transition only.
	lifecycleMu sync.RWMutex
	queue       chan Record
	cancel      context.CancelFunc
	loopWG      sync.WaitGroup
	closed      atomic.Bool

	// inflight counts records that have left the queue but haven't been
	// fully written to disk. waitForDrain uses this to tell tests "all
	// records are on disk, you can now open the file."
	inflight atomic.Int64

	// lost counts records that were accepted for logging but never made
	// it to disk — queue-full drops and write failures alike. The
	// harness recording flow compares LostCount across its capture
	// window: a drained queue only proves everything *queued* was
	// written, not that nothing was lost on the way in.
	lost atomic.Int64
}

// ManagerConfig tunes manager behaviour.
type ManagerConfig struct {
	// RootDir is the directory where per-thread files live.
	// Files are placed at RootDir/<threadID>.jsonl.
	RootDir string
	// QueueSize bounds the in-memory queue. Zero uses defaultQueueSize.
	QueueSize int
	// WriterConfig is passed through to each Writer at creation time.
	WriterConfig WriterConfig
	// IdleTimeout is how long a writer may sit without writes before it is
	// closed. Zero uses defaultIdleTimeout.
	IdleTimeout time.Duration
	// Enabled sets the initial enabled state. Callers can toggle at
	// runtime via SetEnabled.
	Enabled bool
	// DropHook is invoked every time a record is dropped due to a full
	// queue. Used for metrics; may be nil.
	DropHook func()
}

// NewManager constructs a Manager. Background goroutines are only started
// when the manager is enabled — either via ManagerConfig.Enabled at
// construction or via a later SetEnabled(true). Disabled construction
// therefore has zero goroutine footprint.
func NewManager(cfg ManagerConfig) *Manager {
	qsize := cfg.QueueSize
	if qsize <= 0 {
		qsize = defaultQueueSize
	}
	idle := cfg.IdleTimeout
	if idle <= 0 {
		idle = defaultIdleTimeout
	}

	m := &Manager{
		rootDir:     cfg.RootDir,
		writers:     make(map[string]*Writer),
		queueSize:   qsize,
		writerCfg:   cfg.WriterConfig,
		idleTimeout: idle,
		dropHook:    cfg.DropHook,
	}
	if cfg.Enabled {
		m.enabled.Store(true)
		m.startLoops()
	}
	return m
}

// startLoops builds a fresh queue + cancel context and starts the drain
// and reap goroutines. Must be called with lifecycleMu held, with no
// loops currently running.
func (m *Manager) startLoops() {
	if m.queue != nil {
		// Already running; this branch guards against a logic bug rather
		// than a legitimate caller state, so we simply return.
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.queue = make(chan Record, m.queueSize)
	m.cancel = cancel
	m.loopWG.Go(func() { m.drain(ctx) })
	m.loopWG.Go(func() { m.reap(ctx) })
}

// stopLoops cancels the background context, waits for drain + reap to
// exit, and releases the queue channel. Must be called with lifecycleMu
// held. Safe to call when loops are already stopped.
func (m *Manager) stopLoops() {
	if m.queue == nil {
		return
	}
	m.cancel()
	m.loopWG.Wait()
	m.cancel = nil
	m.queue = nil
}

// Enabled reports whether the manager will accept new records. Safe to call
// from multiple goroutines; reads are atomic.
func (m *Manager) Enabled() bool {
	if m == nil {
		return false
	}
	return m.enabled.Load()
}

// SetEnabled toggles the manager at runtime. Enabling a disabled manager
// spins up the drain + reap goroutines on a fresh context; disabling an
// enabled manager cancels that context, waits for the loops to exit, and
// closes all open writers. The net result is that a disabled manager
// holds no goroutines and no file descriptors.
//
// Toggling takes effect immediately: after SetEnabled(true) returns,
// Enqueue will accept records; after SetEnabled(false) returns, Enqueue
// will reject them and no background work remains.
func (m *Manager) SetEnabled(v bool) {
	if m == nil {
		return
	}
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	// Shutdown has permanently disabled the manager; further enable
	// requests would leak goroutines that no one can stop (closed is
	// a one-way latch and Shutdown is idempotent).
	if m.closed.Load() {
		m.enabled.Store(false)
		return
	}

	if !m.enabled.CompareAndSwap(!v, v) {
		// State didn't change; nothing to do.
		return
	}
	if v {
		m.startLoops()
		return
	}
	m.stopLoops()
	// Close every open writer so the disabled state owns no FDs. A
	// future re-enable will lazily re-open files on first write.
	m.mu.Lock()
	for id, w := range m.writers {
		if err := w.Close(); err != nil {
			log.Printf("replay: close writer %s on disable: %v", id, err)
		}
		delete(m.writers, id)
	}
	m.mu.Unlock()
}

// Enqueue offers a record to the background writer. Returns true on success,
// false when the queue is full or the manager is disabled/closed. When
// Enqueue returns false the configured DropHook is invoked — callers should
// not retry, the event is lost.
//
// We bump inflight BEFORE enqueueing so there's no window where the
// record has left the queue but inflight has not yet been incremented —
// that window is what waitForDrain was racing against.
//
// The read-lock on lifecycleMu is held across the channel send so a
// concurrent SetEnabled(false) cannot reap the queue out from under us.
func (m *Manager) Enqueue(rec Record) bool {
	if m == nil || m.closed.Load() || !m.enabled.Load() {
		return false
	}
	m.lifecycleMu.RLock()
	defer m.lifecycleMu.RUnlock()
	// Re-check under the lock: the manager could have been disabled in
	// the window between the atomic reads above and the lock
	// acquisition. Without this, an Enqueue that squeaks in just after
	// SetEnabled(false) would push onto a soon-to-be-nil queue.
	if m.closed.Load() || !m.enabled.Load() || m.queue == nil {
		return false
	}
	m.inflight.Add(1)
	select {
	case m.queue <- rec:
		return true
	default:
		m.inflight.Add(-1)
		m.lost.Add(1)
		if m.dropHook != nil {
			m.dropHook()
		}
		return false
	}
}

// LostCount reports how many records have been lost since construction —
// queue-full drops plus write failures. Monotonic; capture flows compare
// values across a window to detect loss.
func (m *Manager) LostCount() int64 {
	if m == nil {
		return 0
	}
	return m.lost.Load()
}

// QueueLen returns the current queue depth. Primarily used in tests.
// Returns 0 when the manager is disabled and has no queue.
func (m *Manager) QueueLen() int {
	if m == nil {
		return 0
	}
	m.lifecycleMu.RLock()
	defer m.lifecycleMu.RUnlock()
	if m.queue == nil {
		return 0
	}
	return len(m.queue)
}

// Shutdown drains the queue (bounded by ctx), closes all writers, and stops
// background goroutines. Subsequent Enqueue calls return false and
// subsequent SetEnabled(true) calls are no-ops.
func (m *Manager) Shutdown(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()

	if !m.closed.CompareAndSwap(false, true) {
		return nil
	}
	m.enabled.Store(false)

	if m.queue != nil {
		m.cancel()
		done := make(chan struct{})
		go func() {
			m.loopWG.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			// Loops didn't exit in time; fall through and close writers anyway.
		}
		m.cancel = nil
		m.queue = nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	var firstErr error
	for id, w := range m.writers {
		if err := w.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("replay: close writer %s: %w", id, err)
		}
		delete(m.writers, id)
	}
	return firstErr
}

// drain pulls records off the queue and writes them to the per-thread file.
// Runs on its own goroutine. Exits when ctx is cancelled or the queue is
// drained after cancellation.
//
// Enqueue increments inflight before the record enters the queue; drain
// decrements it after the write completes. The net result is that
// inflight == 0 implies "every enqueued record has been written" — the
// invariant waitForDrain depends on.
//
// The queue is captured in a local so a concurrent SetEnabled cannot nil
// out m.queue mid-loop: we always read from the channel we were handed
// at startLoops time, even if the field has since been reset.
func (m *Manager) drain(ctx context.Context) {
	queue := m.queue
	for {
		select {
		case <-ctx.Done():
			// Drain anything still in the queue before exiting so we
			// don't lose records the triage loop already handed off.
			for {
				select {
				case rec := <-queue:
					m.writeRecord(rec)
					m.inflight.Add(-1)
				default:
					return
				}
			}
		case rec := <-queue:
			m.writeRecord(rec)
			m.inflight.Add(-1)
		}
	}
}

// writeRecord looks up or creates the writer for rec.ThreadID and writes.
// Errors are logged, not returned — the drain loop is fire-and-forget —
// but every failure counts toward LostCount so capture flows can tell a
// complete log from a lossy one.
func (m *Manager) writeRecord(rec Record) {
	w, err := m.writerFor(rec.ThreadID)
	if err != nil {
		m.lost.Add(1)
		log.Printf("replay: open writer for thread %s: %v", rec.ThreadID, err)
		return
	}
	if err := w.Write(rec); err != nil {
		m.lost.Add(1)
		log.Printf("replay: write thread %s: %v", rec.ThreadID, err)
	}
}

// writerFor returns an open Writer for the thread, creating one if needed.
func (m *Manager) writerFor(threadID string) (*Writer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.writers[threadID]; ok {
		return w, nil
	}
	path := filepath.Join(m.rootDir, threadID+".jsonl")
	w, err := NewWriter(path, m.writerCfg)
	if err != nil {
		return nil, err
	}
	m.writers[threadID] = w
	return w, nil
}

// reap closes writers that have been idle for IdleTimeout. Runs on its own
// goroutine.
func (m *Manager) reap(ctx context.Context) {
	ticker := time.NewTicker(reaperInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.evictIdle()
		}
	}
}

// evictIdle closes and forgets writers whose LastAccess is older than
// idleTimeout. Exported in name only for readability in the reap method.
func (m *Manager) evictIdle() {
	cutoff := time.Now().Add(-m.idleTimeout)
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, w := range m.writers {
		if w.LastAccess().Before(cutoff) {
			if err := w.Close(); err != nil {
				log.Printf("replay: close idle writer %s: %v", id, err)
			}
			delete(m.writers, id)
		}
	}
}

// openCount reports how many writers the manager currently holds. Used by
// tests.
func (m *Manager) openCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.writers)
}

// RemoveThreadLog closes the writer for threadID (if any) and removes the
// per-thread replay file plus any rotated backups (.1 / .2 / .3). Called by
// deleteThreadTree so thread deletion does not leave orphan replay logs on
// disk. Missing files are not an error — callers delete threads that may
// never have been recorded.
func (m *Manager) RemoveThreadLog(threadID string) error {
	if m == nil || threadID == "" {
		return nil
	}

	// Close the writer first so the file descriptor is released before we
	// unlink. On Linux the unlink would succeed anyway (the file stays
	// alive until the fd closes) but on Windows it would fail with a
	// sharing violation, and even on Linux a dangling writer trying to
	// rotate would look up the now-deleted file.
	m.mu.Lock()
	w, ok := m.writers[threadID]
	if ok {
		delete(m.writers, threadID)
	}
	m.mu.Unlock()
	var closeErr error
	if w != nil {
		if err := w.Close(); err != nil {
			closeErr = fmt.Errorf("replay: close writer for %s: %w", threadID, err)
		}
	}

	path := filepath.Join(m.rootDir, threadID+".jsonl")
	var removalErr error
	for _, candidate := range []string{path, path + ".1", path + ".2", path + ".3"} {
		if err := os.Remove(candidate); err != nil && !os.IsNotExist(err) {
			// Record the first removal error but keep trying so a
			// read-only backup doesn't block the main file's deletion.
			if removalErr == nil {
				removalErr = fmt.Errorf("replay: remove %s: %w", candidate, err)
			}
		}
	}

	switch {
	case closeErr != nil && removalErr != nil:
		return fmt.Errorf("%w; %w", closeErr, removalErr)
	case closeErr != nil:
		return closeErr
	case removalErr != nil:
		return removalErr
	}
	return nil
}

// WaitForDrain blocks until either (a) the queue is empty AND no record is
// mid-write, or (b) ctx is cancelled. Used by tests to let the background
// goroutine finish processing before inspecting the on-disk file, and by
// the harness recording flow to guarantee a captured bundle contains every
// event emitted before the capture point.
func (m *Manager) WaitForDrain(ctx context.Context) error {
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		if m.QueueLen() == 0 && m.inflight.Load() == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
