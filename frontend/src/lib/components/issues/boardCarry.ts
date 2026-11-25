// Carrying a Board card with a mouse or pen (docs/specs/issues.md,
// "Board"), loaded on the first press of a card. The Board screen
// (BoardScreen.svelte) arms a press and starts a carry once the pointer
// has moved far enough; a press that does not move is the click that
// opens the ticket. Under compact there is no carry: a card moves by its
// status mark.
//
// Cost while carried, per animation frame: the plate's step
// (boardCarryPhysics.ts), the hit test against column rectangles read once
// at pick-up (offset by the board's sideways scroll), and one transform
// written to the card's copy on `document.body`. The host is told only
// when the column under the card or the drop slot in its own column
// changes, so no card of the board renders while it is carried. Near an
// edge the board scrolls sideways and the card's own column up or down.
// The frame loop ends when the card lands or the carry is cancelled
// (Escape, a lost pointer, the window losing focus).

import { motionReduced } from '../../utils/reducedMotion';
import { plateAt, stepPlate, type Plate } from './boardCarryPhysics';

/** How near an edge the pointer scrolls, and how fast at the edge. */
const EDGE_PX = 40;
const EDGE_SPEED = 900;
/** The longest the drop's glide runs. */
const LAND_MS = 400;

/** A column as the carry tests it. */
export interface CarryColumn {
  readonly rect: DOMRect;
  /** Where the card would go among the column's cards at `clientY` (an
   * index into its shown rows, the carried card's own place included),
   * and where to draw the slot; null outside its cards. */
  slotAt(clientY: number): number | null;
  /** Scrolls the column's cards by `dy` px. */
  scrollBy(dy: number): void;
  readonly cardsRect: DOMRect;
}

/** What the Board screen gives a carry. */
export interface CarryHost {
  /** The columns, read once at pick-up. */
  readonly columns: readonly CarryColumn[];
  /** The board's sideways scroller. */
  readonly scroller: HTMLElement;
  /** The column the card came from. */
  readonly from: number;
  /** The column under the card changed; null outside every column. */
  over(column: number | null): void;
  /** The slot in the card's own column changed; null for none. */
  slot(index: number | null): void;
  /** The card was let go over `column` (null: over none) at `slot` in its
   * own column. Answers where the card now shows, for the copy to glide
   * to, or null to put it down at once. */
  drop(column: number | null, slot: number | null, at: { x: number; y: number }): Promise<DOMRect | null>;
  /** The carry ended, dropped or cancelled. */
  end(): void;
}

export interface Carry {
  move(x: number, y: number): void;
  up(): void;
  cancel(): void;
}

/** Picks up `card`, pressed at (sx, sy) and now at (x, y); `level` is a
 * press on the grab bar, which pins the card's top center. */
export function startCarry(card: HTMLElement, sx: number, sy: number, x: number, y: number, level: boolean, host: CarryHost): Carry {
  const r = card.getBoundingClientRect();
  const calm = motionReduced();
  const gx = level ? r.width / 2 : sx - r.left;
  const gy = level ? 4 : sy - r.top;
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
  copy.classList.add('shadow-lg');
  document.body.appendChild(copy);
  document.body.classList.add('cursor-grabbing');

  const scroll0 = host.scroller.scrollLeft;
  const box = host.scroller.getBoundingClientRect();
  let px = x;
  let py = y;
  let over: number | null = host.from;
  let slot: number | null = null;
  let last = performance.now();
  let frame = 0;
  let done = false;

  function columnAt(): number | null {
    const shift = host.scroller.scrollLeft - scroll0;
    const i = host.columns.findIndex((c) => px >= c.rect.left - shift && px <= c.rect.right - shift);
    return i < 0 ? null : i;
  }

  function edgeScroll(dt: number): void {
    if (px < box.left + EDGE_PX) host.scroller.scrollLeft -= EDGE_SPEED * dt * (1 - (px - box.left) / EDGE_PX);
    else if (px > box.right - EDGE_PX) host.scroller.scrollLeft += EDGE_SPEED * dt * (1 - (box.right - px) / EDGE_PX);
    if (over !== host.from) return;
    const cards = host.columns[host.from].cardsRect;
    if (py < cards.top + EDGE_PX) host.columns[host.from].scrollBy(-EDGE_SPEED * dt * (1 - (py - cards.top) / EDGE_PX));
    else if (py > cards.bottom - EDGE_PX) host.columns[host.from].scrollBy(EDGE_SPEED * dt * (1 - (cards.bottom - py) / EDGE_PX));
  }

  function place(tx: number, ty: number, ang: number): void {
    copy.style.transform = `translate3d(${(tx - r.left).toFixed(1)}px, ${(ty - r.top).toFixed(1)}px, 0) rotate(${ang.toFixed(4)}rad)`;
  }

  function tick(now: number): void {
    if (done) return;
    const dt = Math.min(0.034, (now - last) / 1000);
    last = now;
    stepPlate(plate, px - hx, py - hy, dt, calm);
    place(px - hx - gx, py - hy - gy, plate.ang);
    edgeScroll(dt);
    const at = columnAt();
    if (at !== over) {
      over = at;
      host.over(at);
    }
    const nextSlot = at === host.from ? host.columns[host.from].slotAt(py) : null;
    if (nextSlot !== slot) {
      slot = nextSlot;
      host.slot(nextSlot);
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
    host.slot(null);
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
    up() {
      if (done) return;
      const column = columnAt();
      const at = column === host.from ? host.columns[host.from].slotAt(py) : null;
      finish();
      void host.drop(column, at, { x: px, y: py }).then(land, () => land(null));
    },
    cancel() {
      if (done) return;
      finish();
      land(r);
    },
  };
  place(x - hx - gx, y - hy - gy, 0);
  frame = requestAnimationFrame(tick);
  host.over(host.from);
  return carry;
}
