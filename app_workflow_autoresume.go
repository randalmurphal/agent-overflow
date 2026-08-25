package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/workflow/engine"

	"agent-overflow/internal/workflowhost"
)

// Auto-resume is the app-side half of an explicitly scheduled resume: the
// engine holds no timers by boundary, so the moment a parked run comes back
// lives here, and the only durable record is `work_items.auto_resume_at`
// (store v54). Provider usage-limit parks do not arm it (D75).
//
// The column is the single source of truth and the timer registry is armed from
// it, in both directions:
//
//   - ARM writes the column and installs the timer together
//     (`setWorkflowAutoResume`), so a restart re-arms what it finds
//     (`sweepWorkflowAutoResumes`) instead of losing a five-day stall to a
//     process lifetime.
//   - CLEAR is every transition OUT of the park (`clearWorkflowAutoResume`,
//     driven from the one state listener): a manual resume, a cancel, a
//     discard, a rerun, and the fire's own resume all land there. An opt-out
//     therefore clears what the opt-in stored, rather than leaving a timer to
//     fire into work somebody already repaired.
//
// The fire re-reads the run before acting, because the two halves are not one
// transaction: a clear that raced a firing timer must not resume a repaired run.

// workflowAutoResumeBootDelay is how long after startup a run whose moment has
// already passed waits before resuming. It is not politeness: the engine's
// crash rebuild runs at boot and parks every interrupted run, so firing into
// that window would race a resume against the rebuild that is still deciding
// what the run is.
const workflowAutoResumeBootDelay = 30 * time.Second

// maxWorkflowResumeDelay bounds `run resume --at`. A schedule further out than
// this is not a paced resume, it is a run somebody meant to cancel, and a timer
// nothing will ever fire is worse than a refusal that says so.
const maxWorkflowResumeDelay = 30 * 24 * time.Hour

// workflowAutoResumeRetryDelay is how long a fire waits before trying again
// after the resume itself failed.
//
// A failure here is transient by construction: the fire has already re-read the
// run and confirmed it is still the park this schedule was written for, so what
// is left is the engine being briefly unavailable, a worktree that is not back
// yet, or a store error. Without a retry the timer is spent, the registration is
// dead, and the column only gets another chance at the next boot — days away on
// a desktop app, which is the exact stall this whole mechanism exists to end.
// The column is deliberately left ARMED across the retry, so a restart in the
// meantime re-arms it too, and each attempt re-checks resumability — a run that
// has genuinely moved on clears itself on the next fire instead of looping.
const workflowAutoResumeRetryDelay = 5 * time.Minute

func (a *App) workflowAutoResumeTimer(delay time.Duration, fire func()) workflowhost.Timer {
	if a.workflowAutoResume.newTimer != nil {
		return a.workflowAutoResume.newTimer(delay, fire)
	}
	return time.AfterFunc(delay, fire)
}

// workflowAutoResumeNow is the clock every schedule is measured against.
// Production leaves the injection nil and reads `time.Now`, mirroring
// `idleReaperNowFn` / `retentionNowFn`.
func (a *App) workflowAutoResumeNow() time.Time {
	if a.workflowAutoResume.nowFn != nil {
		return a.workflowAutoResume.nowFn()
	}
	return time.Now()
}

// setWorkflowAutoResume persists the explicitly requested moment this parked
// run resumes and arms the timer that does it. The write comes first: a timer
// armed over a column that was not written would not survive a restart, which
// is the exact failure the column exists to prevent.
func (a *App) setWorkflowAutoResume(itemID string, at time.Time) error {
	if err := a.store.SetWorkItemAutoResumeAt(itemID, at.UnixMilli()); err != nil {
		return err
	}
	a.armWorkflowAutoResume(itemID, at.Sub(a.workflowAutoResumeNow()))
	return nil
}

// armWorkflowAutoResume installs (or replaces) one run's timer. A delay in the
// past fires as soon as the runtime will let it, which is what a moment that
// elapsed while the app was closed asks for.
func (a *App) armWorkflowAutoResume(itemID string, delay time.Duration) {
	if delay < 0 {
		delay = 0
	}
	a.workflowAutoResume.mu.Lock()
	if existing, ok := a.workflowAutoResume.timers[itemID]; ok {
		existing.Stop()
	}
	if a.workflowAutoResume.timers == nil {
		a.workflowAutoResume.timers = make(map[string]workflowhost.Timer)
	}
	a.workflowAutoResume.timers[itemID] = a.workflowAutoResumeTimer(delay, func() {
		a.fireWorkflowAutoResume(itemID)
	})
	a.workflowAutoResume.mu.Unlock()
}

