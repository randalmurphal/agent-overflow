package codexthread

import (
	"context"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/codexghost"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

const reopenReconcileTimeout = 30 * time.Second

// retireCodexBackgroundRuntime runs before every Codex session start. The old
// app-server is gone at this point, so its running terminals become
// errored/lost and its completed spawn cards become inactive while retaining
// the child ownership needed for history recovery.
//
// A child thread's identity and history can be resumed by a later app-server.
// Its active task cannot. Keeping the ownership while clearing only live
// runtime state lets resume recover a missed terminal result without claiming
// that the prior task is still running.
//
// The post-spawn probe handles a later systemError through the same store
// transition. It cannot replace this pre-spawn path because a clean prior
// process exit does not make the replacement process report systemError.
//
// Ordering: this runs before a replacement process starts. A later typed child
// turn/started event reactivates the spawn and removes the session-end reason.
//
// Claude does NOT use this path: its `stop_task` primitive and natural
// completion events settle backgrounded items; a Claude subprocess dying
// is handled by the existing error-stream plumbing. This method is
// Codex-only by caller scope.
func (a *Service) RetireBackgroundRuntime(threadID string) error {
	if a == nil || a.store == nil {
		return nil
	}
	retired, err := a.store.RetireCodexBackgroundRuntime(threadID, codexghost.GhostSummary, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("retire Codex background runtime for %s: %w", threadID, err)
	}
	a.emitRetiredItems(retired)
	return nil
}

// emitRetiredItems pushes retired rows as a page reads them: a spawn row
// with children carries its descendant aggregate at read time, so the
// write's read-back is not the row a client may later prove fresh
// (docs/architecture/thread-replica-sync.md §3.1). When the read fails
// the rows go out unstamped, so a reopen pages instead.
func (a *Service) emitRetiredItems(retired []store.Item) {
	byThread := make(map[string][]string)
	order := make([]string, 0, len(retired))
	for _, item := range retired {
		if _, seen := byThread[item.ThreadID]; !seen {
			order = append(order, item.ThreadID)
		}
		byThread[item.ThreadID] = append(byThread[item.ThreadID], item.ID)
	}
	for _, threadID := range order {
		rows, err := a.store.ListWireItems(threadID, byThread[threadID])
		if err != nil {
			log.Printf("app: read retired Codex rows for %s: %v", threadID, err)
			for _, item := range retired {
				if item.ThreadID == threadID {
					item.Rev = store.UnstampedItemRev
					a.emit(eventchan.ProviderItemEvent, triage.NewItemStreamUpsert(item))
				}
			}
			continue
		}
		for _, row := range rows {
			a.emit(eventchan.ProviderItemEvent, triage.NewItemStreamUpsert(row))
		}
	}
}

// RecoverBackgroundRuntimeOnStartup retires the Codex background runtime
// state the previous app process left and pushes the rows it retired. A
// failure can follow a committed retirement (the card flush before it), so
// the retired rows are pushed either way and the error is returned.
func (a *Service) RecoverBackgroundRuntimeOnStartup() error {
	if a == nil || a.store == nil {
		return nil
	}
	retired, err := a.store.RecoverCodexBackgroundRuntime(codexghost.GhostSummary, time.Now().UnixMilli())
	if len(retired) > 0 {
		log.Printf("app: retired %d Codex background items from the prior app instance", len(retired))
	}
	a.emitRetiredItems(retired)
	if err != nil {
		return fmt.Errorf("app: recover Codex background runtime: %w", err)
	}
	return nil
}

// ReconcileOnReopen probes a Codex thread's liveness via
// `thread/read` after the session has spawned and returns a classified
// result the caller uses to sequence follow-up work:
//
//   - idle / active   → session is alive. Pre-spawn retirement already handled
//     prior-process runtime state, and live completions will arrive over wire.
//   - notLoaded       → call `thread/resume` to rehydrate. We return a
//     NeedsResume hint so the caller can sequence it.
//   - systemError     → retire any runtime state emitted after the pre-spawn
//     transition. This covers a process that fails during resume/replay.
//   - unknown kind    → log and fall back to systemError behaviour so a
//     new enum value doesn't silently mask lost work.
//
// The Codex adapter must already be connected in the session manager.
// An error only surfaces transport/database
// failures — a `systemError` verdict is a successful probe.
func (a *Service) ReconcileOnReopen(ctx context.Context, threadID string) (ReconcileResult, error) {
	if a.isShuttingDown() {
		return ReconcileResult{}, a.shutdownError()
	}
	sess, ok := a.session(threadID)
	if !ok {
		return ReconcileResult{}, fmt.Errorf("app: reconcile codex: no active session for thread %s", threadID)
	}
	return a.reconcileSession(ctx, threadID, sess)
}

// reconcileSession is ReconcileOnReopen for one session of the thread. The
// verdict is about that session: what it asks for is done only while the
// session is still the thread's (whileThreadSession). A session that
// replaced it retired the prior runtime state at its own start and runs
// its own reconcile.
func (a *Service) reconcileSession(ctx context.Context, threadID string, sess LiveSession) (ReconcileResult, error) {
	if a.isShuttingDown() {
		return ReconcileResult{}, a.shutdownError()
	}
	if sess.Session == nil {
		return ReconcileResult{}, fmt.Errorf("app: reconcile codex: thread %s is not a Codex thread", threadID)
	}

	probe, err := sess.Session.Probe(ctx)
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("app: reconcile codex probe: %w", err)
	}

	runningBg, err := a.store.CountLiveRunningBackgroundToolCalls(threadID)
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("app: reconcile codex count running: %w", err)
	}

	result := ReconcileResult{
		ThreadID: threadID,
		Status:   probe.Status,
		Running:  runningBg,
	}

	switch probe.Status {
	case codex.ThreadStatusIdle, codex.ThreadStatusActive:
		// Session alive; nothing to flip.
		return result, nil

	case codex.ThreadStatusNotLoaded:
		// `notLoaded` means the thread isn't in memory, not that it's
		// dead. Resume rehydrates. Caller uses NeedsResume to sequence
		// the follow-up thread/resume call.
		result.NeedsResume = true
		return result, nil

	case codex.ThreadStatusSystemError:
		// Pre-spawn retirement covered the prior process. Anything still in
		// `runningBg` here is either a warm-reconnect resurrection that
		// the subprocess has since failed on, or a row inserted
		// post-spawn that immediately became unreachable. Either way,
		// use the same transition so the summary suffix stays idempotent.

	default:
		log.Printf("app: reconcile codex: unknown thread status %q; treating as systemError", probe.Status)
	}
	err = a.whileThreadSession(ctx, threadID, sess, func() error {
		retired, err := a.store.RetireCodexBackgroundRuntime(threadID, codexghost.GhostSummary, time.Now().UnixMilli())
		if err != nil {
			return fmt.Errorf("app: reconcile Codex runtime retirement: %w", err)
		}
		for _, item := range retired {
			if item.Status == "errored" {
				result.Flipped++
			}
		}
		a.emitRetiredItems(retired)
		return nil
	})
	return result, err
}

