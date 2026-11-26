// Carrying a ticket with a mouse or pen between lists (docs/specs/issues.md,
// "Board" and "Backlog and sprint planning"), loaded on the first press of
// a card or a row. A list is a Board column (`column:<i>`) or a Backlog
// section (`sprint:<id>`, `backlog`). The screen arms a press and starts a
// carry once the pointer has moved far enough; a press that does not move
// is the click that opens the ticket. Under compact there is no carry.
//
// Cost while carried, per animation frame: the plate's step
// (boardCarryPhysics.ts), the screen's hit test (`CarryLayout.at`, from
// geometry it read at pick-up), its edge scroll, and one transform written
// to the copy on `document.body`. The screen is told only when the list
// under the copy or the slot in it changes, so nothing it draws renders
// per pointer move. The frame loop ends when the copy lands or the carry
// is cancelled (Escape, a lost pointer, the window losing focus). A release
// counts only where it is let go over a list's rows as they are on screen
// then (`document.elementFromPoint`, the closest `data-carry-list`);
// anywhere else the copy goes home and nothing is sent.

import { motionReduced } from '../../utils/reducedMotion';
import { plateAt, stepPlate, type Plate } from './boardCarryPhysics';

/** How near an edge the pointer scrolls, and how fast at the edge. */
const EDGE_PX = 40;
const EDGE_SPEED = 900;
/** The longest the drop's glide runs. */
const LAND_MS = 400;

/** The attribute naming the list an element holds the rows of: each
 * screen marks its lists' rows with it. Empty, it marks an area whose
 * list is the one the layout answers there: the Backlog's one scroller,
 * whose rows slide aside to open a gap the pointer can be over. */
const CARRY_LIST_ATTR = 'data-carry-list';

/** Where a carried ticket would go: a list, and the place in it (an index
 * into its shown rows, the carried one's own place included), null where
 * the list takes no place. */
export interface CarrySpot {
  readonly list: string;
  readonly slot: number | null;
}

/** How a screen lays out its lists for a carry, read at pick-up. */
export interface CarryLayout {
  /** Where the copy would go with the pointer at (x, y); null outside
   * every list. */
  at(x: number, y: number): CarrySpot | null;
  /** Scrolls what is near an edge, `dt` seconds' worth, with the pointer at
   * (x, y) over `over`. */
  edgeScroll(x: number, y: number, dt: number, over: CarrySpot | null): void;
}

/** What a screen gives a carry. */
export interface CarryHost {
  readonly layout: CarryLayout;
  /** The list the ticket came from. */
  readonly from: string;
  /** The spot under the copy changed; null outside every list. */
  over(spot: CarrySpot | null): void;
  /** The copy was let go over `spot` (null: over no list). Answers where
   * the ticket now shows, for the copy to glide to, or null to put it down
   * at once. */
  drop(spot: CarrySpot | null, at: { x: number; y: number }): Promise<DOMRect | null>;
  /** The carry ended, dropped or cancelled. */
  end(): void;
}

export interface Carry {
  move(x: number, y: number): void;
  /** Let go at (x, y). */
  up(x: number, y: number): void;
  cancel(): void;
}

/** How the copy hangs: `grab` pins its top center (a press on the Board
 * card's grab bar), `flat` keeps it level and still wherever it was pressed
 * (a Backlog row). */
export interface CarryHold {
  readonly grab: boolean;
  readonly flat: boolean;
}

/** The speed an edge scrolls at with the pointer at `p` between `lo` and
 * `hi`: negative near `lo`, positive near `hi`, else 0. */
export function edgeSpeed(p: number, lo: number, hi: number): number {
  if (p < lo + EDGE_PX) return -EDGE_SPEED * (1 - (p - lo) / EDGE_PX);
  if (p > hi - EDGE_PX) return EDGE_SPEED * (1 - (hi - p) / EDGE_PX);
  return 0;
}

/** A Board column as a carry tests it, read once at pick-up. */
export interface CarryColumn {
  readonly list: string;
  readonly cardsRect: DOMRect;
  /** The slot among the column's cards at `clientY`; null outside them. */
  slotAt(clientY: number): number | null;
  /** Scrolls the column's cards by `dy` px. */
  scrollBy(dy: number): void;
}

/** Columns side by side in a sideways `scroller`, each scrolling its own
 * cards: a place is taken only in the column `from`, and near an edge the
 * board scrolls sideways and that column up or down. */
export function columnsLayout(scroller: HTMLElement, columns: readonly CarryColumn[], from: string): CarryLayout {
  const scroll0 = scroller.scrollLeft;
  const box = scroller.getBoundingClientRect();
  const own = columns.find((c) => c.list === from);
  return {
    at(x, y) {
      const shift = scroller.scrollLeft - scroll0;
      const col = columns.find((c) => x >= c.cardsRect.left - shift && x <= c.cardsRect.right - shift && y >= c.cardsRect.top && y <= c.cardsRect.bottom);
      if (!col) return null;
      return { list: col.list, slot: col === own ? col.slotAt(y) : null };
    },
    edgeScroll(x, y, dt, over) {
      scroller.scrollLeft += edgeSpeed(x, box.left, box.right) * dt;
      if (!own || over?.list !== from) return;
      own.scrollBy(edgeSpeed(y, own.cardsRect.top, own.cardsRect.bottom) * dt);
    },
  };
}