// clearWorkflowAutoResume disarms and forgets one run's self-resume. It is
// called from the state listener for every transition that takes a run out of
// the park it was waiting through.
//
// Nothing armed means nothing to clear, and the store is deliberately not
// touched in that case: it is the answer for every run that ever parks, and the
// listener runs on the engine's own emit path. The registration is therefore
// what makes a clear reach the column at all, which is why the fire keeps its
// own registration across the resume it performs (see `fireWorkflowAutoResume`)
// — a run whose column is set with nothing armed in THIS process is one the
// boot sweep will re-arm, and the fire re-checks the run before acting.
func (a *App) clearWorkflowAutoResume(itemID string) {
	a.workflowAutoResume.mu.Lock()
	timer, armed := a.workflowAutoResume.timers[itemID]
	if armed {
		timer.Stop()
		delete(a.workflowAutoResume.timers, itemID)
	}
	a.workflowAutoResume.mu.Unlock()
	if !armed {
		return
	}
	if err := a.store.SetWorkItemAutoResumeAt(itemID, 0); err != nil {
		log.Printf("workflow auto-resume %s: clear schedule: %v", itemID, err)
	}
}

// fireWorkflowAutoResume is the timer's callback: the bare resume explicitly
// requested by `run resume --at`, preserving whichever continuable attempt the
// schedule was armed on.
//
// It re-reads the run rather than trusting the timer, because the arm and the
// clear are two writes: a run repaired between them has already been resumed,
// and a run that moved on to another park is not the one this schedule was for.
//
// It deliberately KEEPS its registry entry across the resume. The registration
// is what lets the run's own transition to `running` clear the column through
// the one hook every repair shares; dropping it here would leave the column set
// on the very run this schedule succeeded for, and every later boot would
// re-arm a schedule that had already been spent. The entry's timer has already
// fired, so holding it costs nothing and cannot fire twice — the next arm
// replaces it and the next transition removes it.
//
// A resume that FAILS leaves the schedule standing and re-arms at
// `workflowAutoResumeRetryDelay`, which is the direction that cannot strand a
// run. Nothing else here retries: a run that is no longer resumable is cleared,
// and a store that will not answer is left for the next boot.
func (a *App) fireWorkflowAutoResume(itemID string) {
	if a.shuttingDown.Load() {
		return
	}
	at, err := a.store.WorkItemAutoResumeAt(itemID)
	if err != nil {
		log.Printf("workflow auto-resume %s: read schedule: %v", itemID, err)
		return
	}
	if at == 0 {
		return
	}
	item, err := a.store.GetWorkItemSummary(itemID)
	if err != nil {
		log.Printf("workflow auto-resume %s: load run: %v", itemID, err)
		return
	}
	if !workflowAutoResumable(item.State, item.Reason) {
		// The run is no longer the park this schedule was written for — somebody
		// repaired it, or it moved on to a park a bare resume does not continue.
		// Clearing is the honest end of it, through the same pair the state
		// listener uses: leaving the column set would re-arm the same dead
		// schedule on every restart from here on.
		a.clearWorkflowAutoResume(itemID)
		return
	}
	// The resume itself transitions the run to `running`, which is what clears
	// the column through the state listener. Nothing is cleared here on success.
	if err := a.WorkflowResumeItem(a.lifeCtx(), itemID, "", false); err != nil {
		log.Printf("workflow auto-resume %s: resume (retrying in %s): %v", itemID, workflowAutoResumeRetryDelay, err)
		a.armWorkflowAutoResume(itemID, workflowAutoResumeRetryDelay)
	}
}

// sweepWorkflowAutoResumes re-arms every persisted schedule at startup. It runs
// after the engine has started — its rebuild is what decides which runs are
// parked at all — and a moment that already passed is armed a short way out
// rather than fired immediately, so the resume lands after the rebuild settles.
func (a *App) sweepWorkflowAutoResumes() {
	resumes, err := a.store.ListWorkItemAutoResumes()
	if err != nil {
		log.Printf("workflow auto-resume sweep: %v", err)
		return
	}
	now := a.workflowAutoResumeNow()
	for _, resume := range resumes {
		delay := time.UnixMilli(resume.At).Sub(now)
		if delay < workflowAutoResumeBootDelay {
			delay = workflowAutoResumeBootDelay
		}
		a.armWorkflowAutoResume(resume.ItemID, delay)
	}
}

