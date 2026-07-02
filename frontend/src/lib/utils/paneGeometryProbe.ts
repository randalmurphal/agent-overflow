// Dev-only per-pane scroll-geometry probe for diagnosing the width-reflow
// strand: after a chat pane WIDENS (e.g. closing a third pane so two remaining
// panes grow wider), its timeline is left with the last message floating high
// and a large gap down to the composer — and it never self-corrects, even on
// manual scroll up/down.
//
// `window.__stickState` (useStickToBottom's dev hook) is the WRONG instrument
// for this bug for two reasons: it is last-writer-wins (reports a single,
// arbitrary pane) and it centers on `distanceFromBottom`, which reads ~0 BOTH
// when healthy AND at this strand (you are pinned at a wrong max-scroll), so it
// cannot tell the two apart. This probe instead dumps EVERY mounted timeline and
// reports the per-row delta that DOES discriminate the failure:
//
//   slotVsWrapper = engine slot size (sizeAt) − wrapper.offsetHeight
//     > 0 ⇒ the engine's slot is taller than the real (correct) DOM row, i.e.
//           a stale/dropped re-measure inside the size store.
//
// `scrollHeight === engine totalSize` exactly (the virtualizer container is
// `contain: size` + explicit `height: <totalSize>px`), so an inflated BOTTOM-row
// slot is the only way to strand the last row floating high at max scroll. Read
// the bottom rendered row (`bottomRenderedIndex`): a positive delta there names
// the mechanism; deltas ~0 while `scrollHeight` still exceeds the row sum
// points at a spacer / elsewhere. Compare `scrollSurfaceContentWidth` to `clientWidth` to
// catch a size-priors replay keyed at a stale (pre-widen) width.
//
// See docs/architecture/frontend-scroll.md and the Ctrl+Shift+B capture in
// uiRenderTrace.ts, which folds this dump into the `user.bugReport` marker so a
// single keypress at the stable strand is self-describing.
//
// For TRANSIENT failures — a strand that self-heals in ~1s — a single-shot
// capture is hard to time by hand. `setPaneGeometryRecording(true)` (Ctrl+Shift+Y,
// wired in uiRenderTrace.ts) arms a ~10Hz rolling sampler; the next Ctrl+Shift+B
// emits the whole timestamped buffer as correlated `user.bugReportRecFrame` lines
// (one trace line per frame — folding all ~80 full frames into a SINGLE marker
// line blew the 64KiB per-line cap and the marker was silently dropped). So ONE
// keypress AFTER the glitch captures the full reserve→release transition and lets
// the analyst MEASURE the release delay instead of inferring it. Opt-in because
// sampling forces layout at 10Hz; off by default, zero cost unless armed.
//
// The recorder's timer + ring are MODULE state, deliberately decoupled from pane
// mount/unmount: the width-reflow strand is frequently triggered BY a timeline
// remount (thread switch, pane close), and a recorder torn down with its pane
// would clear itself on the exact transition it exists to capture (this silently
// ate an earlier capture — the ring dumped empty). So arming survives any number
// of pane remounts; only an explicit disarm (Ctrl+Shift+Y again) or
// stopPaneGeometryRecording() clears it. The per-pane DUMP hook (__paneGeometry)
// still tears down with the last pane; the rolling buffer does not.
//
// Mirrors timelineMemoryDiagnostics.ts: per-pane registry, dev-only build gate.

const PANE_GEOMETRY_BUILD_GATE =
  import.meta.env.DEV ||
  import.meta.env.MODE === 'test' ||
  import.meta.env.VITE_AGENT_OVERFLOW_UI_TRACE === '1';

export interface PaneGeometryRowSample {
  // Engine item index (the `data-row-index` on the measured wrapper).
  index: number;
  // The [data-row-index] wrapper's border-box height.
  wrapperHeight: number;
  // The engine's slot size for this index (sizeAt: measured or estimate).
  slotSize: number | null;
  // slotSize − wrapper. > 0 ⇒ the engine's slot is taller than the real DOM row.
  slotVsWrapper: number | null;
}

export interface PaneGeometrySnapshot {
  paneId: string;
  threadId: string | null;
  // Controller intent, read straight off THIS pane's stick controller — not the
  // last-writer-wins __stickState global.
  isAtBottom: boolean;
  isSticky: boolean;
  escapedFromLock: boolean;
  isWarm: boolean;
  // Scroll-container geometry. scrollHeight === engine totalSize.
  scrollTop: number | null;
  scrollHeight: number | null;
  clientHeight: number | null;
  clientWidth: number | null;
  distanceFromBottom: number | null;
  // Scroll surface content-box width from the async width observer (feeds the
  // size-priors validity key); compare to clientWidth to catch a
  // stale (pre-reflow) width.
  scrollSurfaceContentWidth: number | null;
  itemsLength: number;
  // listRef.getTotalSize() — should equal scrollHeight (the container height
  // IS this value); a mismatch means the height write didn't land.
  engineTotalSize: number | null;
  bottomRenderedIndex: number | null;
  // Every currently-rendered row, ordered top → bottom by index.
  rows: PaneGeometryRowSample[];
  error?: string;
}

