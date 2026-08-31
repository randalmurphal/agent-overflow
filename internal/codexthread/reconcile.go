package codexthread

import (
	"context"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/codexghost"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider/codex"
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
	for _, item := range retired {
		a.emit(eventchan.ProviderItemEvent, triage.NewItemStreamUpsert(item))
	}
	return nil
}

func (a *Service) RecoverBackgroundRuntimeOnStartup() {
	if a == nil || a.store == nil {
		return
	}
	retired, err := a.store.RecoverCodexBackgroundRuntime(codexghost.GhostSummary, time.Now().UnixMilli())
	if err != nil {
		log.Printf("app: recover Codex background runtime: %v", err)
		return
	}
	if len(retired) > 0 {
		log.Printf("app: retired %d Codex background items from the prior app instance", len(retired))
	}
	for _, item := range retired {
		a.emit(eventchan.ProviderItemEvent, triage.NewItemStreamUpsert(item))
	}
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
	if sess.Session == nil {
		return ReconcileResult{}, fmt.Errorf("app: reconcile codex: thread %s is not a Codex thread", threadID)
	}

	probe, err := sess.Session.Probe(ctx)
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("app: reconcile codex probe: %w", err)
	}

	runningBg, err := a.store.ListRunningBackgroundToolCalls(threadID)
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("app: reconcile codex list running: %w", err)
	}

	result := ReconcileResult{
		ThreadID: threadID,
		Status:   probe.Status,
		Running:  len(runningBg),
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
		flipped, flipErr := a.retireCodexBackgroundRuntimeForReconcile(threadID)
		if flipErr != nil {
			return result, flipErr
		}
		result.Flipped = flipped
		return result, nil

	default:
		log.Printf("app: reconcile codex: unknown thread status %q; treating as systemError", probe.Status)
		flipped, flipErr := a.retireCodexBackgroundRuntimeForReconcile(threadID)
		if flipErr != nil {
			return result, flipErr
		}
		result.Flipped = flipped
		return result, nil
	}
}

func (a *Service) retireCodexBackgroundRuntimeForReconcile(threadID string) (int, error) {
	retired, err := a.store.RetireCodexBackgroundRuntime(threadID, codexghost.GhostSummary, time.Now().UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("app: reconcile Codex runtime retirement: %w", err)
	}
	flipped := 0
	for _, item := range retired {
		a.emit(eventchan.ProviderItemEvent, triage.NewItemStreamUpsert(item))
		if item.Status == "errored" {
			flipped++
		}
	}
	return flipped, nil
}

// ReconcileResult is the caller-facing summary of a single reconcile
// pass. The fields are structured so tests can introspect the outcome
// without parsing store rows after the fact.
type ReconcileResult struct {
	ThreadID    string                 `json:"threadId"`
	Status      codex.ThreadStatusKind `json:"status"`
	Running     int                    `json:"running"`     // count of running background rows found
	Flipped     int                    `json:"flipped"`     // count we transitioned to errored/lost
	NeedsResume bool                   `json:"needsResume"` // true when status=notLoaded
}

// ReconcileAfterStart performs the bounded post-start probe and rehydrates a
// provider thread that Codex reports as not loaded.
func (a *Service) ReconcileAfterStart(threadID string) {
	ctx, cancel := context.WithTimeout(a.lifeCtx(), reopenReconcileTimeout)
	defer cancel()
	result, err := a.ReconcileOnReopen(ctx, threadID)
	if err != nil {
		log.Printf("app: reconcile codex on reopen for %s: %v", threadID, err)
		return
	}
	if !result.NeedsResume {
		return
	}
	sess, ok := a.session(threadID)
	if !ok || sess.Session == nil {
		return
	}
	if err := sess.Session.Resume(ctx); err != nil {
		log.Printf("app: reconcile codex resume for %s: %v", threadID, err)
	}
}