// stopWorkflowAutoResumes disarms every timer on the way down. The column
// survives, so the next boot re-arms exactly what this leaves behind.
func (a *App) stopWorkflowAutoResumes() {
	a.workflowAutoResume.mu.Lock()
	for itemID, timer := range a.workflowAutoResume.timers {
		timer.Stop()
		delete(a.workflowAutoResume.timers, itemID)
	}
	a.workflowAutoResume.mu.Unlock()
}

// workflowAutoResumable reports whether a run is in the state a scheduled bare
// resume was written for: parked, on a reason a bare resume continues.
func workflowAutoResumable(state, reason string) bool {
	return engine.State(state) == engine.StateNeedsHuman &&
		engine.ContinuableReason(engine.Reason(reason))
}

// WorkflowScheduleResume arms a parked run's resume at an explicit time. This
// is always an operator-authored schedule; provider usage limits never create
// one automatically. It resumes nothing now: the run stays exactly where it is
// until the moment arrives, and every action that repairs it in the meantime
// disarms the schedule.
//
// `at` is either an RFC 3339 timestamp or a leading-`+` duration relative to the
// APP's clock (`+36h`), which is the clock the timer will actually run on.
//
// It returns the armed moment in RFC 3339 so the caller prints the time the app
// holds rather than re-deriving one from a duration it sent.
func (a *App) WorkflowScheduleResume(ctx context.Context, itemID, at string) (string, error) {
	// The engine is required even though nothing resumes now: what this arms is
	// a `WorkflowResumeItem`, and an app with no engine would take the schedule,
	// persist it, and fail the resume on this boot and every boot after. Its
	// siblings refuse for the same reason, at the same point.
	if _, err := a.requireWorkflowEngine(); err != nil {
		return "", err
	}
	if err := a.authorizeScopedRunAction(ctx, itemID, "schedule workflow resume"); err != nil {
		return "", err
	}
	now := a.workflowAutoResumeNow()
	resumeAt, err := parseWorkflowResumeAt(at, now)
	if err != nil {
		return "", err
	}
	item, err := a.store.GetWorkItemSummary(itemID)
	if err != nil {
		return "", err
	}
	if !workflowAutoResumable(item.State, item.Reason) {
		return "", fmt.Errorf(
			"schedule workflow resume %s: run is %s(%s); a scheduled resume continues a parked attempt, which applies to %s",
			itemID, item.State, item.Reason, workflowContinuableReasonList(),
		)
	}
	if err := a.setWorkflowAutoResume(itemID, resumeAt); err != nil {
		return "", err
	}
	return resumeAt.Local().Format(time.RFC3339), nil
}

// parseWorkflowResumeAt accepts the two forms `run resume --at` documents. Both
// are resolved here rather than in the CLI so a relative one is measured against
// the clock the timer runs on, and so one refusal answers both shapes.
func parseWorkflowResumeAt(value string, now time.Time) (time.Time, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, fmt.Errorf("schedule workflow resume: a time is required, as RFC 3339 (2026-08-15T19:56:00Z) or a duration (+36h)")
	}
	var resumeAt time.Time
	if strings.HasPrefix(trimmed, "+") {
		delay, err := time.ParseDuration(strings.TrimPrefix(trimmed, "+"))
		if err != nil {
			return time.Time{}, fmt.Errorf("schedule workflow resume: %q is not a duration: %w", trimmed, err)
		}
		if delay <= 0 {
			return time.Time{}, fmt.Errorf("schedule workflow resume: %q is not in the future", trimmed)
		}
		resumeAt = now.Add(delay)
	} else {
		parsed, err := time.Parse(time.RFC3339, trimmed)
		if err != nil {
			return time.Time{}, fmt.Errorf("schedule workflow resume: %q is neither RFC 3339 nor a duration like +36h: %w", trimmed, err)
		}
		resumeAt = parsed
	}
	if !resumeAt.After(now) {
		return time.Time{}, fmt.Errorf("schedule workflow resume: %s is not in the future", resumeAt.Local().Format(time.RFC3339))
	}
	if resumeAt.Sub(now) > maxWorkflowResumeDelay {
		return time.Time{}, fmt.Errorf(
			"schedule workflow resume: %s is more than %s away; cancel the run instead of parking it that long",
			resumeAt.Local().Format(time.RFC3339), maxWorkflowResumeDelay,
		)
	}
	return resumeAt, nil
}

// workflowContinuableReasonList renders the engine's own membership, so this
// refusal cannot fall behind a reason the engine later admits.
func workflowContinuableReasonList() string {
	reasons := engine.ContinuableReasons()
	names := make([]string, len(reasons))
	for index, reason := range reasons {
		names[index] = string(reason)
	}
	return strings.Join(names, ", ")
}
