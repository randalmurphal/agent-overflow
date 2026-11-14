// The Pull requests page's list column width: the width the reader asked
// for (dragged, keyed, or the default), remembered in appStorage, and the
// width rendered, which is that request clamped so the detail beside the
// list keeps a usable width in the pane as it is now. A pane that shrinks
// narrows the list without forgetting the request; it widens back when
// the pane does. The pointer and keyboard paths set the request through
// the same clamp, so the reader only ever asks for a width they can see.

import { appStorageGet, appStorageSet } from '../../stores/appStorage';

export const LIST_MIN_PX = 280;
export const LIST_MAX_PX = 720;
export const LIST_DEFAULT_PX = 400;
/** The narrowest detail the list leaves beside it. */
export const DETAIL_MIN_PX = 320;
/** One arrow key press; Page Up and Page Down move four. */
export const LIST_KEY_STEP_PX = 16;

export interface ListWidthBounds {
  min: number;
  max: number;
}

/**
 * The widths the list may take in a body `paneWidth` wide: at least
 * LIST_MIN_PX and at most LIST_MAX_PX, leaving DETAIL_MIN_PX for the
 * detail. A body too narrow for both splits evenly. An unmeasured body
 * (0) answers the absolute bounds.
 */
export function listWidthBounds(paneWidth: number): ListWidthBounds {
  if (!(paneWidth > 0)) return { min: LIST_MIN_PX, max: LIST_MAX_PX };
  const max = Math.min(LIST_MAX_PX, Math.floor(paneWidth - DETAIL_MIN_PX));
  if (max >= LIST_MIN_PX) return { min: LIST_MIN_PX, max };
  const half = Math.floor(paneWidth / 2);
  return { min: half, max: half };
}

export function clampListWidth(px: number, bounds: ListWidthBounds): number {
  return Math.min(bounds.max, Math.max(bounds.min, Math.round(px)));
}

export class ListColumnWidth {
  /** The width asked for, as remembered. */
  requested = $state(LIST_DEFAULT_PX);
  /** The body the list and the detail share, as measured; 0 before. */
  paneWidth = $state(0);
  readonly bounds: ListWidthBounds = $derived(listWidthBounds(this.paneWidth));
  /** The width rendered. */
  readonly width: number = $derived(clampListWidth(this.requested, this.bounds));

  constructor(private readonly storageKey: string) {
    const raw = Number(appStorageGet(storageKey));
    if (Number.isFinite(raw) && raw > 0) this.requested = clampListWidth(raw, listWidthBounds(0));
  }

  /** Asks for `px`, as far as the pane allows now; a drag remembers it
   * once, when it ends. */
  set(px: number, remember = true): void {
    this.requested = clampListWidth(px, this.bounds);
    if (remember) this.remember();
  }

  reset(): void {
    this.requested = LIST_DEFAULT_PX;
    this.remember();
  }

  remember(): void {
    appStorageSet(this.storageKey, String(this.requested));
  }
}
