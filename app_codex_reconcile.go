package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/provider/codex"
)

// ReconcileCodexOnReopen probes a Codex thread's liveness via
// `thread/read` and marks every still-`running && is_background`
// tool_call row with the correct disposition:
//
//   - idle / active   → keep `running` (session is alive; real completion
//                       will arrive over the wire if it lands)
//   - notLoaded       → call `thread/resume` to rehydrate (handled by
//                       startSessionNow which the session bootstrap
//                       already routes through when ResumeThreadID is
//                       populated). We return hinting "rehydrate" so
//                       callers can sequence a resume.
//   - systemError     → flip every running background tool_call to
//                       status=errored, decision=lost. Matches the
//                       spec's "Approval lost on restart" vocabulary
//                       so the timeline renders consistently.
//
// The Codex adapter must already be connected (i.e., Session is in
// `a.sessions[threadID]`) — reconciliation runs against a live session,
// not a cold restart. On a fully cold app launch, bootstrap first
// starts/resumes the Codex session (which seeds the adapter with the
// provider-side thread id) and can then call this to triage any
// lingering running background rows.
//
// Return contract: a single ReconcileResult summarising the probe and
// any flips. An error only surfaces transport/database failures — a
// `systemError` verdict is a successful probe.
//
// This path is the minimum viable wiring called out by the chat-rewrite
// spec §"On app reopen" — it will grow as the broader crash-recovery
// flow lands.
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

	a.mu.Lock()
	sess, ok := a.sessions[threadID]
	a.mu.Unlock()
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
		// Session alive; the real completion will arrive over the wire
		// (if/when it lands). Nothing to flip.
		return result, nil

	case codex.ThreadStatusNotLoaded:
		// `notLoaded` doesn't mean dead — the thread just isn't in
		// memory. Resume would rehydrate. We don't call resume here
		// because startSessionNow already orchestrates that path for
		// Codex; this method's contract is probe + classify, not
		// session lifecycle. The caller uses `NeedsResume` to sequence
		// a follow-up startSession.
		result.NeedsResume = true
		return result, nil

	case codex.ThreadStatusSystemError:
		// Session reports a terminal failure. Flip every running
		// background tool_call row to errored/lost.
		now := time.Now().UnixMilli()
		for _, item := range runningBg {
			item.Status = "errored"
			if item.Summary == "" {
				item.Summary = "Interrupted — session ended"
			} else {
				item.Summary = item.Summary + " — interrupted"
			}
			item.Decision = "lost"
			item.UpdatedAt = now
			if _, err := a.store.UpsertItem(item, nil); err != nil {
				return result, fmt.Errorf("app: reconcile codex flip %s: %w", item.ID, err)
			}
			result.Flipped++
		}
		return result, nil

	default:
		// Unknown status kind from the wire — log it and fall back to
		// the conservative "treat as systemError" path so a new
		// provider enum value doesn't silently hide lost work.
		log.Printf("app: reconcile codex: unknown thread status %q; treating as systemError", probe.Status)
		now := time.Now().UnixMilli()
		for _, item := range runningBg {
			item.Status = "errored"
			item.Summary = item.Summary + " — interrupted (unknown session status)"
			item.Decision = "lost"
			item.UpdatedAt = now
			if _, err := a.store.UpsertItem(item, nil); err != nil {
				return result, fmt.Errorf("app: reconcile codex flip %s: %w", item.ID, err)
			}
			result.Flipped++
		}
		return result, nil
	}
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

