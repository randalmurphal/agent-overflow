// The reveal-queue drain probe: `window.__aoRevealDrain()`, whitelisted in
// the harness bridge's globals table (`lib/harness/globals.ts`).
//
// WHY IT EXISTS. A bench or a profile window used to end at
// `provider:turn_completed`, which is when the WIRE finished — but the thing
// a reader watches is the reveal queue draining afterwards, and under the
// mock providers that drain outlives the turn by an order of magnitude (a
// burst-stream turn closes in about a second and keeps revealing for ten).
// Every measurement that stopped at turn completion therefore excluded the
// part of the stream a human actually sees.
//
// WHY IT IS THIS SHAPE. The bridge's settledness machinery cannot answer
// this: `perf start`/`stop` force-disarm the document-wide MutationObserver
// precisely so a perf run measures a renderer with no observer on it
// (bridge.ts header), and re-arming one to detect quiet would perturb the
// experiment it is timing. The drain is already cheap STORE state — one
// SvelteMap size and one nullable field per pane — so the probe reads that
// instead. Three integers, no DOM walk, no allocation per pane beyond the
// summary object.
//
// It is a READ. Nothing here snaps, skips, rushes or pops the drain; the
// reveal queue's behaviour is unchanged by the presence of a reader (repo
// doctrine: nothing may skip the readable drain).

/** The per-pane facts the summary folds. Structural, so tests need no pane. */
export interface RevealDrainPane {
  /** Non-null while the timeline is withholding nodes behind the gate. */
  readonly revealBoundary: unknown;
  /** Live per-item smoothers in this pane. */
  readonly smoothingItemCount: number;
}

export interface RevealDrainSummary {
  v: 1;
  /** Panes mounted in the registry right now. */
  panes: number;
  /**
   * Panes still draining: a live smoother, a standing reveal boundary, or
   * both. This is the number a caller waits on — `draining === 0` is the
   * claim that no pane is mid-reveal.
   */
  draining: number;
  /** Live smoothers across every pane. */
  smoothers: number;
  /** Panes whose reveal gate is engaged. */
  boundaries: number;
}

/**
 * Folds the panes into the summary. Pure, and separate from the window
 * install so the arithmetic is testable without a pane registry.
 */
export function summarizeRevealDrain(panes: Iterable<RevealDrainPane>): RevealDrainSummary {
  let total = 0;
  let draining = 0;
  let smoothers = 0;
  let boundaries = 0;
  for (const pane of panes) {
    total += 1;
    // A pane whose accessors are missing (an older pane shape, a stub in a
    // test) contributes zero rather than NaN: an unreadable pane must not
    // make the whole summary unreadable.
    const count = typeof pane.smoothingItemCount === 'number' && Number.isFinite(pane.smoothingItemCount)
      ? pane.smoothingItemCount
      : 0;
    const gated = pane.revealBoundary !== null && pane.revealBoundary !== undefined;
    smoothers += count;
    if (gated) boundaries += 1;
    if (count > 0 || gated) draining += 1;
  }
  return { v: 1, panes: total, draining, smoothers, boundaries };
}

/**
 * The live reading, over the pane registry.
 *
 * The registry is reached by DYNAMIC import so this module stays free of
 * store state at import time: it is loaded by a `window.__aoRevealDrain`
 * call that already awaits, and keeping the static graph empty means the
 * arithmetic above is unit-testable without instantiating a pane. The
 * import resolves from the module cache on every call after the first.
 */
export async function revealDrainStats(): Promise<RevealDrainSummary> {
  const { listPanes } = await import('../stores/panes.svelte');
  return summarizeRevealDrain(listPanes());
}
