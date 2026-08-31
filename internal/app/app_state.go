package app

import (
	"sync"
	"sync/atomic"
	"time"

	appbrowser "agent-overflow/internal/browser"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/sessionimport"
)

// The App struct's concern groups.
//
// Most of App's fields are single-owner: 70% are referenced by exactly one
// `app_*.go` besides `app.go` (docs/architecture/root-decomposition.md §(a)).
// Collapsing each such run into a named struct is what lets the struct be read
// as a list of concerns instead of a wall of 200 names, and it makes the cut
// line visible if a concern is ever promoted to its own package.
//
// Rules these groups follow, and any new one must:
//
//   - Plain named struct types stored as NAMED fields (`flushDispatch
//     appFlushDispatchState`), never embedded — an embedded group would promote its
//     field names back onto App and undo the whole point.
//   - A mutex moves into a group only WITH EVERY FIELD IT GUARDS. A group is
//     never a reason to separate a lock from its wards.
//   - Session lifecycle state is owned atomically by
//     `sessionruntime.Manager`; no compatibility session mutex or maps live in
//     these groups. Ambient dependencies (store, settings, configDir, triage,
//     logger, transportServer, telemetry, shuttingDown, ...) stay on App.
//   - Only genuinely single-cluster runs group. A field two clusters touch
//     stays top-level.

// appFlushDispatchState is the queued-message flush concern
// (`app_flush_queue*.go`): the per-thread dispatch batches and the two mutexes
// whose hierarchy RegisterQueueItem documents.
type appFlushDispatchState struct {
	// mu guards the per-thread dispatch bookkeeping below. Triage decides
	// whether a drain is boundary or immediate; App owns the asynchronous
	// provider writes so sequence allocation and Send/Steer locking stay in
	// the same layer.
	mu            sync.Mutex
	queues        map[string][]flushDispatchBatch
	current       map[string]flushDispatchBatch
	running       map[string]bool
	inflightItems map[string]int
	generation    map[string]uint64
	wg            sync.WaitGroup
	// handoffMu serializes RegisterQueueItem's enqueue→flush handoff
	// against the revert-on-interrupt predicate's read of the queued /
	// in-flight counters. tryFlushQueue deletes a batch from the triage queue
	// before the dispatcher records it as in-flight; in that window the item
	// is invisible to every counter the predicate consults. Holding this mutex
	// across both the handoff (RegisterQueueItem) and the counter read
	// (pendingFlushWorkCount) makes the queued message observable to a
	// concurrent Stop click as either still-queued or already-in-flight, never
	// neither. Deliberately NOT the per-thread action lock: that lock is held
	// for seconds by git / worktree ops, and queueing a message
	// must stay responsive while those run. See RegisterQueueItem for the full
	// lock hierarchy and deadlock-freedom argument.
	handoffMu sync.Mutex
}

// appPRUpdateState is the PR-scope review-pane polling concern
// (`app_forge_review.go`).
type appPRUpdateState struct {
	// pumps index active PR-scope polling by PR key
	// ("<forge>:<namespace>/<repo>:<number>"). Each PR owns ONE
	// low-cadence poller and one change-detection state however many
	// callers watch it, and emits only when the normalized snapshot (or
	// its failure) changes. handles maps each caller's wire-visible
	// SubscribePRUpdates id to its reference on that pump, which is what
	// UnsubscribePRUpdates, SetPRUpdatesActive, and the per-connection
	// cleanup act on. Both maps are guarded by mu; wg tracks pump goroutines
	// so Shutdown drains them before returning. seq stamps every stored pump
	// state so a subscriber can order the frames it sees against the state
	// its subscribe returned; it is GLOBAL rather than per-pump because a
	// pump can be replaced under its key (a dead pump's successor), and a
	// per-pump counter would restart at zero — letting the dead one's late
	// frames outrank the replacement's fresh state.
	mu       sync.Mutex
	pumps    map[string]*prUpdatePump
	handles  map[string]*prUpdateHandle
	seq      uint64
	wg       sync.WaitGroup
	interval time.Duration
	fetchFn  func(gitops.PRReference) (prUpdateSnapshot, error)
}

// appSessionImportState is the session-import concern
// (`app_session_import*.go`): the provider-home scan cache and the single
// in-flight import run.
type appSessionImportState struct {
	// Manager owns the provider-home scan cache and the one active import
	// run. Lazy initialization keeps bare App test fixtures valid.
	once    sync.Once
	manager *sessionimport.Manager
}

// appBrowserState is the provider-neutral arbitrary-web browser concern. The
// MCP listener is cheap and per-session-tokened; Manager owns the lazily
// launched Chrome process and workspace BrowserContexts.
type appBrowserState struct {
	manager *appbrowser.Manager
	mcp     *appbrowser.MCPServer
	// cdpRelay is the backend end of the Windows launcher's CDP tunnel,
	// handed in by the executable before startup (bootstrap.go) and non-nil
	// only on the WSL deployment. Its presence selects the hosted engine.
	cdpRelay           appbrowser.CDPRelay
	applyMu            sync.Mutex
	applyWG            sync.WaitGroup
	settingsGeneration atomic.Uint64
	liveEnabled        atomic.Bool
}

// appTurnObserverState fans provider events out to internal App features after
// triage handling has been attempted.
type appTurnObserverState struct {
	// Each registration lives until its returned unsubscribe function runs;
	// the built-in discussion observer lives for the App lifetime. mu is
	// deliberately independent of sessionruntime so callbacks can safely enter other
	// App coordination paths.
	mu             sync.RWMutex
	byThread       map[string]map[uint64]turnObserver
	nextID         uint64
	discussionOnce sync.Once
}

// appMarkThreadReadState joins the background thread read-state stamps
// (`app_session_bindings.go`).
type appMarkThreadReadState struct {
	// SwitchThread registers one stamp per focus so the RPC the UI blocks on
	// doesn't queue behind the store's single writer connection. stopped is
	// set inside the same critical section that the WaitGroup is joined from,
	// so no stamp can register after the wait has begun; Shutdown joins them
	// before the store closes.
	mu      sync.Mutex
	stopped bool
	wg      sync.WaitGroup
}