export type PaneGeometryGetter = () => PaneGeometrySnapshot;

// One rolling-buffer frame: a full multi-pane dump stamped with milliseconds
// since recording was armed, so the analyst can read the reserve→release
// transition as a time series and measure how long a too-tall row was held.
export interface PaneGeometryRecordingSample {
  t: number;
  panes: Record<string, PaneGeometrySnapshot>;
}

declare global {
  interface Window {
    __paneGeometry?: () => Record<string, PaneGeometrySnapshot>;
    // Arm/disarm the rolling sampler (no arg toggles); returns the new state.
    __paneGeometryRecord?: (on?: boolean) => boolean;
    // The rolling buffer captured so far (oldest → newest).
    __paneGeometryRecording?: () => PaneGeometryRecordingSample[];
  }
}

const gettersByPane = new Map<string, PaneGeometryGetter>();

// Register a pane's geometry getter and (re)install the window hook. Returns a
// teardown that removes this pane and drops the hook once the last pane leaves.
// A no-op outside the dev build gate, so production carries no surface.
export function installPaneGeometryProbe(
  paneId: string,
  getSnapshot: PaneGeometryGetter,
): () => void {
  if (!PANE_GEOMETRY_BUILD_GATE || typeof window === 'undefined') return () => {};
  gettersByPane.set(paneId, getSnapshot);
  window.__paneGeometry = dumpAllPaneGeometry;
  window.__paneGeometryRecord = setPaneGeometryRecording;
  window.__paneGeometryRecording = getPaneGeometryRecording;
  return () => {
    if (gettersByPane.get(paneId) === getSnapshot) {
      gettersByPane.delete(paneId);
    }
    if (gettersByPane.size === 0 && typeof window !== 'undefined') {
      // Drop the per-pane DUMP surface with the last pane, but DO NOT touch the
      // rolling recorder: it must outlive a remount to capture the transition
      // across it (see header). Stopping it here clears the ring, and the strand
      // trigger IS a remount — that silently ate an earlier capture.
      delete window.__paneGeometry;
      delete window.__paneGeometryRecord;
      delete window.__paneGeometryRecording;
    }
  };
}

function dumpAllPaneGeometry(): Record<string, PaneGeometrySnapshot> {
  const out: Record<string, PaneGeometrySnapshot> = {};
  for (const [id, getter] of gettersByPane) {
    out[id] = getter();
  }
  return out;
}

const RECORDING_SAMPLE_INTERVAL_MS = 100;
// ~8s of history at 10Hz — long enough to hold a trigger and the self-heal that
// follows, bounded so an armed-and-forgotten recorder can't grow without limit.
export const RECORDING_MAX_SAMPLES = 80;

let recordingTimer: ReturnType<typeof setInterval> | null = null;
let recordingStartedAt = 0;
const recordingRing: PaneGeometryRecordingSample[] = [];

function recordingNow(): number {
  return typeof performance !== 'undefined' && typeof performance.now === 'function'
    ? performance.now()
    : Date.now();
}

function sampleRecordingRing(): void {
  recordingRing.push({
    t: Math.round(recordingNow() - recordingStartedAt),
    panes: dumpAllPaneGeometry(),
  });
  while (recordingRing.length > RECORDING_MAX_SAMPLES) recordingRing.shift();
}

// Arm (or, with no arg, toggle) the rolling sampler. Arming resets the buffer and
// takes an immediate t≈0 frame; disarming clears the interval but keeps the buffer
// so a Ctrl+Shift+B right after disarm still dumps it. Returns the new state.
export function setPaneGeometryRecording(on?: boolean): boolean {
  const isRecording = recordingTimer !== null;
  const next = on ?? !isRecording;
  if (next === isRecording) return isRecording;

  if (next) {
    recordingRing.length = 0;
    recordingStartedAt = recordingNow();
    sampleRecordingRing();
    recordingTimer = setInterval(sampleRecordingRing, RECORDING_SAMPLE_INTERVAL_MS);
  } else if (recordingTimer !== null) {
    clearInterval(recordingTimer);
    recordingTimer = null;
  }
  return next;
}

export function getPaneGeometryRecording(): PaneGeometryRecordingSample[] {
  return recordingRing.slice();
}

// Explicit hard reset: stop the sampler AND clear the ring. Deliberately NOT
// wired to pane teardown — a timeline remount (often the strand trigger itself)
// must not nuke an in-flight capture. Used by tests for isolation and available
// as a manual full-stop; ordinary disarm (setPaneGeometryRecording(false)) keeps
// the ring so a trailing Ctrl+Shift+B can still dump it.
export function stopPaneGeometryRecording(): void {
  if (recordingTimer !== null) {
    clearInterval(recordingTimer);
    recordingTimer = null;
  }
  recordingRing.length = 0;
}

// Test-only: synchronous read of the live dump without going through `window`,
// so registry/teardown behavior can be asserted in jsdom-free unit tests.
export function dumpPaneGeometryForTest(): Record<string, PaneGeometrySnapshot> {
  return dumpAllPaneGeometry();
}
