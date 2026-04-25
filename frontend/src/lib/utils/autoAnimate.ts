// Tiny Svelte action wrapper around @formkit/auto-animate. Use as
// `<div use:autoAnimate>` on any container whose direct children
// reorder, mount, or unmount — auto-animate FLIPs them with a
// matched-duration transition so the user sees motion instead of a
// hard cut.
//
// Defaults match forge / t3-code: 180ms ease-out. Pass options as the
// action argument to override per-call site.
//
// Returning `{ destroy }` is load-bearing: the underlying lib registers
// a MutationObserver and stamps WeakMap entries keyed by the parent
// element. Without an explicit teardown, the observer stays alive
// across the lifetime of the page and the parent element can't be
// garbage-collected when its host component unmounts (sidebar HMR,
// project list filtering, project collapse).

import type { ActionReturn } from 'svelte/action';
import autoAnimateLib, { type AutoAnimateOptions } from '@formkit/auto-animate';

const DEFAULT_OPTIONS: Partial<AutoAnimateOptions> = {
  duration: 180,
  easing: 'ease-out',
};

export function autoAnimate(
  node: HTMLElement,
  options: Partial<AutoAnimateOptions> = {},
): ActionReturn<Partial<AutoAnimateOptions>> {
  const controller = autoAnimateLib(node, { ...DEFAULT_OPTIONS, ...options });
  return {
    destroy: () => {
      controller.destroy?.();
    },
  };
}
