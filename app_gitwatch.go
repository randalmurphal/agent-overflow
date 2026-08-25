package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/google/uuid"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitwatch"
	"agent-overflow/internal/transport"
)

// maxGitWatchHandles bounds the outstanding GitStatusSubscribe handles for
// the whole app. Each distinct cwd behind them costs a recursive fs watch
// (hundreds of inotify watchpoints on a real repo) and a git status cadence,
// so an unbounded handle map is a resource-exhaustion surface reachable by
// anything that can call the binding in a loop — a compromised renderer, a
// buggy caller that never unsubscribes. Panes are counted in single digits;
// this is three orders of magnitude above any real UI.
const maxGitWatchHandles = 256

// ErrTooManyGitStatusSubscriptions is returned once the handle cap is hit.
// Typed so the frontend can tell it apart from a workspace that failed to
// resolve — this one is never fixed by retrying the same call.
var ErrTooManyGitStatusSubscriptions = errors.New("gitwatch: too many active git status subscriptions")

// GitStatusSubscriptionResult is the wire shape returned by
// GitStatusSubscribe. ID is the per-caller handle used to call
// GitStatusUnsubscribe. Cwd is the canonical workspace path the stream is
// keyed on — the frontend maps it back to whatever local spelling it asked
// with, because "git:status" events are addressed by cwd.
type GitStatusSubscriptionResult struct {
	ID     string           `json:"id"`
	Cwd    string           `json:"cwd"`
	Status gitops.GitStatus `json:"status"`
}

// GitStatusEvent is the shape pushed on the "git:status" channel. One
// event per actual change per cwd — gitwatch dedups against the previous
// status before broadcasting, and the App forwards each cwd's stream
// exactly once regardless of how many callers subscribed to it.
//
// The event carries no subscription id on purpose. Git status is workspace
// state, so the wire key is the workspace: two panes on one worktree are
// the normal case, and addressing them by subscription made each hold a
// private copy that could disagree with the other's for minutes.
type GitStatusEvent struct {
	Cwd    string           `json:"cwd"`
	Status gitops.GitStatus `json:"status"`
}

// gitWatchPump is the App's per-cwd record for an active gitwatch stream:
// one gitwatch.Subscription and one goroutine forwarding its Updates() to
// the wire, shared by every caller of that cwd via refs. The goroutine
// exits when either the underlying channel closes or done is closed (the
// last caller unsubscribed).
//
// Naming: the upstream handle is `gitwatch.Subscription`. The App's
// adapter is the *pump* that translates Subscription.Updates() into wire
// events — that's the responsibility this struct represents, not "another
// subscription".
//
// refs and dead are guarded by App.gitStatus.mu, never by the pump
// itself. `dead` is set by the goroutine's own teardown: a pump whose
// Updates() channel closed under it (Manager.Close) forwards nothing ever
// again, so a caller must not be handed a reference on it — it would get a
// subscription id that receives no events and releases nothing.
type gitWatchPump struct {
	cwd  string
	sub  *gitwatch.Subscription
	done chan struct{}
	refs int
	dead bool
}

// GitStatusSubscribe begins streaming git-status updates for the
// thread's workspace. Returns the initial status synchronously so the
// caller can render immediately, plus the canonical cwd the stream is
// keyed on and a handle used to call GitStatusUnsubscribe.
//
// LocalOnly: workspace paths are local FS paths and gitwatch can spawn
// recursive fs watches plus continuous git invocations; exposing this
// surface to LAN peers would leak repo locations and let a token-only
// peer enumerate threads via probe attempts.
//
// The subscription is automatically released when the calling WS
// connection drops (via transport.ConnState cleanup). The frontend
// SHOULD still call GitStatusUnsubscribe on unmount; the
// connection-tied cleanup is the safety net for unclean disconnects.
func (a *App) GitStatusSubscribe(ctx context.Context, threadID string) (GitStatusSubscriptionResult, error) {
	if a.shuttingDown.Load() {
		return GitStatusSubscriptionResult{}, ErrShuttingDown
	}
	if a.gitWatch == nil {
		return GitStatusSubscriptionResult{}, fmt.Errorf("gitwatch: manager not initialised")
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return GitStatusSubscriptionResult{}, err
	}
	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return GitStatusSubscriptionResult{}, err
	}

	// Capacity is checked BEFORE anything is acquired: a capped caller would
	// otherwise drop the PR cache and stand up a recursive fs watch plus a
	// git-status pass for a workspace it is about to be refused. The check
	// under the pump lock below stays the authoritative one — it is the only
	// point where the handle actually lands — so a caller that loses a race
	// for the last slot still cannot exceed the cap.
	a.gitStatus.mu.Lock()
	atCap := len(a.gitStatus.handles) >= maxGitWatchHandles
	a.gitStatus.mu.Unlock()
	if atCap {
		return GitStatusSubscriptionResult{}, ErrTooManyGitStatusSubscriptions
	}

	// Opening a thread is an explicit UI attach. Drop cached open-PR lookups so
	// the async full refresh requested by gitwatch can see remote-only MR
	// creation that did not touch the local filesystem.
	a.gitCore().InvalidatePRCache(workspace)
	// Subscribe unconditionally, even when a pump for this cwd already
	// exists: it is the manager's own canonicalization of the path, it
	// hands back the watcher's freshest snapshot under the watcher lock,
	// and it re-arms the attach-time PR re-check. On the shared path the
	// handle is released again immediately below — the pump's own
	// subscription is what keeps the watcher alive.
	sub, err := a.gitWatch.Subscribe(workspace)
	if err != nil {
		return GitStatusSubscriptionResult{}, fmt.Errorf("gitwatch subscribe: %w", err)
	}
	cwd, initial := sub.Cwd(), sub.Initial()

	id := uuid.NewString()
	a.gitStatus.mu.Lock()
	if len(a.gitStatus.handles) >= maxGitWatchHandles {
		a.gitStatus.mu.Unlock()
		sub.Close()
		return GitStatusSubscriptionResult{}, ErrTooManyGitStatusSubscriptions
	}
	pump, shared := a.gitStatus.pumps[cwd]
	// A dead pump reads as absent. Its goroutine has already stopped
	// forwarding, so sharing it would hand back a handle that receives
	// nothing; the fresh pump replaces the map entry and the dead one's
	// own drop leaves it alone (it checks identity) while still releasing
	// exactly the handles that referenced IT.
	if shared && pump.dead {
		shared = false
	}
	if shared {
		pump.refs++
	} else {
		pump = &gitWatchPump{cwd: cwd, sub: sub, done: make(chan struct{}), refs: 1}
		a.gitStatus.pumps[cwd] = pump
	}
	a.gitStatus.handles[id] = pump
	a.gitStatus.mu.Unlock()

	if shared {
		sub.Close()
	} else {
		a.gitStatus.wg.Go(func() { a.pumpGitWatch(pump) })
	}

	if state := transport.ConnStateFromContext(ctx); state != nil {
		// If RegisterCleanup returns false the connection is already
		// tearing down — the safety net it would have provided is
		// gone, so eagerly unsubscribe now to avoid a watcher leak
		// until app shutdown.
		if !state.RegisterCleanup(func() { a.unsubscribeGitWatch(id) }) {
			a.unsubscribeGitWatch(id)
			return GitStatusSubscriptionResult{}, fmt.Errorf("gitwatch: connection closing")
		}
	}

	return GitStatusSubscriptionResult{ID: id, Cwd: cwd, Status: initial}, nil
}

