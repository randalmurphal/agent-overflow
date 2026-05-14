package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/codexghost"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// flipCodexGhostBackgroundRowsOnStart runs unconditionally on every
// Codex session start (new OR resume) to flip every persisted
// `is_background=1 AND status='running' AND kind='tool_call'` row for
// threadID to `status='errored'`, `decision='lost'`, with " — session
// ended" suffixed on the summary.
//
// Rationale: Phase 2's projector stamps `is_background=true` on
// unifiedExec startups and spawn_agent rows that outlive their launching
// turn. When the Codex subprocess dies between app sessions, its PTYs
// and spawned child threads die with it — a fresh subprocess cannot
// reach any of them. The persisted row is therefore a ghost from the
// app's perspective; flipping it on the next session start gives the
// timeline a clean, accurate state before the user sends the next turn.
//
// Distinction from ReconcileCodexOnReopen: the probe-based reconciler
// runs after the session spawns and is gated on status verdicts
// (notLoaded → resume, systemError → flip re-resurrected rows). That
// path is too narrow for the common case — a subprocess that crashed
// cleanly between app sessions never reports systemError from a new
// spawn, yet its PTYs are still dead. Phase 4 runs before the probe
// ever fires, with no status condition — the subprocess-identity
// change alone is the signal.
//
// Ordering: this MUST run BEFORE the Codex subprocess is spawned
// (before spawnProviderSession) so the flip lands before any replay
// events can re-upsert the same rows. The store flip is DB-only, no
// session needed. On the warm-reconnect case (a surviving subprocess
// re-emitting `item/started` for a still-running item), the existing
// parser dedup + triage UpsertItem path will re-upsert those rows back
// to `status='running'` — the correct behaviour for an item that
// actually is still running.
//
// Best-effort: a store error is logged because session startup must not
// block on cleanup noise. The session still starts; any remaining
// ghost rows get retried on the next restart.
//
// Claude does NOT use this path: its `stop_task` primitive and natural
// completion events settle backgrounded items; a Claude subprocess dying
// is handled by the existing error-stream plumbing. This method is
// Codex-only by caller scope.
func (a *App) flipCodexGhostBackgroundRowsOnStart(threadID string) {
	if a.store == nil {
		return
	}
	flipped, err := a.store.FlipGhostBackgroundRowsOnStart(threadID, codexghost.GhostSummary, time.Now().UnixMilli())
	if err != nil {
		log.Printf("app: flip codex ghost rows for %s: %v", threadID, err)
		return
	}
	for _, item := range flipped {
		a.emit("provider:item_event", triage.NewItemStreamUpsert(item))
	}
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
	if a.shuttingDown.Load() {
		return ReconcileResult{}, ErrShuttingDown
	}

	sess, ok := a.sessionManager().get(threadID)
	if !ok {
		return ReconcileResult{}, fmt.Errorf("app: reconcile codex: no active session for thread %s", threadID)
	}
	if sess.codex == nil {
		return ReconcileResult{}, fmt.Errorf("app: reconcile codex: thread %s is not a Codex thread", threadID)
	}

	probe, err := sess.codex.Probe(ctx)
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
		// Session reports terminal failure. Phase 4's pre-spawn flip
		// already covered most ghost rows; anything still in
		// `runningBg` here is either a warm-reconnect resurrection that
		// the subprocess has since failed on, or a row inserted
		// post-spawn that immediately became unreachable. Either way,
		// flip via the shared helper so the summary-suffix stays
		// idempotent.
		flipped, flipErr := a.flipRunningBgRowsAsSessionEnded(runningBg)
		if flipErr != nil {
			return result, flipErr
		}
		result.Flipped = flipped
		return result, nil

	default:
		log.Printf("app: reconcile codex: unknown thread status %q; treating as systemError", probe.Status)
		flipped, flipErr := a.flipRunningBgRowsAsSessionEnded(runningBg)
		if flipErr != nil {
			return result, flipErr
		}
		result.Flipped = flipped
		return result, nil
	}
}

// flipRunningBgRowsAsSessionEnded updates the supplied rows in place,
// applying the same status / decision / summary as Phase 4's pre-spawn
// ghost flip. Shared between the Phase 4 entry path (the store-level
// bulk flip) and ReconcileCodexOnReopen's post-probe systemError branch
// so the suffix convention stays idempotent across both.
//
// Returns the count of rows actually mutated (non-zero only on the rare
// warm-reconnect+systemError branch, since Phase 4's pre-spawn flip has
// already converted every ghost row on the common path).
func (a *App) flipRunningBgRowsAsSessionEnded(items []store.Item) (int, error) {
	now := time.Now().UnixMilli()
	var flipped int
	for _, item := range items {
		item.Status = "errored"
		item.Summary = codexghost.GhostSummary(item.Summary)
		item.Decision = "lost"
		item.UpdatedAt = now
		if _, err := a.store.UpsertItem(item, nil); err != nil {
			return flipped, fmt.Errorf("app: reconcile codex flip %s: %w", item.ID, err)
		}
		flipped++
	}
	return flipped, nil
}

// ReconcileResult is the caller-facing summary of a single reconcile
// pass. The fields are structured for both the eventual binding
// surface (we'll want these in bindings/ once we wire the frontend)
// and the tests that need to introspect the outcome without parsing
// store rows after the fact.
type ReconcileResult struct {
	ThreadID    string                 `json:"threadId"`
	Status      codex.ThreadStatusKind `json:"status"`
	Running     int                    `json:"running"`     // count of running background rows found
	Flipped     int                    `json:"flipped"`     // count we transitioned to errored/lost
	NeedsResume bool                   `json:"needsResume"` // true when status=notLoaded
}
