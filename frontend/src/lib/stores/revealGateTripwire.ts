import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';

/**
 * Diagnostic-only tripwire over the reveal gate's one hand-maintained
 * invariant.
 *
 * `threadStreamingReveal.svelte.ts` withholds every top-level row after
 * the one currently revealing, and there is deliberately NO reactive
 * `$effect` watching `items` to keep that boundary fresh (a parallel
 * watcher over the timeline is banned — frontend/AGENTS.md). The gate is
 * instead kept in sync by explicit `recomputeReveal()` calls from every
 * path that can change which rows exist relative to a live smoother. A
 * path that forgets one does not throw and does not log: it silently
 * pins the timeline at a stale boundary, and rows that should have
 * mounted never do.
 *
 * This watches for exactly that. The pane's two items-commit chokepoints
 * arm a dirty flag; `recomputeReveal` (and `disposeAll`, which drops the
 * boundary outright) clear it; a microtask queued behind the commit
 * reports whatever is still dirty. Same task ⇒ clean, which is what
 * every correct caller does today — the recompute follows its commit
 * synchronously.
 *
 * It OBSERVES and nothing else. It must never call `recomputeReveal`
 * itself: doing so would hoist a recompute into the commit chokepoints
 * and rush the readable drain the queue exists to protect. A finding
 * here is a bug to fix at the offending call site, not something to
 * paper over from the commit path.
 *
 * Armed only while the gate is actually engaged (a non-null
 * `revealBoundary`). With nothing withheld a missed recompute has no
 * observable consequence — no smoother can be left paused, because
 * pausing requires a frontier — and reporting it would bury the real
 * finding in noise.
 */

/** Which pane chokepoint committed the items that armed the flag. */
export type ItemsCommitSite = 'commitTimelineItems' | 'commitUpsertResult';

export interface RevealGateTripwireOptions {
  /** Thread the pane is showing, for the report; may be null. */
  getThreadId(): string | null;
  /** True while the reveal gate is withholding rows (boundary non-null). */
  isRevealGateEngaged(): boolean;
}

export interface RevealGateTripwire {
  /** Called from an items-commit chokepoint, AFTER the write. */
  noteItemsCommitted(site: ItemsCommitSite): void;
  /** Called from `recomputeReveal` / `disposeAll` — the gate is fresh again. */
  noteRevealSynced(): void;
}

/**
 * Per-pane report budget. A structurally missing recompute repeats on
 * every batch of a streaming turn; the first few carry the whole signal
 * and the rest would burn the ui-trace rotation budget.
 */
const MAX_REPORTS_PER_PANE = 5;

export function createRevealGateTripwire(
  options: RevealGateTripwireOptions,
): RevealGateTripwire {
  let pendingSite: ItemsCommitSite | null = null;
  let pendingThreadId: string | null = null;
  let drainScheduled = false;
  let reported = 0;

  function drain(): void {
    drainScheduled = false;
    const site = pendingSite;
    const threadId = pendingThreadId;
    pendingSite = null;
    pendingThreadId = null;
    if (site === null) return;
    if (reported >= MAX_REPORTS_PER_PANE) return;
    reported += 1;
    const detail = `thread=${threadId ?? 'none'} site=${site} occurrence=${reported}`;
    // Paired console line: `ReportFrontendErrorBatch` is LocalOnly, so a
    // remote session cannot persist the record and this is the only
    // surviving evidence there.
    console.warn(
      `[revealGate] items committed with no reveal recompute (${detail})`,
    );
    // Constant message — every variable rides in `detail`, or the dedupe
    // signature mints a fresh entry per thread and walks past the cap.
    reportFrontendDiagnostic(
      'reveal gate: items commit with no reveal recompute',
      detail,
    );
  }

  return {
    noteItemsCommitted(site: ItemsCommitSite): void {
      if (reported >= MAX_REPORTS_PER_PANE) return;
      if (!options.isRevealGateEngaged()) return;
      pendingSite = site;
      pendingThreadId = options.getThreadId();
      if (drainScheduled) return;
      drainScheduled = true;
      queueMicrotask(drain);
    },
    noteRevealSynced(): void {
      pendingSite = null;
      pendingThreadId = null;
    },
  };
}
