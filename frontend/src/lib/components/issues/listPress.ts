// A press on a Board card or a Backlog row, until it moves far enough to
// pick the item up (listCarry.ts) or ends as the click that opens it. One
// controller per screen (createListPress); the screen says which item a
// press is on and picks it up. What must hold:
// - Only a plain primary press of a mouse or pen starts one, off a card's
//   marks (which open their editors), one at a time, and never under
//   compact; a press with Ctrl or Cmd held is a click with its own meaning.
// - The carry's code loads on the first press, once per page, and a
//   failed load ends the press and says so.
// - The click a carry's release raises opens nothing.
// - A cancel, and the screen going, end the press and cancel its carry.

import type { Carry } from './listCarry';
import { addErrorToast } from '../../stores/toast.svelte';

type CarryModule = typeof import('./listCarry');

let carryModule: Promise<CarryModule> | null = null;
function loadCarry(): Promise<CarryModule> {
  carryModule ??= import('./listCarry');
  return carryModule;
}

/** How far a press moves before it picks the item up. */
const PICK_UP_PX = 5;

/** A press as a screen picks its item up. */
export interface ListPress<T> {
  readonly el: HTMLElement;
  readonly item: T;
  /** Pressed on the item's grip (`data-grab`): it is carried level. */
  readonly level: boolean;
  /** Where the press began. */
  readonly sx: number;
  readonly sy: number;
  /** Where the pointer is now. */
  readonly x: number;
  readonly y: number;
}

export interface ListPressScreen<T> {
  /** Presses take nothing now (compact). */
  off(): boolean;
  /** The item a press on `target` is on, as the screen shows it, or null
   * when it is on none or the item may not be carried now (busy). */
  pressed(target: Element): { readonly el: HTMLElement; readonly item: T } | null;
  /** Picks the item up; null when the screen cannot carry now, which ends
   * the press. */
  pickUp(mod: CarryModule, p: ListPress<T>): Carry | null;
  /** What a toast says when the carry's code did not load. */
  readonly loadFailed: string;
}

interface Pressing<T> extends ListPress<T> {
  readonly id: number;
  x: number;
  y: number;
  carry: Carry | null;
  starting: boolean;
}

/** The screen's press controller: `down` is its list's pointerdown, and
 * `destroy` runs as the screen goes. */
export function createListPress<T>(screen: ListPressScreen<T>): { down(event: PointerEvent): void; destroy(): void } {
  let press: Pressing<T> | null = null;

  function end(): void {
    window.removeEventListener('pointermove', move);
    window.removeEventListener('pointerup', up);
    window.removeEventListener('pointercancel', cancel);
    press = null;
  }

  function down(event: PointerEvent): void {
    if (screen.off() || press || event.button !== 0 || (event.pointerType !== 'mouse' && event.pointerType !== 'pen')) return;
    if (event.ctrlKey || event.metaKey) return;
    const target = event.target instanceof Element ? event.target : null;
    if (!target || target.closest('[data-card-mark]')) return;
    const at = screen.pressed(target);
    if (!at) return;
    void loadCarry().catch(() => {});
    press = {
      id: event.pointerId, el: at.el, item: at.item, level: target.closest('[data-grab]') !== null,
      sx: event.clientX, sy: event.clientY, x: event.clientX, y: event.clientY, carry: null, starting: false,
    };
    window.addEventListener('pointermove', move);
    window.addEventListener('pointerup', up);
    window.addEventListener('pointercancel', cancel);
  }

  function move(event: PointerEvent): void {
    const p = press;
    if (!p || event.pointerId !== p.id) return;
    p.x = event.clientX;
    p.y = event.clientY;
    if (p.carry) {
      p.carry.move(p.x, p.y);
      return;
    }
    if (p.starting || Math.hypot(p.x - p.sx, p.y - p.sy) < PICK_UP_PX) return;
    p.starting = true;
    loadCarry().then(
      (mod) => {
        if (press !== p) return;
        p.carry = screen.pickUp(mod, p);
        if (!p.carry) end();
      },
      (err: unknown) => {
        if (press === p) end();
        addErrorToast(screen.loadFailed, err);
      },
    );
  }

  function up(event: PointerEvent): void {
    const p = press;
    if (!p || event.pointerId !== p.id) return;
    end();
    if (!p.carry) return;
    const swallow = (e: MouseEvent) => {
      e.preventDefault();
      e.stopPropagation();
    };
    window.addEventListener('click', swallow, { capture: true, once: true });
    setTimeout(() => window.removeEventListener('click', swallow, { capture: true }), 0);
    p.carry.up(event.clientX, event.clientY);
  }

  function cancel(event: PointerEvent): void {
    const p = press;
    if (!p || event.pointerId !== p.id) return;
    end();
    p.carry?.cancel();
  }

  return {
    down,
    destroy() {
      const p = press;
      end();
      p?.carry?.cancel();
    },
  };
}
