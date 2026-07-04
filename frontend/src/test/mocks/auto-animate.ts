// Test stub for @formkit/auto-animate (aliased in vitest.config.ts).
//
// The real library's element poller registers untracked node timers: a
// randomly-offset setTimeout that installs a 2s setInterval calling
// requestAnimationFrame. Node keeps those timers alive after happy-dom
// tears down a test file's globals, so under a loaded machine the
// interval fires into a dead environment and vitest reports
// "ReferenceError: requestAnimationFrame is not defined" as an
// unhandled error — a suite-order-dependent flake, not a real failure.
// FLIP animations are meaningless in unit tests, so stub the whole
// module instead of racing its timers.

import type { AnimationController, AutoAnimateOptions, AutoAnimationPlugin } from '@formkit/auto-animate';

export type { AnimationController, AutoAnimateOptions, AutoAnimationPlugin };

export default function autoAnimate(
  el: HTMLElement,
  _config?: Partial<AutoAnimateOptions> | AutoAnimationPlugin,
): AnimationController {
  let enabled = true;
  return {
    parent: el,
    enable: () => {
      enabled = true;
    },
    disable: () => {
      enabled = false;
    },
    isEnabled: () => enabled,
    destroy: () => {},
  };
}
