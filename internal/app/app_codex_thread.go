package app

import (
	"context"

	"agent-overflow/internal/codexthread"
	"agent-overflow/internal/provider/codex"
)

func (a *App) codexThreadService() *codexthread.Service {
	a.codexThreadOnce.Do(func() {
		a.codexThread = codexthread.New(codexthread.Deps{
			Context:        a.lifeCtx,
			IsShuttingDown: a.shuttingDown.Load,
			ShutdownError:  ErrShuttingDown,
			Store:          a.store,
			Emit:           a.emit,
			Session: func(threadID string) (codexthread.LiveSession, bool) {
				sess, ok := a.sessionManager().get(threadID)
				if !ok {
					return codexthread.LiveSession{}, false
				}
				return codexthread.LiveSession{Token: sess.Token, Session: sess.Codex}, true
			},
		})
	})
	return a.codexThread
}

// ReconcileCodexOnReopen probes a Codex thread's liveness via
// `thread/read` after the session has spawned and returns a classified
// result the caller uses to sequence follow-up work:
//
//   - idle / active   → session is alive. No flip needed here — Phase 4's
//     pre-spawn ghost flip already handled any dead-
//     subprocess rows, and live completions will
//     arrive over the wire.
//   - notLoaded       → call `thread/resume` to rehydrate. We return a
//     NeedsResume hint so the caller can sequence it.
//   - systemError     → the warm-reconnect rarity: Phase 4 flipped
//     ghost rows before spawn, then the replay
//     re-upserted some back to running (warm
//     reconnect), and the subprocess has since died.
//     Flip those re-resurrected rows via the same
//     helper Phase 4 uses so the summary suffix stays
//     idempotent. The vast majority of reopens see
//     zero rows here.
//   - unknown kind    → log and fall back to systemError behaviour so a
//     new enum value doesn't silently mask lost work.
//
// The Codex adapter must already be connected in the session manager.
// An error only surfaces transport/database
// failures — a `systemError` verdict is a successful probe.
//
// `//wails:ignore` keeps this off the auto-generated TS bindings: the
// reconcile is triggered internally by reconcileCodexAfterStart (fired
// from startSessionNow on Codex resumes) — the frontend never needs to
// call it directly.
//
//wails:ignore
func (a *App) ReconcileCodexOnReopen(ctx context.Context, threadID string) (ReconcileResult, error) {
	result, err := a.codexThreadService().ReconcileOnReopen(ctx, threadID)
	return ReconcileResult{
		ThreadID:    result.ThreadID,
		Status:      result.Status,
		Running:     result.Running,
		Flipped:     result.Flipped,
		NeedsResume: result.NeedsResume,
	}, err
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

func (a *App) retireCodexBackgroundRuntime(threadID string) error {
	return a.codexThreadService().RetireBackgroundRuntime(threadID)
}

func (a *App) recoverCodexBackgroundRuntimeOnStartup() {
	a.codexThreadService().RecoverBackgroundRuntimeOnStartup()
}

func (a *App) reconcileCodexAfterStart(threadID string) {
	a.codexThreadService().ReconcileAfterStart(threadID)
}

func (a *App) noteCodexThreadCost(threadID, sessionToken string) {
	a.codexThreadService().NoteThreadCost(threadID, sessionToken)
}

func (a *App) forgetCodexThreadCost(threadID string) {
	a.codexThreadService().ForgetThreadCost(threadID)
}
