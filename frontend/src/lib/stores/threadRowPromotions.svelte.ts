// stores/threadRowPromotions.svelte.ts
//
// OWNS the rows a pane holds back while the row they replace finishes on
// screen. A Claude thinking block the provider tags as narration settles as
// an assistant_text row under the same id (docs/references/claude-wire.md
// §The signature tag). The pane keeps its streaming thinking row until the reveal
// has shown the last word and the timeline has presented the change, then
// installs the held row in one commit. Until that commit the pane's items
// describe exactly what is on screen, so no reader of them needs to know a
// promotion exists.
//
// A hold moves through three phases:
//   draining    the reveal is still behind the row's text
//   drained     the last word is shown; a presenter is asked for
//   presenting  a mounted timeline runs the change and reports its end
// A hold no presenter takes commits at once. A presenting hold commits when
// its presenter completes it, when the presenter goes away, or at the
// ceiling below, whichever is first.
//
// MUST NOT touch smoothers or the reveal gate. The composition root
// (threadStreamingReveal.svelte.ts) sequences those with each phase change.

import type { Item } from '../types/models';
import type { ItemPatchEvent } from '../types/events';

/**
 * Longest a presentation may hold the reveal queue. The promotion springs
 * land in under a second; a presenter that has not reported by this bound
 * lost its frames (hidden tab, stalled main thread) and the row installs
 * without it.
 */
export const PROMOTION_PRESENT_CEILING_MS = 2000;

export type RowPromotionPhase = 'draining' | 'drained' | 'presenting';

/** The fields a patch or meta event can change on a held row. */
export type RowPromotionPatch = Partial<
  Pick<ItemPatchEvent['patch'], 'rev' | 'status' | 'summary' | 'meta' | 'decision' | 'updatedAt'>
>;

/** A hold a presenter is showing: the row the pane will install. */
export interface PresentingRowPromotion {
  readonly itemId: string;
  readonly row: Item;
}

/**
 * Asked once per hold, synchronously, when its reveal has drained. Returns
 * true to take the presentation: the hold then stays until `complete`.
 * Read-only: it must not write items, smoothers or the DOM.
 */
export type RowPromotionPresenter = (itemId: string, row: Item) => boolean;

export interface ThreadRowPromotionsOptions {
  /** Install the held row. Called exactly once per hold that is not dropped. */
  commit(row: Item): void;
  setTimeout?: (handler: () => void, ms: number) => unknown;
  clearTimeout?: (handle: unknown) => void;
}

export interface ThreadRowPromotions {
  has(itemId: string): boolean;
  readonly size: number;
  ids(): IterableIterator<string>;
  phase(itemId: string): RowPromotionPhase | undefined;
  /** Start a hold in `draining`, or replace the row an existing hold keeps. */
  hold(row: Item): void;
  /**
   * Fold changed fields into the held row, as the pane would into its own.
   * False when no hold keeps a row for the id.
   */
  patch(itemId: string, fields: RowPromotionPatch): boolean;
  /** The reveal reached the end of the row: `draining` becomes `drained`. */
  markDrained(itemId: string): void;
  /** Ask the presenters for every `drained` hold; commit the ones none takes. */
  presentDrained(): void;
  /** The presenter finished (or abandoned) the hold: install its row. */
  complete(itemId: string): void;
  /** Install every held row now, in any phase, without presenting it. */
  completeAll(): void;
  /** The row left the window: forget the hold without installing anything. */
  drop(itemId: string): void;
  /** Thread switch or pane clear. */
  clear(): void;
  /** Reactive. The holds a presenter is showing, in the order they started. */
  readonly presenting: readonly PresentingRowPromotion[];
  /**
   * Register a presenter for its lifetime. Unregistering completes the holds
   * it took, so a timeline that unmounts mid-presentation never strands one.
   */
  registerPresenter(presenter: RowPromotionPresenter): () => void;
}

interface Hold {
  row: Item;
  phase: RowPromotionPhase;
  presenter: RowPromotionPresenter | null;
  ceiling: unknown;
}