// whileThreadSession runs act under the thread lock if sess is still the
// thread's session, and does nothing otherwise. A restart holds that lock
// from its stop through its start, so act never runs against the thread
// of a session that replaced sess.
func (a *Service) whileThreadSession(ctx context.Context, threadID string, sess LiveSession, act func() error) error {
	if a.deps.LockThread == nil {
		return fmt.Errorf("app: reconcile codex: no thread lock for %s", threadID)
	}
	unlock, err := a.deps.LockThread(ctx, threadID)
	if err != nil {
		return fmt.Errorf("app: reconcile codex: lock thread %s: %w", threadID, err)
	}
	defer unlock()
	if current, ok := a.session(threadID); !ok || current != sess {
		return nil
	}
	return act()
}

// ReconcileResult is the caller-facing summary of a single reconcile
// pass. The fields are structured so tests can introspect the outcome
// without parsing store rows after the fact.
type ReconcileResult struct {
	ThreadID    string                 `json:"threadId"`
	Status      codex.ThreadStatusKind `json:"status"`
	Running     int                    `json:"running"`     // count of live background launches found
	Flipped     int                    `json:"flipped"`     // count we transitioned to errored/lost
	NeedsResume bool                   `json:"needsResume"` // true when status=notLoaded
}

// ReconcileAfterStart performs the bounded post-start probe of the session a
// start registered and rehydrates a provider thread that Codex reports as
// not loaded.
func (a *Service) ReconcileAfterStart(threadID string, started LiveSession) {
	ctx, cancel := context.WithTimeout(a.lifeCtx(), reopenReconcileTimeout)
	defer cancel()
	result, err := a.reconcileSession(ctx, threadID, started)
	if err != nil {
		log.Printf("app: reconcile codex on reopen for %s: %v", threadID, err)
		return
	}
	if !result.NeedsResume {
		return
	}
	if err := a.whileThreadSession(ctx, threadID, started, func() error { return started.Session.Resume(ctx) }); err != nil {
		log.Printf("app: reconcile codex resume for %s: %v", threadID, err)
	}
}
