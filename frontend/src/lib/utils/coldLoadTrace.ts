// Cold-load instrumentation for thread switches: one consolidated
// dev-trace record per pane per cold-load, segmenting where the time
// went (fetch vs settle) and why the warm-up gate finally opened.
//
// This is measurement plumbing ONLY — it never reads or writes scroll
// state, and it has no opinion on when the warm-up gate should open
// (that stays entirely in utils/scroll/observers.ts). It just watches
// three call sites (`threadSwitchLoad.svelte.ts`'s switchThread and
// runItemWindowSync, and MessageTimeline's warm-edge $effect) and folds
// them into one `timeline.coldload` record per pane per switch.
//
// Session lifecycle, per pane:
//   coldLoadSwitchStart  — opens the pane's session, emitting any
//                          session still open for it as abandoned.
//   coldLoadItemsApplied — records when/how-many for the fetch leg, and
//                          whether that application re-armed the warm
//                          gate. When it did not AND the gate is already
//                          open, no further rising edge is coming — the
//                          measurement is complete, so it closes here.
//   coldLoadWarmEdge     — detects the warm false→true rising edge
//                          itself. A fetch session's gate opens once
//                          against the EMPTY pane while the slice is in
//                          flight (see PaneScrollController.armWarmup);
//                          that edge measures the empty pane, not the
//                          content, so it is COUNTED and the session is
//                          held for the post-items edge the re-arm
//                          produces. On a thread-id mismatch the session
//                          is emitted as abandoned.
//
// Every close path emits. A session is never silently discarded, so
// "the switch happened but no record came out" is a real signal rather
// than a routine outcome; module-scoped state stays O(mounted panes).
import { isUiRenderTraceEnabled, recordUiTrace } from './uiRenderTrace';

export type ColdLoadSource = 'cache-restore' | 'fetch';

/**
 * Which cache, if any, put rows on screen before the window sync
 * answered: the in-memory LRU ('l1'), the IndexedDB replica ('replica'),
 * or nothing ('none' — the pane waited on the wire). Distinct from
 * `ColdLoadSource`, which records only whether the LRU hit at switch
 * time; the replica paint is decided later, inside the load leg.
 */
export type ColdLoadPaintSource = 'l1' | 'replica' | 'none';

/** Why a session closed without the warm edge it was waiting for. */
export type ColdLoadAbandonReason = 'switched-away' | 'thread-changed';

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
  /** Did the initial-slice application re-close the warm gate? */
  warmupRearmed: boolean;
  /** Warm rising edges observed before the items landed. */
  warmBeforeItems: number;
  priors: ColdLoadPriorsStats | null;
  paintSource: ColdLoadPaintSource;
  /** `SyncThreadWindow` verdict for this open; null if it never landed. */
  syncStatus: string | null;
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

function emitSession(
  paneId: string,
  session: ColdLoadSession,
  close: { warmReason: string | null; abandoned: ColdLoadAbandonReason | null },
): void {
  const now = performance.now();
  const fetchMs =
    session.source === 'fetch' && session.itemsAppliedAt !== null
      ? Math.round(session.itemsAppliedAt - session.switchStartAt)
      : null;
  const settleBase = session.itemsAppliedAt ?? session.switchStartAt;

  recordUiTrace('timeline.coldload', {
    paneId,
    threadId: session.threadId,
    source: session.source,
    fetchMs,
    itemCount: session.itemCount,
    settleMs: Math.round(now - settleBase),
    totalMs: Math.round(now - session.switchStartAt),
    warmReason: close.warmReason,
    warmupRearmed: session.warmupRearmed,
    warmBeforeItems: session.warmBeforeItems,
    abandoned: close.abandoned,
    priors: session.priors,
    paintSource: session.paintSource,
    syncStatus: session.syncStatus,
  });
}

/** Open the pane's cold-load session. A session still open for this pane
 * means the user switched away before it finished — it is emitted with
 * `abandoned: 'switched-away'` rather than dropped, so the switch that
 * outran its own load stays visible in the trace. */