/** Picks up `card`, pressed at (sx, sy) and now at (x, y). */
export function startCarry(card: HTMLElement, sx: number, sy: number, x: number, y: number, hold: CarryHold, host: CarryHost): Carry {
  const r = card.getBoundingClientRect();
  const calm = motionReduced();
  const gx = hold.grab ? r.width / 2 : sx - r.left;
  const gy = hold.grab ? 4 : sy - r.top;
  // The pointer's offset from the pin: none unless the grab bar was used,
  // which pins the top center wherever on the bar it was pressed.
  const hx = sx - (r.left + gx);
  const hy = sy - (r.top + gy);
  const plate: Plate = plateAt(r.width, r.height, gx, gy, sx, sy);
  const copy = card.cloneNode(true) as HTMLElement;
  copy.removeAttribute('data-testid');
  copy.setAttribute('data-testid', 'issues-card-carried');
  copy.setAttribute('aria-hidden', 'true');
  Object.assign(copy.style, {
    position: 'fixed', left: `${r.left}px`, top: `${r.top}px`, width: `${r.width}px`, margin: '0', zIndex: '90',
    pointerEvents: 'none', transformOrigin: `${gx}px ${gy}px`, willChange: 'transform',
  });
  copy.classList.add('shadow-sheet');
  document.body.appendChild(copy);
  document.body.classList.add('cursor-grabbing');

  let px = x;
  let py = y;
  let spot: CarrySpot | null = host.layout.at(x, y);
  let last = performance.now();
  let frame = 0;
  let done = false;

  /** The list whose rows are under (x, y) on screen now: empty for an
   * area whose list is the layout's; null over none. */
  function listUnder(x: number, y: number): string | null {
    return document.elementFromPoint(x, y)?.closest(`[${CARRY_LIST_ATTR}]`)?.getAttribute(CARRY_LIST_ATTR) ?? null;
  }

  function place(tx: number, ty: number, ang: number): void {
    copy.style.transform = `translate3d(${(tx - r.left).toFixed(1)}px, ${(ty - r.top).toFixed(1)}px, 0) rotate(${ang.toFixed(4)}rad)`;
  }

  function sameSpot(a: CarrySpot | null, b: CarrySpot | null): boolean {
    return a === b || (!!a && !!b && a.list === b.list && a.slot === b.slot);
  }

  function tick(now: number): void {
    if (done) return;
    const dt = Math.min(0.034, (now - last) / 1000);
    last = now;
    stepPlate(plate, px - hx, py - hy, dt, calm || hold.flat);
    place(px - hx - gx, py - hy - gy, plate.ang);
    host.layout.edgeScroll(px, py, dt, spot);
    const at = host.layout.at(px, py);
    if (!sameSpot(at, spot)) {
      spot = at;
      host.over(at);
    }
    frame = requestAnimationFrame(tick);
  }

  function onKey(event: KeyboardEvent): void {
    if (event.key !== 'Escape') return;
    event.preventDefault();
    event.stopPropagation();
    carry.cancel();
  }
  const onBlur = (): void => carry.cancel();
  window.addEventListener('keydown', onKey, true);
  window.addEventListener('blur', onBlur);

  function finish(): void {
    done = true;
    cancelAnimationFrame(frame);
    window.removeEventListener('keydown', onKey, true);
    window.removeEventListener('blur', onBlur);
    document.body.classList.remove('cursor-grabbing');
    host.over(null);
  }

  /** Glides the copy to `to` and upright, then removes it. */
  function land(to: DOMRect | null): void {
    if (!to || calm) {
      copy.remove();
      host.end();
      return;
    }
    const fromX = px - hx - gx;
    const fromY = py - hy - gy;
    const fromAng = plate.ang;
    const t0 = performance.now();
    const glide = (now: number) => {
      const k = Math.min(1, (now - t0) / LAND_MS);
      const e = 1 - (1 - k) ** 3;
      place(fromX + (to.left - fromX) * e, fromY + (to.top - fromY) * e, fromAng * (1 - e));
      if (k < 1) requestAnimationFrame(glide);
      else {
        copy.remove();
        host.end();
      }
    };
    requestAnimationFrame(glide);
  }

  const carry: Carry = {
    move(nx, ny) {
      px = nx;
      py = ny;
    },
    up(x, y) {
      if (done) return;
      px = x;
      py = y;
      const list = listUnder(x, y);
      const at = host.layout.at(x, y);
      const released = list === null ? null : list === '' || at?.list === list ? at : { list, slot: null };
      finish();
      void host.drop(released, { x: px, y: py }).then(land, () => land(null));
    },
    cancel() {
      if (done) return;
      finish();
      land(r);
    },
  };
  place(x - hx - gx, y - hy - gy, 0);
  frame = requestAnimationFrame(tick);
  host.over(spot);
  return carry;
}
