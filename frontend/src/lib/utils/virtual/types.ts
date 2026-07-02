// Shared types for the bespoke timeline virtualizer engine
// (utils/virtual/). Pure data shapes — no DOM, no Svelte imports.
// Design: docs/architecture/virtualizer-replacement-plan.md §2.

/** Inclusive mounted-row range. `[0, -1]` means "mount nothing". */
export type ItemsRange = readonly [startIndex: number, endIndex: number];

/**
 * A geometry change that moved content above the viewport top, reported by
 * the engine as an observation. The engine NEVER writes scrollTop — the
 * scroll controller's resolver decides whether `target` is applied (while
 * pinned the per-beat pin write already covers it, so a decline is not an
 * error state).
 */
export interface EngineCompensation {
  kind: 'remeasure-above' | 'head-splice';
  /** px the content above the viewport grew (+) or shrank (−). */
  delta: number;
  /** Absolute scrollTop that keeps the reading position stationary. */
  target: number;
}

/**
 * One engine output batch: the adapter applies `window`/`totalSize` to the
 * DOM and forwards `compensation` to the scroll controller. `null` from a
 * reducer entry point means nothing changed — the range-equality early-out
 * that keeps per-scroll-event work near-free.
 */
export interface EngineUpdate {
  window: ItemsRange;
  totalSize: number;
  compensation?: EngineCompensation;
}

/**
 * Per-row size estimate used for rows that have no measurement yet.
 * Backed by priors (utils/virtual/priors.ts): persisted snapshot value →
 * kind-based table → flat default. `at` MUST be stable per index between
 * structural changes — the size store's offsets memo bakes estimates in,
 * and the engine only invalidates that memo on structural events.
 */
export interface RowEstimate {
  at(index: number): number;
  /**
   * Remap index-keyed state after a head splice: `count > 0` rows were
   * inserted at the head, `count < 0` removed. Called by the engine at
   * the correct moment relative to the store splice (before a prepend,
   * after a removal) so estimates always resolve against live indices.
   */
  shiftBase(count: number): void;
}

/**
 * One engine-sourced content-geometry sample, delivered by
 * TimelineVirtualizer after the template flush (DOM consistent, still
 * pre-paint) — the replacement for the scroll controller's contentEl
 * ResizeObserver in chat. `height` is the engine's totalSize, which the
 * spacer's explicit height makes identical to the content element's
 * height; `width` is the scroller's content-box width from the adapter's
 * own ResizeObserver (the single async width source — never a
 * synchronous layout read). The two settle fields are per-row settle
 * evidence for the controller's warm-up gate: everything mounted has
 * measured, and how far those first measurements landed from their
 * estimates (a priors-hit revisit measures ~0 correction; a cold
 * estimate cascade measures large ones).
 */
export interface ContentGeometrySample {
  /** Content height = the engine's totalSize (the spacer height just written). */
  height: number;
  /** Scroller content-box width (the wrap point; width-reflow classification). */
  width: number;
  /** Every row in the current mount window has received its first measurement. */
  windowMeasured: boolean;
  /** Max |measured − estimated| px over all first measurements since mount. */
  maxFirstMeasureCorrectionPx: number;
}

export type ScrollToIndexAlign = 'start' | 'center' | 'end' | 'nearest';

/**
 * Imperative surface of components/chat/TimelineVirtualizer.svelte
 * (structurally satisfied by its component instance). `scrollToIndex`
 * computes the target in the engine and performs the write through the
 * scroll controller chokepoint (`applyScrollTarget` prop); `revalidate`
 * is the explicit host-layout geometry recheck; the rest are read-only
 * geometry queries.
 */
export interface TimelineVirtualizerHandle {
  scrollToIndex(index: number, opts?: { align?: ScrollToIndexAlign; offset?: number }): void;
  revalidate(): void;
  getScrollOffset(): number;
  getViewportSize(): number;
  getScrollSize(): number;
  getTotalSize(): number;
  findItemIndex(offset: number): number;
  getItemOffset(index: number): number;
  sizeAt(index: number): number;
  isMeasuredAt(index: number): boolean;
  /** Measured sizes for priors persistence (UNMEASURED where unmeasured). */
  takeSnapshot(): number[];
}
