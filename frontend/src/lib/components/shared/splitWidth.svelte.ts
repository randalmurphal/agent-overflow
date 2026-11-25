// The width of a list column beside a detail (the Pull requests page, a
// ticket beside the Issues board): the width the reader asked for
// (dragged, keyed, or none yet), remembered in appStorage, and the width
// rendered, which is that request clamped so the detail keeps its room in
// the body as it is now. A body that shrinks narrows the list without
// forgetting the request; it widens back when the body does. The pointer
// and keyboard paths set the request through the same clamp, so the reader
// only ever asks for a width they can see. Each page gives its bounds.

import { appStorageDelete, appStorageGet, appStorageSet } from '../../stores/appStorage';

/** A page's bounds of its list column. */
export interface SplitSpec {
  /** The narrowest list. */
  readonly listMin: number;
  /** The widest list; Infinity for no bound but the detail's room. */
  readonly listMax: number;
  /** The narrowest detail the list leaves beside it. */
  readonly detailMin: number;
  /** The list's width before the reader asks for one, in a body this wide. */
  readonly initial: (bodyWidth: number) => number;
}

/** One arrow key press; Page Up and Page Down move four. */
export const SPLIT_KEY_STEP_PX = 16;

export interface SplitBounds {
  min: number;
  max: number;
}

/**
 * The widths the list may take in a body `bodyWidth` wide: at least
 * `listMin` and at most `listMax`, leaving `detailMin` for the detail. A
 * body too narrow for both splits evenly. An unmeasured body (0) answers
 * the absolute bounds.
 */
export function splitBounds(spec: SplitSpec, bodyWidth: number): SplitBounds {
  if (!(bodyWidth > 0)) return { min: spec.listMin, max: spec.listMax };
  const max = Math.min(spec.listMax, Math.floor(bodyWidth - spec.detailMin));
  if (max >= spec.listMin) return { min: spec.listMin, max };
  const half = Math.floor(bodyWidth / 2);
  return { min: half, max: half };
}

function clamp(px: number, bounds: SplitBounds): number {
  return Math.min(bounds.max, Math.max(bounds.min, Math.round(px)));
}

export class SplitWidth {
  private readonly storageKey: string;
  /** The width asked for, as remembered; null before the reader asked. */
  requested: number | null = $state(null);
  /** The body the list and the detail share, as measured; 0 before. */
  paneWidth = $state(0);
  readonly bounds: SplitBounds;
  /** The width rendered. */
  readonly width: number;

  constructor(storageKey: string, spec: SplitSpec) {
    this.storageKey = storageKey;
    this.bounds = $derived(splitBounds(spec, this.paneWidth));
    this.width = $derived(clamp(this.requested ?? spec.initial(this.paneWidth), this.bounds));
    const raw = Number(appStorageGet(storageKey) ?? Number.NaN);
    if (Number.isFinite(raw) && raw > 0) this.requested = clamp(raw, splitBounds(spec, 0));
  }

  /** Asks for `px`, as far as the body allows now; a drag remembers it
   * once, when it ends. */
  set(px: number, remember = true): void {
    this.requested = clamp(px, this.bounds);
    if (remember) this.remember();
  }

  /** Back to the page's initial width, which follows the body again. */
  reset(): void {
    this.requested = null;
    this.remember();
  }

  remember(): void {
    if (this.requested === null) appStorageDelete(this.storageKey);
    else appStorageSet(this.storageKey, String(this.requested));
  }
}
