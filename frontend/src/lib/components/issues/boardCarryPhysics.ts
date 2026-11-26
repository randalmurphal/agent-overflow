// How a carried Board card hangs and swings (docs/specs/issues.md,
// "Board"): a flat plate pinned at the point it was grabbed, under
// gravity. The pin follows the pointer; its acceleration and gravity pull
// on the plate's center, so a card grabbed at a corner hangs on a slant
// and swings as the hand moves, and one held by the grab bar at its top
// center hangs level and swings only with the hand. Damping has three
// parts: DRAG grows with spin speed; FRIC is strong on a slow wobble and
// levels off above W0, so a fast swing carries on; CALM applies only while
// the hand is slower than about VCALM px/s, so the card stays lively while
// it moves and stops swinging soon after the hand pauses. The constants
// are the mockup's (docs/specs/issues-mockup.html). Pure: the carry
// (boardCarry.ts) owns the frame loop and the one transform it writes.

/** Gravity px/s², damping 1/s and rad/s², speeds rad/s and px/s, seconds
 * of hand smoothing, the most pin acceleration taken px/s². */
export const PLATE = { G: 9000, DRAG: 1.8, FRIC: 9, W0: 2, CALM: 10, VCALM: 100, SMOOTH: 0.03, AMAX: 90000 } as const;

/** The longest step taken: a frame after a stall moves no further. */
const MAX_DT = 0.034;
const SUBSTEPS = 4;

/** A plate pinned at (gx, gy) of a w by h card. */
export interface Plate {
  /** The vector from the pin to the card's center at rest. */
  readonly rx: number;
  readonly ry: number;
  /** Moment of inertia about the pin, per unit mass. */
  readonly inertia: number;
  /** Angle (rad) and spin (rad/s). */
  ang: number;
  w: number;
  /** The pin's smoothed velocity, px/s. */
  vx: number;
  vy: number;
  /** The pin's last position. */
  px: number;
  py: number;
}

export function plateAt(width: number, height: number, gx: number, gy: number, px: number, py: number): Plate {
  const rx = width / 2 - gx;
  const ry = height / 2 - gy;
  return { rx, ry, inertia: (width * width + height * height) / 12 + rx * rx + ry * ry, ang: 0, w: 0, vx: 0, vy: 0, px, py };
}

/** Moves the pin to (px, py) over `dt` seconds and swings the plate;
 * `calm` (reduced motion) keeps it level. */
export function stepPlate(p: Plate, px: number, py: number, dt: number, calm: boolean): void {
  const h = Math.min(MAX_DT, dt) || 0.001;
  const k = Math.min(1, h / PLATE.SMOOTH);
  const nvx = p.vx + ((px - p.px) / h - p.vx) * k;
  const nvy = p.vy + ((py - p.py) / h - p.vy) * k;
  let ax = (nvx - p.vx) / h;
  let ay = (nvy - p.vy) / h;
  const am = Math.hypot(ax, ay);
  if (am > PLATE.AMAX) {
    ax *= PLATE.AMAX / am;
    ay *= PLATE.AMAX / am;
  }
  p.vx = nvx;
  p.vy = nvy;
  p.px = px;
  p.py = py;
  if (calm) {
    p.ang = 0;
    p.w = 0;
    return;
  }
  const still = PLATE.CALM / (1 + (nvx * nvx + nvy * nvy) / (PLATE.VCALM * PLATE.VCALM));
  const sub = h / SUBSTEPS;
  for (let i = 0; i < SUBSTEPS; i += 1) {
    const c = Math.cos(p.ang);
    const s = Math.sin(p.ang);
    const rx = p.rx * c - p.ry * s;
    const ry = p.rx * s + p.ry * c;
    p.w += ((rx * (PLATE.G - ay) + ry * ax) / p.inertia - (PLATE.DRAG + still) * p.w - PLATE.FRIC * Math.tanh(p.w / PLATE.W0)) * sub;
    p.ang += p.w * sub;
  }
}

/** Whether the plate has come to rest at its angle. */
export function plateResting(p: Plate): boolean {
  return Math.abs(p.w) < 0.02 && Math.hypot(p.vx, p.vy) < 1;
}
