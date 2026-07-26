import type { ItemKind, Thread } from '../types/models';
import type { ScrollObservationKind } from '../utils/scroll/index.svelte';
import type { SmoothingClock } from '../markdown/smoothing/PerItemSmoother';
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

/**
 * Whether a thread renders the discussion surface (ChannelView) instead
 * of the chat timeline. Shared by ChatView's surface swap,
 * DiscussionView's render gate, and the pane's structural-spring arm
 * skip — the three must agree, or the pane arms a controller that
 * watches channel messages rather than timeline rows.
 */
export function threadUsesDiscussionSurface(
  thread: Pick<Thread, 'mode' | 'discussionId'> | null | undefined,
): boolean {
  return !!thread && thread.mode === 'discussion' && !!thread.discussionId;
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
  /**
   * One-shot structural-append spring arm (250ms TTL in the controller).
   * The pane data layer is the sole caller (`armStructuralSpring` in
   * thread.svelte.ts): synchronously while applying provider upserts that
   * APPEND in-window rows, when the reveal gate releases withheld rows
   * (both of which also stamp the pane's live-content latch — see
   * `armLiveContentAppendSpring`), and for the composer's optimistic
   * user-send (arm only) — each ordered strictly before the flush in
   * which the virtualizer delivers the resulting content-geometry
   * sample. A component-effect arm loses that race — and is blind to
   * appends outside an active turn (interrupt echo, force-closed tool
   * rows) — so the append's own growth sync-pins as a visible teleport
   * (bug-report-20260702T193212Z).
   */
  markStructuralContentPending(): void;
  preserveScrollAnchor(
    anchor: HTMLElement,
    action: () => void | Promise<void>,
  ): Promise<void>;
  preserveTimelineWindowAnchor?(
    operation: TimelineWindowAnchorOperation,
  ): boolean;
  /**
   * Run a deliberate height change with the viewport's BOTTOM edge held, so it
   * opens upward instead of pushing the reader's rows down the page — and
   * without the spring, which would otherwise animate across the whole delta.
   *
   * Optional for the same reason the anchor transaction is: ChannelView
   * registers a raw controller with no virtualizer behind it. Go through
   * `withViewportBottomHeld` rather than reaching for it directly.
   */
  preserveViewportBottom?(change: () => void): void;
}

export interface TimelineWindowAnchorOperation {
  keepsItem(itemId: string): boolean;
  run(): void;
}

/**
 * Apply `change` with the viewport's bottom edge held, wherever the pane's
 * controller can do that, and plainly wherever it cannot.
 *
 * The fallback is the whole reason this exists: a caller that wrote
 * `controller?.preserveViewportBottom?.(change)` would silently do NOTHING on a
 * surface without one, and the run would simply not collapse.
 */
export function withViewportBottomHeld(
  controller: PaneScrollController | null,
  change: () => void,
): void {
  const hold = controller?.preserveViewportBottom;
  if (!hold) {
    change();
    return;
  }
  hold.call(controller, change);
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
  providerSessionAccountRevisionAtRequest: number;
  effectiveModelRevisionAtRequest: number;
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
