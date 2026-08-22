// Per-thread row-size priors: persistence and thread-switch replay for
// MessageTimeline's windowing engine. Resolves the incoming thread's
// `RowEstimate` before the virtualizer remounts, and captures the
// outgoing thread's measured sizes on the same triggers as the scroll
// snapshot (see timelineRestore.svelte.ts's `saveScrollSnapshot`).
//
// Priors are keyed per-row by content signature (nodeSignature), not by
// position — see utils/virtual/priors.ts's header for why a whole-window
// positional key made restart replay nearly inert. That per-row model is
// also what makes priors survive an app restart: installSizePriorsPersistence
// (below) wires a localStorage-backed adapter into priors.ts at module
// scope, so a thread's measured sizes outlive the session, not just a
// same-run thread switch.

import { untrack } from 'svelte';
import type { ThreadPane } from '../../stores/thread.svelte';
import {
  createRowEstimate,
  getThreadSizePriors,
  hasThreadSizePriorsInMemory,
  setThreadSizePriors,
} from '../../utils/virtual/priors';
import { installSizePriorsPersistence } from '../../utils/virtual/priorsStorage';
import type { RowEstimate, TimelineVirtualizerHandle } from '../../utils/virtual/types';
import type { TimelineNode } from '../../utils/subagentGrouping';
import { ACTIVITY_RUN_CAP_REM_PX } from '../../utils/activityRunClip';
import { nodeSignature } from '../../utils/timelineStructureSignature';

// Module scope, not inside createTimelineSizePriors: this must run once,
// before any pane's timeline mounts, in both the embedded webview and
// `agent-overflow --connect` browser mode. installSizePriorsPersistence
// is itself idempotent, so re-importing this module elsewhere is safe.
installSizePriorsPersistence();

// Flat fallback row estimate for the windowing engine. Real sizes come
// from the virtualizer's per-row ResizeObserver; estimates only place
// unmeasured rows before their first measurement lands (priors → kind
// table → this default; see utils/virtual/priors.ts). A floor like the
// kind table below, for the same asymmetry.
const ESTIMATED_ROW_SIZE = 40;
// Cold-thread (priors-miss) placement estimates keyed by rendered node
// kind — leaf rows use their item kind, structural rows their node
// kind (timelineRowEstimateKind below). Estimates never decide what a
// row renders, only where unmeasured rows sit until measured — and the
// two error directions cost differently. OVERSHOOT shrinks totalSize
// when the real measurement lands: the scrollbar dips, and while
// pinned at the exact bottom the browser synchronously clamps
// scrollTop down (the remount-collapse class
// remountReturn.browser.test.ts polices). UNDERSHOOT only grows
// totalSize, which the engine's remeasure-above compensation absorbs
// invisibly; its cost is a few extra transiently mounted rows on a
// cold thread switch. So these are FLOORS, not averages: the measured
// 1-line rendered height per kind (real-Chromium probe, default
// settings, 800px pane), derated ~20% so smaller font settings stay
// under. Warm switches replay exact priors and never touch this table.
const ROW_KIND_ESTIMATE_PX: Readonly<Record<string, number>> = {
  user_text: 72,
  assistant_text: 44,
  thinking: 30,
  tool_call: 20,
  tool_completion: 20,
  error: 42,
  notification: 24,
  api_retry: 24,
  // Label line + the shortest possible one-line monospaced output block.
  command_result: 44,
  read_group: 20,
  group: 36,
  wait_group: 36,
};

/** One header line, matching ActivityRunHeader's `py-0.5` + 12px line box. */
const ACTIVITY_RUN_HEADER_PX = 24;
/**
 * Floor for an expanded run: its rows at the tightest kind height in the
 * table, capped at the clip's own ceiling. Deliberately a floor, matching
 * the rest of this table — an estimate that overshoots pushes real content
 * off the bottom of the placement, which reads worse than a short row that
 * grows on measure.
 */
const ACTIVITY_RUN_ROW_FLOOR_PX = 20;

