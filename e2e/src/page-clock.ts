// A page clock the spec holds: after holdPageClock, page time moves only
// when the spec moves it, so a deadline or a retry ladder the spec crosses is
// placed by page time rather than by how long the steps between took.
import type { Page } from '@playwright/test';

/**
 * Past the page time read, where the hold lands. Playwright's `pauseAt`
 * takes an absolute time and refuses one the running clock has already
 * passed, and a running clock moves on during the call that reads it, so
 * the hold lands this far ahead: one round trip to the page has to fit in
 * it. The jump fires each timer due inside it once, as a `fastForward` does.
 */
export const HOLD_SLACK_MS = 1_000;

/** A page whose installed clock the spec holds; `at` is the page time it stopped at. */
export interface HeldPageClock {
  readonly page: Page;
  readonly at: number;
}

/**
 * Stops the page's running clock and returns where it stopped. The clock
 * must be installed (`page.clock.install`); `page.clock.resume()` lets it run
 * again.
 */
export async function holdPageClock(page: Page): Promise<HeldPageClock> {
  const now = await page.evaluate(() => Date.now());
  const at = now + HOLD_SLACK_MS;
  try {
    await page.clock.pauseAt(at);
  } catch (error) {
    throw new Error(`holding the page clock at ${at}, ${HOLD_SLACK_MS} ms past the page time ${now}, failed; ` +
      `a round trip to the page took longer than that: ${error instanceof Error ? error.message : String(error)}`);
  }
  return { page, at };
}

/** The held page's time now: where it stopped, plus every move the spec made since. */
export function heldPageTime(clock: HeldPageClock): Promise<number> {
  return clock.page.evaluate(() => Date.now());
}

/**
 * Moves the held clock on in steps of `stepMs` until `reached` says the page
 * got where the spec waits for it, checking before each step; fails once
 * `budgetMs` of page time has gone without it. For work the page finishes on
 * its own timers (a probe's grace, a retry) while the spec must not move the
 * clock past what comes after it: a step no longer than the shortest timer
 * that would come after cannot fire it, since a step starts only once a
 * check found the page short of `reached`.
 */
export async function stepHeldClockUntil(
  clock: HeldPageClock,
  what: string,
  reached: () => Promise<boolean>,
  stepMs: number,
  budgetMs: number,
): Promise<void> {
  for (let spent = 0; !(await reached()); spent += stepMs) {
    if (spent >= budgetMs) throw new Error(`${what}: not reached in ${budgetMs} ms of page time`);
    await clock.page.clock.runFor(stepMs);
  }
}
