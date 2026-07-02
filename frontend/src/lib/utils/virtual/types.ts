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

export type ScrollToIndexAlign = 'start' | 'center' | 'end' | 'nearest';
