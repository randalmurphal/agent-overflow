import type { ItemKind, Thread } from '../types/models';
import type { ScrollObservationKind } from '../utils/scroll/types';
import type { SmoothingClock } from '../markdown/smoothing/PerItemSmoother';
import type { RhsPanel } from './rhsPanelSlot.svelte';
import type { ActiveTurn } from './threadStatuses.svelte';

// Test-only injection: when set, every PerItemSmoother created by
// `getOrCreateSmoothing` uses this clock instead of the default rAF +
// performance.now() pair. Lets reveal-cadence tests run deterministically
// without globally monkey-patching browser APIs.
let smoothingClockForTest: SmoothingClock | undefined;

export function __setSmoothingClockForTest(
  clock: SmoothingClock | undefined,
): void {
  smoothingClockForTest = clock;
}

export function getSmoothingClockForTest(): SmoothingClock | undefined {
  return smoothingClockForTest;
}

// Live-content stamp timebase. Reuses the smoother's clock so a stamp
// written from inside `onReveal` shares that callback's time source.
export function nowForLiveContent(): number {
  return smoothingClockForTest?.now() ?? performance.now();
}

// Reasoning-tail kinds share the live 3-line tail UX — a smoother-driven
// monotonic tail, a tail-trimmed summary, and mid-stream payload expansion.
// Accepts a plain string because `Item.kind` is `ItemKind | string` to absorb
// unknown wire values without a guard at every call site.
export function isReasoningTailKind(kind: ItemKind | string): boolean {
  return kind === 'thinking' || kind === 'compaction_reasoning';
}

// Smoothable kinds: assistant_text + the reasoning-tail kinds. Tool calls,
// errors, notifications, etc. pass through directly — they have their own
// rendering and don't benefit from word-aligned reveal.
export function isSmoothLiveContentKind(kind: ItemKind | string): boolean {
  return kind === 'assistant_text' || isReasoningTailKind(kind);
}

/**
 * Default raw-item budget passed to `ListItemsBeforeTurn` for an
 * explicit "Load older" page. The backend walks turns DESC summing
 * each turn's item count until cumulative >= this budget, then returns
 * that turn's items plus every newer one below the caller's floor. One
 * click loads about this many items regardless of per-turn density.
 */
export const LOAD_OLDER_ITEM_BUDGET = 200;

/**
 * Target loaded item count after pruning or recentering a long active
 * timeline window. This is intentionally larger than the initial slice so
 * paging has room to preserve reading context without keeping a whole thread.
 */
export const ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS = 500;

/**
 * Loaded item count that arms pruning/recentering. The pane keeps a
 * single contiguous window; exceeding this cap drops the far side and
 * exposes an older/newer gap control. The recent-window path defers
 * its prune to turn settle while a turn is streaming — see
 * ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS below.
 */
export const ACTIVE_TIMELINE_WINDOW_MAX_ITEMS = 800;

/**
 * Memory backstop for the prune deferral. While a turn is streaming, the
 * recent-window prune waits for turn settle (a mid-stream head-drop
 * repaints the visible timeline — incident 2026-06-10); a single turn
 * that streams past this ceiling gets pruned anyway, accepting the
 * repaint over unbounded growth.
 */
export const ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS = 1600;

/**
 * Initial-load slice size on `switchThread`. Sized to cover several
 * desktop viewports and large enough that one dense subagent turn
 * collapsing to a single card does not leave the timeline visually empty.
 */
export const SLICE_AROUND_ITEM_BUDGET = 200;

/**
 * Doherty perception threshold for suppressing spinner flash on fast
 * thread switches.
 */
export const SPINNER_THRESHOLD_MS = 100;

/**
 * Maximum runes the frontend keeps in `items[i].summary` for a streaming
 * thinking row. Mirrors `thinkingPreviewRunes` in
 * `internal/triage/stream_items.go`.
 *
 * Also load-bearing for merge correctness: reconnect interior reveal
 * matching treats an already-shown suffix at least this long as safe via
 * substring containment. Lowering this toward the length of common
 * repeated phrases can false-match genuine new tails and drop them until
 * settle.
 */
export const THINKING_TAIL_RUNES = 400;

/**
 * Returns the tail of `text` containing at most `maxRunes` Unicode code
 * points.
 */
export function trimToTailRunes(text: string, maxRunes: number): string {
  if (text.length <= maxRunes) return text;
  let runes = 0;
  for (let i = text.length; i > 0; ) {
    const codeUnit = text.charCodeAt(i - 1);
    i -= codeUnit >= 0xdc00 && codeUnit <= 0xdfff && i > 1 ? 2 : 1;
    runes += 1;
    if (runes >= maxRunes) return text.slice(i);
  }
  return text;
}

export function sameRhsPanel(
  left: RhsPanel | null,
  right: RhsPanel | null,
): boolean {
  if (left === null || right === null) return left === right;
  if (left.kind !== right.kind) return false;
  if (left.kind !== 'diff-payload' || right.kind !== 'diff-payload')
    return true;
  return left.payloadId === right.payloadId && left.filePath === right.filePath;
}

export type LoadOlderResult = {
  insertedBeforeWindow: boolean;
  insertedRows: boolean;
  status: 'loaded' | 'noop' | 'stale' | 'error';
};

/**
 * Minimal surface a registered scroll controller exposes to the pane.
 * MessageTimeline registers an explicit adapter (its `observe` routes
 * `'host-layout'` through the listRef-aware retry ladder and it adds the
 * timeline-window anchor transaction); ChannelView registers its raw
 * `useStickToBottom` controller, which satisfies the required members
 * directly.
 */
export interface PaneScrollController {
  pauseAutoScroll(): () => void;
  observe(kind: ScrollObservationKind): void;
  preserveScrollAnchor(
    anchor: HTMLElement,
    action: () => void | Promise<void>,
  ): Promise<void>;
  preserveTimelineWindowAnchor?(
    operation: TimelineWindowAnchorOperation,
  ): boolean;
}

export interface TimelineWindowAnchorOperation {
  keepsItem(itemId: string): boolean;
  run(): void;
}

export interface ScrollToItemOptions {
  flash?: boolean;
}

export interface ScrollToItemRequest {
  itemId: string;
  nonce: number;
  flash: boolean;
}

export function loadOlderResult(
  status: LoadOlderResult['status'],
  insertedBeforeWindow = false,
  insertedRows = false,
): LoadOlderResult {
  return { status, insertedBeforeWindow, insertedRows };
}

export interface LiveStateHydrationGuard {
  activeTurnAtRequest: ActiveTurn | null;
  queueRevisionAtRequest: number;
  liveTodoRevisionAtRequest: number;
}

/**
 * Returns the absolute workspace path of a pane's active thread.
 */
export function paneWorkspacePath(
  pane: { thread: Thread | null } | undefined,
): string {
  return pane?.thread?.workspacePath ?? '';
}

export type DraftPlaceholderMode = 'chat' | 'plan' | 'design';

export interface DraftThreadPlaceholder {
  id: string;
  projectId: string;
  projectName: string;
  projectPath: string;
  mode: DraftPlaceholderMode;
  createdAt: number;
}

export interface DraftPlaceholderDefaults {
  provider?: string;
  model?: string;
  reasoningEffort?: string;
  fastMode?: boolean;
  contextWindow?: number;
  runtimeMode?: string;
  branch?: string;
  workspacePath?: string;
}

export interface ThreadPaneOptions {
  paneId?: string;
}
