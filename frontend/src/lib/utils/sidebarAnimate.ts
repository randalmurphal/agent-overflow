// Sidebar list motion: svelte-native FLIP + enter/exit for the two keyed
// eaches that used to run @formkit/auto-animate (ProjectList,
// ProjectThreadList).
//
// Why not auto-animate: it is framework-blind. A MutationObserver fires only
// AFTER the DOM changed, when old positions are gone, so the library had to
// track every row's position continuously — a ResizeObserver, a per-element
// IntersectionObserver rebuilt on every move, and a 2s poll per element.
// Measured on an idle app (2026-08-24): 22.5 forced layouts/s, 45 IO
// constructions/s, ~11ms/s of style recalc, for lists that change a few
// times a minute. Svelte's keyed-each reconcile is the one moment old and
// new positions are both knowable; `animate:flip` measures there and the
// idle cost is zero.
//
// The parameters mirror what auto-animate rendered with our options
// (duration 180, easing ease-out — utils/autoAnimate.ts, now deleted):
// - move (FLIP): 180ms ease-out translate — `SIDEBAR_FLIP` below.
// - enter: space appears instantly (siblings FLIP around it), the new row
//   holds invisible at scale .98 through the eased first half of 1.5× the
//   base duration, then fades/scales in — auto-animate's default "add"
//   keyframes ([{.98,0}, {.98,0,offset:.5}, {1,1}], 270ms ease-in).
// - exit: fade to scale .98 over 180ms ease-out. One deliberate divergence:
//   auto-animate yanked the dying row out of flow (position:absolute ghost)
//   so siblings could FLIP into the freed space; svelte keeps it in flow
//   until the transition ends, which would make the rows below SNAP up at
//   detach. The exit therefore also collapses the row's height, so the
//   space closes smoothly in the same 180ms instead.
import { cubicIn, cubicOut } from 'svelte/easing';
import type { TransitionConfig } from 'svelte/transition';

export const SIDEBAR_ANIM_MS = 180;

export const SIDEBAR_FLIP = { duration: SIDEBAR_ANIM_MS, easing: cubicOut };

export function sidebarEnter(node: Element): TransitionConfig {
  void node;
  return {
    duration: SIDEBAR_ANIM_MS * 1.5,
    css: (t) => {
      // t is linear (no `easing` given): apply the ease-in over the whole
      // timeline first, then auto-animate's midpoint hold in progress space.
      const eased = cubicIn(t);
      const p = Math.max(0, (eased - 0.5) * 2);
      return `opacity: ${p}; transform: scale(${0.98 + 0.02 * p});`;
    },
  };
}

export function sidebarExit(node: Element): TransitionConfig {
  const height = (node as HTMLElement).offsetHeight;
  return {
    duration: SIDEBAR_ANIM_MS,
    easing: cubicOut,
    css: (t) =>
      `opacity: ${t}; transform: scale(${0.98 + 0.02 * t}); ` +
      `height: ${(t * height).toFixed(2)}px; min-height: 0; overflow: hidden;`,
  };
}
