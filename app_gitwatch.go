package main

import (
	"context"
	"fmt"
	"log"

	"github.com/google/uuid"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitwatch"
	"agent-overflow/internal/transport"
)

// GitStatusSubscriptionResult is the wire shape returned by
// GitStatusSubscribe. ID is the handle the frontend uses to filter
// "git:status" events and to call GitStatusUnsubscribe.
type GitStatusSubscriptionResult struct {
	ID     string           `json:"id"`
	Status gitops.GitStatus `json:"status"`
}

// GitStatusEvent is the shape pushed on the "git:status" channel.
// Frontend listens, filters by SubscriptionID, and updates its local
// status state. One event per actual change — gitwatch dedups
// against the previous status before broadcasting.
//
// Per-subscription emission is deliberate. N same-workspace panes do
// produce N copies of each change, but the expensive work (fs watcher,
// git subprocess, PR lookup) is already shared via gitwatch.Manager's
// per-cwd refcount; the duplicated part is a ~300-byte scalar struct at
// a debounced ≤4Hz cadence, and the transport's per-connection
// coalescing batches simultaneous copies into one frame. A shared
// `subscriptionIds []string` emit was evaluated (2026-06) and rejected:
// it would duplicate the manager's per-workspace bookkeeping at the app
// layer, change the wire contract, and mix subscription ids from
// different WS connections into one event for no perceivable win.
type GitStatusEvent struct {
	SubscriptionID string           `json:"subscriptionId"`
	Status         gitops.GitStatus `json:"status"`
}

// gitWatchPump is the App's per-id record for an active gitwatch
// subscription. The pump goroutine forwards Updates() to the wire and
// exits when either the underlying channel closes or the per-sub done
// signal closes (explicit Unsubscribe).
//
// Naming: the upstream handle is `gitwatch.Subscription`. The App's
// adapter is the *pump* that translates Subscription.Updates() into
// wire events — that's the responsibility this struct represents,
// not "another subscription".
type gitWatchPump struct {
	sub  *gitwatch.Subscription
	done chan struct{}
}

// GitStatusSubscribe begins streaming git-status updates for the
// thread's workspace. Returns the initial status synchronously so the
// caller can render immediately, plus a subscription id used to filter
// the "git:status" event channel and to call GitStatusUnsubscribe.
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

	// Opening a thread is an explicit UI attach. Drop cached open-PR lookups so
	// the async full refresh requested by gitwatch can see remote-only MR
	// creation that did not touch the local filesystem.
	a.gitCore().InvalidatePRCache(workspace)
	sub, err := a.gitWatch.Subscribe(workspace)
	if err != nil {
		return GitStatusSubscriptionResult{}, fmt.Errorf("gitwatch subscribe: %w", err)
	}

	id := uuid.NewString()
	entry := &gitWatchPump{
		sub:  sub,
		done: make(chan struct{}),
	}
	a.gitWatchPumpsMu.Lock()
	a.gitWatchPumps[id] = entry
	a.gitWatchPumpsMu.Unlock()

	a.gitWatchPumpWG.Go(func() { a.pumpGitWatch(id, entry) })

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

	return GitStatusSubscriptionResult{ID: id, Status: sub.Initial()}, nil
}

// GitStatusUnsubscribe releases a subscription previously created via
// GitStatusSubscribe. Idempotent — unknown ids and double-unsubscribes
// are no-ops because the connection-cleanup safety net may have run
// first on disconnect.
func (a *App) GitStatusUnsubscribe(subscriptionID string) error {
	a.unsubscribeGitWatch(subscriptionID)
	return nil
}

// pumpGitWatch forwards a Subscription's Updates() to the wire as
// "git:status" events. Exits when either the underlying channel closes
// (Manager teardown / refcount hit zero) or done is closed (explicit
// Unsubscribe path).
//
// The select prefers done over Updates() on every iteration AFTER a
// value is received, so an Unsubscribe that closes done can't lose to
// a buffered Updates() value sneaking through one final emit.
func (a *App) pumpGitWatch(id string, entry *gitWatchPump) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("gitwatch: pump panic for id=%s: %v", id, r)
		}
		// Self-clean if the channel closed under us (Manager.Close
		// path). The Unsubscribe path already deleted the entry
		// before reaching here; double-delete is a benign no-op.
		a.gitWatchPumpsMu.Lock()
		delete(a.gitWatchPumps, id)
		a.gitWatchPumpsMu.Unlock()
	}()
	for {
		select {
		case <-entry.done:
			return
		case status, ok := <-entry.sub.Updates():
			if !ok {
				return
			}
			// Re-check done before emitting: when both arms are ready
			// Go's select picks at random, so a closed done racing a
			// pending Updates() value could otherwise leak one final
			// wire event past Unsubscribe. The explicit recheck makes
			// the "done wins after closing" guarantee real.
			select {
			case <-entry.done:
				return
			default:
			}
			a.emit("git:status", GitStatusEvent{
				SubscriptionID: id,
				Status:         status,
			})
		}
	}
}

func (a *App) unsubscribeGitWatch(id string) {
	a.gitWatchPumpsMu.Lock()
	entry, ok := a.gitWatchPumps[id]
	if ok {
		delete(a.gitWatchPumps, id)
	}
	a.gitWatchPumpsMu.Unlock()
	if !ok {
		return
	}
	// Order matters: close done FIRST so the pump goroutine stops
	// translating new Updates() values into wire events before we tear
	// the underlying subscription down. Then Close the subscription so
	// the gitwatch refcount drops and (if last sub on this cwd) the
	// watcher tears down its goroutine + fs watcher.
	close(entry.done)
	entry.sub.Close()
}