/**
 * Minimum gap between SCROLL-DRIVEN priors captures.
 *
 * A capture is O(window): the pane's expansion signature (walks every
 * expansion registry and sorts up to four arrays), the engine's measured-
 * size slice, and a content signature per revealed node. Its trigger is
 * `handleTimelineScroll` → `saveScrollSnapshot`, which fires on every
 * scroll frame — and while the tail is pinned during streaming the
 * re-pin fires one per frame with a DIFFERENT total size each time, so
 * the O(1) size gate in front of the walk does not bite. That put the
 * whole capture on the streaming path at ~60Hz.
 *
 * Bounding it is safe because nothing depends on an interim capture
 * being current: it is a best-effort refresh of a cache, and the capture
 * that has to be exact is the settle edge (`captureOnWarmRisingEdge`),
 * which is not bounded. The worst case is that a switch lands inside a
 * cooldown and the outgoing thread stores priors up to one interval old
 * — geometry that, by definition, was still cascading.
 */
const INTERIM_PRIORS_MIN_INTERVAL_MS = 250;

/**
 * The clip's real ceiling right now. The cap is a `min()` of a viewport half
 * and a rem height (utils/activityRunClip.ts), so which half wins depends on
 * the window; taking the rem half unconditionally would overestimate every
 * long run on a short viewport — turning this floor into a ceiling-breaker
 * that shrinks total geometry when the measurement lands.
 */
function activityRunCapFloorPx(): number {
  return Math.min(window.innerHeight / 2, ACTIVITY_RUN_CAP_REM_PX);
}

/**
 * Estimate for a node the kind table cannot price, or undefined to fall back
 * to it.
 *
 * Only activity runs qualify so far. A run has no single typical height — the
 * same `runId` is a chip one moment and a capped clip the next, and one kind
 * entry would be wrong by ~20× in one of the two states, which lands
 * fast-scroll placement badly through unmeasured runs. Pure and exported so
 * the state dependence is testable without a pane.
 */
export function timelineRowStructuralSizeFor(
  node: TimelineNode | undefined,
): number | undefined {
  if (node?.kind !== 'activity_run') return undefined;
  // The header is unconditional, so a run is its header plus a clip when it has
  // one — and `collapsed` is exactly whether it has one, liveness already folded
  // in by the registry. Pricing a run that renders a clip as header-only would
  // place a fast scroll past a row that is, right now, the tallest thing on the
  // screen.
  if (node.collapsed) return ACTIVITY_RUN_HEADER_PX;
  const clip = Math.min(
    activityRunCapFloorPx(),
    node.mountedRows * ACTIVITY_RUN_ROW_FLOOR_PX,
  );
  return ACTIVITY_RUN_HEADER_PX + clip;
}

export interface TimelineSizePriorsOptions {
  getPane(): ThreadPane;
  getListRef(): TimelineVirtualizerHandle | undefined;
  getRevealedNodes(): TimelineNode[];
  getScrollSurfaceContentWidth(): number;
  getRestoredThreadId(): string | null;
}

/**
 * Diagnostic summary of the current mount's priors replay, read at the
 * warm edge by the cold-load trace (utils/coldLoadTrace.ts). `validity`
 * stays 'pending' until the first `at()` call runs the lazy-once check;
 * 'replayed-trusted-width' means the width dimension was skipped because
 * the surface had not reported a width yet at first use (see the memo in
 * `buildRowEstimate`). `rowsResolved` counts prior hits, including
 * re-consultations of the same row across structural recomputes — an
 * indicative volume, not a distinct-row count.
 */
export interface SizePriorsReplayStats {
  source: 'none' | 'memory' | 'storage';
  validity:
    | 'no-entry'
    | 'pending'
    | 'replayed'
    | 'replayed-trusted-width'
    | 'width-mismatch'
    | 'expansion-mismatch';
  rowsResolved: number;
}