// GitStatusUnsubscribe releases a subscription previously created via
// GitStatusSubscribe. Idempotent — unknown ids and double-unsubscribes
// are no-ops because the connection-cleanup safety net may have run
// first on disconnect.
func (a *App) GitStatusUnsubscribe(subscriptionID string) error {
	a.unsubscribeGitWatch(subscriptionID)
	return nil
}

// pumpGitWatch forwards a cwd's Subscription updates to the wire as
// "git:status" events. Exits when either the underlying channel closes
// (Manager teardown) or done is closed (the last caller unsubscribed).
//
// The select prefers done over Updates() on every iteration AFTER a
// value is received, so an Unsubscribe that closes done can't lose to
// a buffered Updates() value sneaking through one final emit.
func (a *App) pumpGitWatch(pump *gitWatchPump) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("gitwatch: pump panic for cwd=%s: %v", pump.cwd, r)
		}
		// Self-clean if the channel closed under us (Manager.Close
		// path). The Unsubscribe path already dropped the pump before
		// reaching here; dropping again is a benign no-op.
		a.dropGitWatchPump(pump)
	}()
	for {
		select {
		case <-pump.done:
			return
		case status, ok := <-pump.sub.Updates():
			if !ok {
				return
			}
			// Re-check done before emitting: when both arms are ready
			// Go's select picks at random, so a closed done racing a
			// pending Updates() value could otherwise leak one final
			// wire event past the last Unsubscribe. The explicit recheck
			// makes the "done wins after closing" guarantee real.
			select {
			case <-pump.done:
				return
			default:
			}
			a.emit("git:status", GitStatusEvent{Cwd: pump.cwd, Status: status})
		}
	}
}

// unsubscribeGitWatch releases one caller's handle. The pump (and the
// gitwatch subscription under it) survives until the last handle goes.
func (a *App) unsubscribeGitWatch(id string) {
	a.gitStatus.mu.Lock()
	pump, ok := a.gitStatus.handles[id]
	if !ok {
		a.gitStatus.mu.Unlock()
		return
	}
	delete(a.gitStatus.handles, id)
	pump.refs--
	var teardown *gitWatchPump
	if pump.refs <= 0 {
		teardown = pump
		// Only if it is still the pump serving this cwd — a superseded
		// pump was replaced in the map and must not evict its successor.
		if a.gitStatus.pumps[pump.cwd] == pump {
			delete(a.gitStatus.pumps, pump.cwd)
		}
	}
	a.gitStatus.mu.Unlock()
	if teardown == nil {
		return
	}
	// Order matters: close done FIRST so the pump goroutine stops
	// translating new Updates() values into wire events before we tear
	// the underlying subscription down. Then Close the subscription so
	// the gitwatch refcount drops and (if last sub on this cwd) the
	// watcher tears down its goroutine + fs watcher.
	close(teardown.done)
	teardown.sub.Close()
}

// dropGitWatchPump removes a pump whose Updates() channel closed under it
// (Manager.Close), along with every handle that referenced it — those
// handles name a stream that no longer exists.
//
// The `dead` stamp goes on FIRST and unconditionally, so a Subscribe that
// takes the lock after this point mints a fresh pump instead of taking a
// reference on a goroutine that has stopped. The residual window is a
// Subscribe that took the lock BEFORE this ran: it is unclosable from here
// (the goroutine cannot mark the pump under the lock any earlier than its
// own teardown), and it is shutdown-only — the channel closes on
// Manager.Close, after which gitwatch.Subscribe itself refuses.
func (a *App) dropGitWatchPump(pump *gitWatchPump) {
	a.gitStatus.mu.Lock()
	defer a.gitStatus.mu.Unlock()
	pump.dead = true
	if a.gitStatus.pumps[pump.cwd] == pump {
		delete(a.gitStatus.pumps, pump.cwd)
	}
	for id, held := range a.gitStatus.handles {
		if held == pump {
			delete(a.gitStatus.handles, id)
		}
	}
}