export function createThreadRowPromotions(
  options: ThreadRowPromotionsOptions,
): ThreadRowPromotions {
  const startTimer = options.setTimeout ?? ((handler, ms) => setTimeout(handler, ms));
  const stopTimer = options.clearTimeout
    ?? ((handle) => clearTimeout(handle as ReturnType<typeof setTimeout>));
  const holds = new Map<string, Hold>();
  const presenters = new Set<RowPromotionPresenter>();
  let presenting: readonly PresentingRowPromotion[] = $state.raw([]);

  function publishPresenting(): void {
    const next: PresentingRowPromotion[] = [];
    for (const [itemId, hold] of holds) {
      if (hold.phase === 'presenting') next.push({ itemId, row: hold.row });
    }
    if (next.length === 0 && presenting.length === 0) return;
    presenting = next;
  }

  function release(itemId: string): Hold | undefined {
    const hold = holds.get(itemId);
    if (!hold) return undefined;
    holds.delete(itemId);
    if (hold.ceiling !== null) stopTimer(hold.ceiling);
    if (hold.phase === 'presenting') publishPresenting();
    return hold;
  }

  function complete(itemId: string): void {
    const hold = release(itemId);
    if (hold) options.commit(hold.row);
  }

  function hold(row: Item): void {
    const existing = holds.get(row.id);
    if (!existing) {
      holds.set(row.id, { row, phase: 'draining', presenter: null, ceiling: null });
      return;
    }
    existing.row = row;
    if (existing.phase === 'presenting') publishPresenting();
  }

  function patch(itemId: string, fields: RowPromotionPatch): boolean {
    const existing = holds.get(itemId);
    if (!existing) return false;
    const next = { ...existing.row };
    if (fields.rev !== undefined) next.rev = fields.rev;
    if (fields.status !== undefined) next.status = fields.status;
    if (fields.summary !== undefined) next.summary = fields.summary;
    if (fields.meta !== undefined) next.meta = fields.meta;
    if (fields.decision !== undefined) next.decision = fields.decision;
    if (fields.updatedAt !== undefined) next.updatedAt = fields.updatedAt;
    existing.row = next;
    if (existing.phase === 'presenting') publishPresenting();
    return true;
  }

  function completeAll(): void {
    for (const itemId of Array.from(holds.keys())) complete(itemId);
  }

  function markDrained(itemId: string): void {
    const existing = holds.get(itemId);
    if (existing?.phase === 'draining') existing.phase = 'drained';
  }

  function presentDrained(): void {
    if (holds.size === 0) return;
    // A commit can re-enter through the pane's upsert; walk a snapshot.
    for (const [itemId, existing] of Array.from(holds)) {
      if (existing.phase !== 'drained' || holds.get(itemId) !== existing) continue;
      let taker: RowPromotionPresenter | null = null;
      for (const presenter of presenters) {
        if (presenter(itemId, existing.row)) {
          taker = presenter;
          break;
        }
      }
      if (taker === null) {
        complete(itemId);
        continue;
      }
      existing.phase = 'presenting';
      existing.presenter = taker;
      existing.ceiling = startTimer(() => {
        existing.ceiling = null;
        if (holds.get(itemId) === existing) complete(itemId);
      }, PROMOTION_PRESENT_CEILING_MS);
      publishPresenting();
    }
  }

  function drop(itemId: string): void {
    release(itemId);
  }

  function clear(): void {
    for (const existing of holds.values()) {
      if (existing.ceiling !== null) stopTimer(existing.ceiling);
    }
    holds.clear();
    if (presenting.length > 0) presenting = [];
  }

  function registerPresenter(presenter: RowPromotionPresenter): () => void {
    presenters.add(presenter);
    return () => {
      if (!presenters.delete(presenter)) return;
      for (const [itemId, existing] of Array.from(holds)) {
        if (existing.presenter === presenter) complete(itemId);
      }
    };
  }

  return {
    has: (itemId) => holds.has(itemId),
    get size() {
      return holds.size;
    },
    ids: () => holds.keys(),
    phase: (itemId) => holds.get(itemId)?.phase,
    hold,
    patch,
    markDrained,
    presentDrained,
    complete,
    completeAll,
    drop,
    clear,
    get presenting() {
      return presenting;
    },
    registerPresenter,
  };
}