export interface TimelineSizePriors {
  /** Reactive — the template binds `estimate={sizePriors.rowEstimate}`. */
  readonly rowEstimate: RowEstimate | undefined;
  /**
   * Capture now, subject only to the O(1) total-size gate. The exact
   * capture — used by the settle edge and by tests that want a capture
   * at a known moment.
   */
  maybePersistSizePriors(): void;
  /**
   * Capture from a scroll-driven trigger, rate-bounded. This is what
   * `saveScrollSnapshot` calls; see `INTERIM_PRIORS_MIN_INTERVAL_MS` for
   * why the same call cannot be the exact one.
   */
  maybePersistSizePriorsInterim(): void;
  resolveRowEstimateOnThreadEdge(threadId: string | null): void;
  captureOnWarmRisingEdge(warm: boolean): void;
  /** Non-reactive snapshot — see SizePriorsReplayStats. */
  replayStats(): SizePriorsReplayStats;
}

interface SizePriorsRowEstimateBuild {
  stats: SizePriorsReplayStats;
  estimate: RowEstimate;
}

const NO_REPLAY_STATS: SizePriorsReplayStats = {
  source: 'none',
  validity: 'no-entry',
  rowsResolved: 0,
};

export function createTimelineSizePriors(
  options: TimelineSizePriorsOptions,
): TimelineSizePriors {
  let rowEstimate = $state<RowEstimate | undefined>(undefined);
  let rowEstimateThreadId: string | null = null;
  // Diagnostics for the CURRENT mount's replay — plain (non-reactive)
  // state: it is read imperatively at the warm edge, mutated from inside
  // the rowPrior closure, and must never add reactive deps to the
  // engine's estimate path.
  let currentReplayStats: SizePriorsReplayStats = NO_REPLAY_STATS;

  // Total size at the last capture. The estimate→measure cascade only
  // moves this when rows actually measure, so it gates the capture: we
  // re-snap exactly when (and only when) the engine's geometry changed,
  // never per scroll frame. Reset on the threadId edge so the incoming
  // thread's first measured size is never mistaken for "unchanged"
  // against the outgoing thread's.
  let lastPersistedTotalSize = -1;
  let lastWarmForCapture = false;
  // Completion time of the last scroll-driven capture, or null when none
  // has run for this thread. Reset on the threadId edge so the incoming
  // thread is never made to wait out the outgoing thread's cooldown.
  let lastInterimCaptureEndedAt: number | null = null;

  // Kind resolver for the estimate fallback. Reads live `revealedNodes`,
  // so it needs no remap across head splices (the per-row prior lookup
  // below reads live data the same way — neither carries index-keyed
  // state across a splice).
  function timelineRowEstimateKind(index: number): string | undefined {
    const node = options.getRevealedNodes()[index];
    if (!node) return undefined;
    return node.kind === 'leaf' ? node.item.kind : node.kind;
  }

  function timelineRowStructuralSize(index: number): number | undefined {
    return timelineRowStructuralSizeFor(options.getRevealedNodes()[index]);
  }

  // Capture the engine's current measured sizes for the active thread, but
  // only when the total size changed since the last capture — so a 60Hz
  // spring chase doesn't re-slice the size array every frame. The most recent
  // capture before a switch is what the return replays; mirroring the
  // scroll-snapshot strategy, we never capture in the switch effect.pre
  // because `pane` has already mutated to the incoming thread by then.
  //
  // Mid-stream cost is known and tolerated: on an actively-streaming thread the
  // size-gate passes once per geometry change (each append grows the total), so
  // takeSnapshot() + the O(N) rows-map rebuild below run ~5–20×/sec — bounded
  // by the gate (never per-frame) and only while the visible thread streams.
  // Only the settle capture (isWarm rising) matters for replay; the interim
  // ones are overwritten by the next capture (setThreadSizePriors replaces the
  // whole entry). Deliberately NOT gated on spring-chase state: that would risk
  // dropping the settle capture on an already-warm streaming thread (isWarm
  // does not re-arm), regressing replay.
  //
  // A capture is a REPLACE at the store level, so an early capture must not
  // discard what a settled one stored: the restore flow saves a snapshot
  // synchronously at restore time (timelineRestore's restoreToBottom /
  // restoreAnchor), when the freshly remounted engine has measured NOTHING —
  // persisting that as-is wholesale-replaced the thread's settled entry with
  // an empty map, and the next visit replayed zero rows (the coldload trace's
  // memory/replayed/rowsResolved:0 signature). Two guards close that class:
  // rows the engine has not (re)measured CARRY FORWARD from the previous
  // entry when their signature is still live in the current window and the
  // entry's width/expansionSig match this capture's (sizes measured under
  // different geometry never mix), and a capture that still resolves nothing
  // is skipped outright rather than stored. Self-cleaning is preserved: only
  // signatures present in the current window survive a capture, so streaming
  // signature churn still drops dead rows for free.
  function maybePersistSizePriors(): void {
    const pane = options.getPane();
    // Priors stay keyed per THREAD (row sizes are a property of the
    // content, deliberately shared across surfaces showing it); the
    // restored-guard compares the pane's scroll-state key, which is what
    // the restore module records.
    const threadId = pane.threadId || null;
    const listRef = options.getListRef();
    if (!threadId || !listRef || options.getRestoredThreadId() !== pane.scrollStateKey) return;
    // O(1) read (the engine's prefix-sum total) — the cheap change-gate.
    // Skip the takeSnapshot() slice entirely when geometry hasn't moved
    // (60Hz spring).
    const totalSize = listRef.getTotalSize();
    if (totalSize === lastPersistedTotalSize) return;
    lastPersistedTotalSize = totalSize;

    const width = Math.round(options.getScrollSurfaceContentWidth());
    const expansionSig = pane.expansionSignature();
    const previous = getThreadSizePriors(threadId);
    const carry =
      previous && previous.width === width && previous.expansionSig === expansionSig
        ? previous.rows
        : undefined;
    const nodes = options.getRevealedNodes();
    const snapshot = listRef.takeSnapshot();
    const rows = new Map<string, number>();
    for (let index = 0; index < snapshot.length; index++) {
      const node = nodes[index];
      if (!node) continue;
      const signature = nodeSignature(node);
      const size = snapshot[index];
      if (size >= 0) {
        // Negative sizes (UNMEASURED or any corrupt value) never persist.
        rows.set(signature, size);
        continue;
      }
      const carried = carry?.get(signature);
      if (carried !== undefined) rows.set(signature, carried);
    }
    if (rows.size === 0) return;
    setThreadSizePriors(threadId, { width, expansionSig, rows });
  }

  function maybePersistSizePriorsInterim(): void {
    if (
      lastInterimCaptureEndedAt !== null
      && Date.now() - lastInterimCaptureEndedAt < INTERIM_PRIORS_MIN_INTERVAL_MS
    ) return;
    maybePersistSizePriors();
    // Stamped after the work, so a slow capture spaces itself out rather
    // than queueing the next one behind its own cost. Stamped even when
    // the size gate no-ops: the check itself is what was rate-bounded.
    lastInterimCaptureEndedAt = Date.now();
  }

  // Resolve the row estimate for the INCOMING thread before the
  // {#key pane.threadId} block remounts the <TimelineVirtualizer>. The
  // virtualizer configures its engine once at construction, and
  // $effect.pre runs before DOM flush, so `rowEstimate` is settled by the
  // time the remount reads it. Gated on the threadId edge: mid-thread
  // revision/width churn must not recompute it (the mounted virtualizer
  // ignores a changed `estimate` anyway), and the same-thread revert flow
  // keeps threadId constant so it never remounts.
  function resolveRowEstimateOnThreadEdge(threadId: string | null): void {
    if (threadId === rowEstimateThreadId) return;
    rowEstimateThreadId = threadId;
    lastPersistedTotalSize = -1;
    lastInterimCaptureEndedAt = null;
    if (threadId) {
      const build = untrack(() => buildRowEstimate(threadId));
      currentReplayStats = build.stats;
      rowEstimate = build.estimate;
    } else {
      currentReplayStats = NO_REPLAY_STATS;
      rowEstimate = undefined;
    }
  }

  // Fetching the stored entry is cheap and layout-independent (it may
  // lazily hydrate from localStorage), so it happens eagerly here. The
  // width/expansionSig VALIDITY CHECK is deliberately deferred to the
  // first `at()` call instead: this function runs in $effect.pre, before
  // the virtualizer remounts, and on a fresh app boot the scroll surface
  // has not been laid out yet — `getScrollSurfaceContentWidth()` would
  // read 0 here, and checking eagerly would spuriously refuse EVERY
  // restart replay (0 never equals a captured width).
  //
  // Deferral alone is not enough, though: the width signal is RO-only
  // (scrollSurfaceWidth.ts's async-delivery rule), and the engine's FIRST
  // `at()` calls run synchronously when the virtualizer mounts with data
  // (sizes.ts's updateLength consults estimates eagerly for the spacer
  // height). On boot, whichever lands first — the surface RO's initial
  // width delivery or the item fetch's WS response — is a machine-speed
  // race, so the first `at()` can still legitimately see width 0. A width
  // of 0 is "layout hasn't reported yet", not a real wrap point, so the
  // memo TRUSTS the entry's captured width in that case rather than
  // refusing it: window geometry restores across restarts, so the real
  // width almost always matches, and when it doesn't the stale replay
  // degrades to the documented self-correcting residual (per-row RO
  // corrections behind the warm-up gate — priors.ts header), which is
  // strictly better than guaranteeing the full cascade. The decision is
  // latched either way so every row in the mount resolves consistently.
  function buildRowEstimate(threadId: string): SizePriorsRowEstimateBuild {
    const inMemory = hasThreadSizePriorsInMemory(threadId);
    const entry = getThreadSizePriors(threadId);
    const stats: SizePriorsReplayStats = {
      source: entry ? (inMemory ? 'memory' : 'storage') : 'none',
      validity: entry ? 'pending' : 'no-entry',
      rowsResolved: 0,
    };
    let valid: boolean | undefined;

    function rowPrior(index: number): number | undefined {
      if (!entry) return undefined;
      if (valid === undefined) {
        const width = Math.round(options.getScrollSurfaceContentWidth());
        if (entry.expansionSig !== options.getPane().expansionSignature()) {
          valid = false;
          stats.validity = 'expansion-mismatch';
        } else if (width === 0) {
          valid = true;
          stats.validity = 'replayed-trusted-width';
        } else if (width !== entry.width) {
          valid = false;
          stats.validity = 'width-mismatch';
        } else {
          valid = true;
          stats.validity = 'replayed';
        }
      }
      if (!valid) return undefined;
      const node = options.getRevealedNodes()[index];
      if (!node) return undefined;
      const size = entry.rows.get(nodeSignature(node));
      if (size !== undefined) stats.rowsResolved += 1;
      return size;
    }

    return {
      stats,
      estimate: createRowEstimate({
        rowPrior,
        structuralSize: timelineRowStructuralSize,
        kindOf: timelineRowEstimateKind,
        kindHeights: ROW_KIND_ESTIMATE_PX,
        defaultSize: ESTIMATED_ROW_SIZE,
      }),
    };
  }

  // Guarantee a post-settle capture. The scroll-driven captures
  // (handleTimelineScroll/ScrollEnd → saveScrollSnapshot) only store settled
  // sizes if the cascade's bottom-pin re-pins fire scroll events — which
  // an idle, bottom-pinned thread the user never scrolls cannot rely on, so the
  // only stored snapshot would be the pre-settle estimate and the NEXT visit
  // would replay it and still cascade. `stick.isWarm` is the controller's
  // existing "measurement cascade has settled" signal (QUIET_MS of geometry
  // stillness); on its rising edge the sizes are final. Capture is `untrack`ed
  // so this effect depends ONLY on isWarm — not on the geometry/content
  // maybePersistSizePriors reads — keeping it a settle-edge trigger, not a
  // content watcher. Size-gated downstream, so if a scroll capture already
  // stored the settled total this is a no-op; cascade-interim warm flickers
  // store interim sizes the final settle overwrites.
  function captureOnWarmRisingEdge(warm: boolean): void {
    const rising = warm && !lastWarmForCapture;
    lastWarmForCapture = warm;
    if (rising) untrack(() => maybePersistSizePriors());
  }

  return {
    get rowEstimate() {
      return rowEstimate;
    },
    maybePersistSizePriors,
    maybePersistSizePriorsInterim,
    resolveRowEstimateOnThreadEdge,
    captureOnWarmRisingEdge,
    replayStats: () => currentReplayStats,
  };
}