export function coldLoadSwitchStart(
  paneId: string,
  threadId: string,
  source: ColdLoadSource,
): void {
  if (!isUiRenderTraceEnabled()) return;
  const open = sessionsByPane.get(paneId);
  if (open) emitSession(paneId, open, { warmReason: null, abandoned: 'switched-away' });
  sessionsByPane.set(paneId, {
    threadId,
    source,
    switchStartAt: performance.now(),
    itemsAppliedAt: null,
    itemCount: null,
    warmupRearmed: false,
    warmBeforeItems: 0,
    priors: null,
    // An LRU hit paints synchronously inside switchThread, so it is
    // already true here; anything else is decided by the load leg.
    paintSource: source === 'cache-restore' ? 'l1' : 'none',
    syncStatus: null,
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

/** Record which cache painted this open, once the load leg knows. Only
 * an upgrade from 'none' to 'replica' ever needs reporting; the setter
 * accepts every value so the call site stays unconditional. */
export function coldLoadPaintSource(paneId: string, paintSource: ColdLoadPaintSource): void {
  if (!isUiRenderTraceEnabled()) return;
  const session = sessionsByPane.get(paneId);
  if (!session) return;
  session.paintSource = paintSource;
}

/** Record the window-sync verdict ('fresh' / 'stale' / 'rewritten' /
 * 'gone'). Reporting only — it never closes or holds a session, because
 * a `fresh` answer mounts no rows and therefore produces no warm edge of
 * its own. */
export function coldLoadSyncStatus(paneId: string, status: string): void {
  if (!isUiRenderTraceEnabled()) return;
  const session = sessionsByPane.get(paneId);
  if (!session) return;
  session.syncStatus = status;
}

/** Mark the fetch leg's initial-slice application: how many rows the
 * pane holds afterwards, and whether that application re-armed the warm
 * gate (`armInitialSliceWarmup` in thread.svelte.ts). No-op if no
 * session is open for the pane (trace was disabled at switchStart, or
 * the session was already closed). */
export function coldLoadItemsApplied(
  paneId: string,
  itemCount: number,
  warmupRearmed: boolean,
): void {
  if (!isUiRenderTraceEnabled()) return;
  const session = sessionsByPane.get(paneId);
  if (!session) return;
  session.itemsAppliedAt = performance.now();
  session.itemCount = itemCount;
  session.warmupRearmed = warmupRearmed;
  // No re-arm and the gate is already open (an empty slice — nothing
  // mounted, so nothing is hidden): no further rising edge is coming,
  // and holding the session would leave it for the next switch to
  // abandon. Everything it measures is already known.
  if (!warmupRearmed && (lastWarmByPane.get(paneId) ?? false)) {
    sessionsByPane.delete(paneId);
    emitSession(paneId, session, { warmReason: null, abandoned: null });
  }
}

/** Report the controller's current isWarm/warmReason for a pane. Only
 * the false→true rising edge does anything; every other call (already
 * warm, still warming) is a no-op read. A mismatched threadId means the
 * pane switched again before warming — that session is emitted as
 * abandoned. A matching edge closes the session, EXCEPT the pre-items
 * edge of a fetch session: that one measured the empty pane the slice
 * had not filled yet, so it is counted and the session waits for the
 * edge the re-arm produces. */
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
  if (session.threadId !== threadId) {
    sessionsByPane.delete(paneId);
    emitSession(paneId, session, { warmReason: reason, abandoned: 'thread-changed' });
    return;
  }
  if (session.source === 'fetch' && session.itemsAppliedAt === null) {
    session.warmBeforeItems += 1;
    return;
  }
  sessionsByPane.delete(paneId);
  emitSession(paneId, session, { warmReason: reason, abandoned: null });
}

/** Test-only reset — clears all pane sessions and warm baselines. */
export function __resetColdLoadTraceForTest(): void {
  sessionsByPane.clear();
  lastWarmByPane.clear();
}
