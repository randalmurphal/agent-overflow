// Cold-load instrumentation for thread switches: one consolidated
// dev-trace record per pane per cold-load, segmenting where the time
// went (fetch vs settle) and why the warm-up gate finally opened.
//
// This is measurement plumbing ONLY — it never reads or writes scroll
// state, and it has no opinion on when the warm-up gate should open
// (that stays entirely in utils/scroll/observers.ts). It just watches
// three call sites (`thread.svelte.ts`'s switchThread/runParallelLoad,
// and MessageTimeline's warm-edge $effect) and folds them into one
// `timeline.coldload` record per pane per switch.
//
// Session lifecycle, per pane:
//   coldLoadSwitchStart  — opens (or overwrites) the pane's session.
//   coldLoadItemsApplied — records when/how-many for the fetch leg.
//   coldLoadWarmEdge     — detects the warm false→true rising edge
//                          itself; on match, emits and closes the
//                          session; on thread-id mismatch, drops it
//                          silently (stale — the pane switched again
//                          before warming).
//
// A session that never sees a matching rising edge (the user switched
// away before the timeline warmed, or the pane never mounts a warm
// consumer at all — Discussion's ChannelView) is simply overwritten or
// abandoned; there is no timeout or cleanup pass because module-scoped
// state here is O(mounted panes), not O(history).
import { isUiRenderTraceEnabled, recordUiTrace } from './uiRenderTrace';

export type ColdLoadSource = 'cache-restore' | 'fetch';

/**
 * Structural mirror of timelineSizePriors.svelte.ts's
 * SizePriorsReplayStats — declared locally (loose strings) so this utils
 * module never imports from components/. The chat layer's stats object
 * satisfies it as-is.
 */
export interface ColdLoadPriorsStats {
  source: string;
  validity: string;
  rowsResolved: number;
}

interface ColdLoadSession {
  threadId: string;
  source: ColdLoadSource;
  switchStartAt: number;
  itemsAppliedAt: number | null;
  itemCount: number | null;
  priors: ColdLoadPriorsStats | null;
}

// Keyed by paneId. One in-flight session per pane — a pane can only be
// mid-switch for one thread at a time.
const sessionsByPane = new Map<string, ColdLoadSession>();
// Keyed by paneId. Tracks the last `isWarm` value observed per pane so
// `coldLoadWarmEdge` can detect the false→true transition itself,
// independent of whether a session happens to be open — a pane that
// warmed once and stays warm across unrelated re-renders must not
// re-fire on every call.
const lastWarmByPane = new Map<string, boolean>();

/** Open (or silently overwrite) the pane's cold-load session. An
 * overwritten session means the user switched away before it warmed —
 * that's not a useful sample, so the abandoned session is simply
 * discarded, not emitted. */
export function coldLoadSwitchStart(
  paneId: string,
  threadId: string,
  source: ColdLoadSource,
): void {
  if (!isUiRenderTraceEnabled()) return;
  sessionsByPane.set(paneId, {
    threadId,
    source,
    switchStartAt: performance.now(),
    itemsAppliedAt: null,
    itemCount: null,
    priors: null,
  });
}

/** Attach the size-priors replay summary to the pane's open session.
 * Called from MessageTimeline's warm-edge $effect on every run (cheap
 * no-op without a session); the last write before the warm edge is what
 * the record carries. */
export function coldLoadPriors(paneId: string, stats: ColdLoadPriorsStats): void {
  if (!isUiRenderTraceEnabled()) return;
  const session = sessionsByPane.get(paneId);
  if (!session) return;
  session.priors = stats;
}

/** Mark the fetch leg's initial-slice application. No-op if no session
 * is open for the pane (trace was disabled at switchStart, or the
 * session was already overwritten/closed). */
export function coldLoadItemsApplied(paneId: string, itemCount: number): void {
  if (!isUiRenderTraceEnabled()) return;
  const session = sessionsByPane.get(paneId);
  if (!session) return;
  session.itemsAppliedAt = performance.now();
  session.itemCount = itemCount;
}

/** Report the controller's current isWarm/warmReason for a pane. Only
 * the false→true rising edge does anything; every other call (already
 * warm, still warming) is a no-op read. On a rising edge with a session
 * open for a MATCHING threadId, emits exactly one `timeline.coldload`
 * record and closes the session. A mismatched threadId means the pane
 * switched again before warming — the stale session is dropped without
 * emitting. */
export function coldLoadWarmEdge(
  paneId: string,
  threadId: string,
  isWarm: boolean,
  reason: string | null,
): void {
  if (!isUiRenderTraceEnabled()) return;
  const wasWarm = lastWarmByPane.get(paneId) ?? false;
  lastWarmByPane.set(paneId, isWarm);
  if (!isWarm || wasWarm) return;

  const session = sessionsByPane.get(paneId);
  if (!session) return;
  sessionsByPane.delete(paneId);
  if (session.threadId !== threadId) return;

  const now = performance.now();
  const fetchMs =
    session.source === 'fetch' && session.itemsAppliedAt !== null
      ? Math.round(session.itemsAppliedAt - session.switchStartAt)
      : null;
  const settleBase = session.itemsAppliedAt ?? session.switchStartAt;

  recordUiTrace('timeline.coldload', {
    paneId,
    threadId,
    source: session.source,
    fetchMs,
    itemCount: session.itemCount,
    settleMs: Math.round(now - settleBase),
    totalMs: Math.round(now - session.switchStartAt),
    warmReason: reason,
    priors: session.priors,
  });
}

/** Test-only reset — clears all pane sessions and warm baselines. */
export function __resetColdLoadTraceForTest(): void {
  sessionsByPane.clear();
  lastWarmByPane.clear();
}
