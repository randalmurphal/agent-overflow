// Probes for a sidebar that must stay still while a paired computer is
// unreachable: the closed Projects section gains no status line and runs no
// animation, and the Projects menu is where the computer's state is said.
import { expect, type Page } from '@playwright/test';
import { stepHeldClockUntil, type HeldPageClock } from '../src/page-clock.js';
import { RECONNECT_FLOOR_MS, RECONNECT_MAX_REMOTE_MS } from '../../frontend/src/lib/pageTiming.ts';

export interface ProjectsQuiet {
  samples: number;
  /** Each catalog status line seen, as `<backend>: <text>`. */
  statusLines: string[];
  /** Each running animation seen, as `<name> on <target>`. */
  animations: string[];
  /** The most thread rows dimmed as unreachable at once. */
  dimmed: number;
}

/**
 * Sample the Projects section at every change of the page and every
 * animation start while `through` runs, and return what was seen once it
 * settles. A status line, an animation or a dimmed row starts with a DOM
 * change or an animation event, so this sees each one without sampling
 * every frame: a page clock run of a minute costs as many samples as the
 * page changed, not one per frame. `through` is the barrier that ends the
 * watch: an event the page shows after the moment a status line, an
 * animation or a dimmed row would have appeared. The pointer moves off the
 * sidebar first and the hover transitions that starts are waited out, so
 * what remains is what the page runs on its own.
 */
export async function watchProjectsQuiet(page: Page, through: () => Promise<unknown>): Promise<ProjectsQuiet> {
  const viewport = page.viewportSize();
  if (viewport === null) throw new Error('the page has no viewport');
  await page.mouse.move(viewport.width - 2, viewport.height - 2);
  await page.evaluate(() => Promise.all(
    document.getAnimations().filter((a) => a instanceof CSSTransition).map((a) => a.finished.catch(() => undefined)),
  ));
  await page.evaluate(() => {
    const probe = {
      seen: { samples: 0, statusLines: [] as string[], animations: [] as string[], dimmed: 0 },
      stop: () => {},
      error: '',
    };
    (window as unknown as { __projectsQuiet: typeof probe }).__projectsQuiet = probe;
    const sample = () => {
      const section = document.querySelector('[data-testid="sidebar-projects-section"]');
      if (section === null) {
        probe.error = 'the Projects section is not mounted';
        return;
      }
      const seen = probe.seen;
      seen.samples++;
      for (const line of section.querySelectorAll('[data-testid="sidebar-catalog-status"] > *')) {
        const text = `${line.getAttribute('data-backend')}: ${line.textContent?.trim()}`;
        if (!seen.statusLines.includes(text)) seen.statusLines.push(text);
      }
      for (const animation of section.getAnimations({ subtree: true })) {
        if (animation.playState !== 'running') continue;
        const target = (animation.effect as KeyframeEffect | null)?.target as Element | null;
        const name = animation instanceof CSSAnimation ? animation.animationName
          : animation instanceof CSSTransition ? animation.transitionProperty : animation.constructor.name;
        const text = `${name} on ${target?.getAttribute('data-testid') ?? target?.tagName ?? '?'}`;
        if (!seen.animations.includes(text)) seen.animations.push(text);
      }
      seen.dimmed = Math.max(seen.dimmed, section.querySelectorAll('[data-machine-unreachable="true"]').length);
    };
    // A script animation a change starts (a Svelte transition) begins in the
    // frame after it, so a change is sampled again then.
    let frame = 0;
    const changed = () => {
      sample();
      if (frame === 0) frame = requestAnimationFrame(() => { frame = 0; sample(); });
    };
    const observer = new MutationObserver(changed);
    observer.observe(document.body, { subtree: true, childList: true, attributes: true, characterData: true });
    for (const type of ['animationstart', 'transitionrun']) document.addEventListener(type, sample, true);
    probe.stop = () => {
      observer.disconnect();
      if (frame !== 0) cancelAnimationFrame(frame);
      for (const type of ['animationstart', 'transitionrun']) document.removeEventListener(type, sample, true);
    };
    sample();
  });
  const stop = () => page.evaluate(() => {
    const p = (window as unknown as { __projectsQuiet: { seen: ProjectsQuiet; stop(): void; error: string } }).__projectsQuiet;
    p.stop();
    return { seen: p.seen, error: p.error };
  });
  try {
    await through();
  } catch (error) {
    // The barrier's failure is the result; a page it took down cannot stop.
    await stop().catch(() => undefined);
    throw error;
  }
  const probe = await stop();
  if (probe.error !== '') throw new Error(probe.error);
  return probe.seen;
}

const RECONNECT_COUNTDOWN = /^Reconnecting in (\d+)s/;

// An attempt in flight can wait on the page's own timers, so the held clock
// is stepped while the spec waits for the ladder to arm or settle. A step
// shorter than the shortest rung cannot fire the attempt armed after it.
const LADDER_STEP_MS = RECONNECT_FLOOR_MS / 2;
// No attempt outlasts the longest rung of the ladder.
const LADDER_BUDGET_MS = RECONNECT_MAX_REMOTE_MS;

/**
 * Whether this page's reconnect ladder has an attempt armed now: the
 * banner's countdown, which shows only while an attempt waits on its rung
 * and none is in flight.
 */
export function reconnectArmed(page: Page): Promise<boolean> {
  return page.getByTestId('transport-status-banner').getByText(RECONNECT_COUNTDOWN).isVisible();
}

/** Wait for this page's reconnect ladder to have an attempt armed, running an attempt in flight to its end. */
export async function awaitReconnectArmed(clock: HeldPageClock): Promise<void> {
  await stepHeldClockUntil(clock, 'an armed reconnect attempt', () => reconnectArmed(clock.page), LADDER_STEP_MS, LADDER_BUDGET_MS);
}

/**
 * Run the reconnect attempt this page's banner counts down to, and return
 * once the page has settled it. The clock is held, so the countdown is read
 * on a still page and nothing fires until the spec moves it. The banner
 * rounds the wait up to whole seconds, so the move reaches the attempt; the
 * ladder arms the one after it only once this one ends, after the move. The
 * pass ends when the page has settled the attempt, stepping the clock for
 * the attempt's own timers: a new countdown after the attempt cleared the
 * old one (the next attempt armed), or the banner reporting a connection or
 * gone.
 */
export async function runNextReconnect(clock: HeldPageClock): Promise<void> {
  const { page } = clock;
  const countdown = page.getByTestId('transport-status-banner').getByText(RECONNECT_COUNTDOWN);
  await awaitReconnectArmed(clock);
  const seconds = Number(RECONNECT_COUNTDOWN.exec(await countdown.innerText())?.[1]);
  if (!Number.isInteger(seconds)) throw new Error(`the reconnect countdown reads ${await countdown.innerText()}`);
  await page.evaluate((pattern) => {
    const countdownShown = (banner: Element | null) =>
      banner !== null && [...banner.querySelectorAll('*')].some((el) => new RegExp(pattern).test(el.textContent?.trim() ?? ''));
    const pass = { attempted: false, settled: '' };
    (window as unknown as { __reconnectPass: typeof pass }).__reconnectPass = pass;
    const read = () => {
      const banner = document.querySelector('[data-testid="transport-status-banner"]');
      if (!pass.attempted) {
        pass.attempted = !countdownShown(banner);
        if (!pass.attempted) return;
      }
      if (banner === null) pass.settled = 'gone';
      else if (banner.getAttribute('data-status') === 'connected') pass.settled = 'connected';
      else if (countdownShown(banner)) pass.settled = 'armed';
      if (pass.settled !== '') observer.disconnect();
    };
    const observer = new MutationObserver(read);
    observer.observe(document.body, { subtree: true, childList: true, characterData: true, attributes: true });
  }, RECONNECT_COUNTDOWN.source);
  await page.clock.fastForward(seconds * 1000);
  await stepHeldClockUntil(clock, 'the page settling its reconnect attempt',
    () => page.evaluate(() => (window as unknown as { __reconnectPass: { settled: string } }).__reconnectPass.settled !== ''),
    LADDER_STEP_MS, LADDER_BUDGET_MS);
}

/**
 * Open the Projects menu, expect exactly one computer row whose text matches
 * `status`, expect nothing in the open menu to move, and close it again.
 */
export async function expectProjectsMenuSays(page: Page, status: RegExp): Promise<void> {
  await page.getByRole('button', { name: 'Filter projects by computer' }).click();
  const menu = page.getByRole('menu');
  await expect(menu.getByRole('menuitemcheckbox').filter({ hasText: status })).toHaveCount(1);
  // The popover's own open transition ends; then nothing in it runs.
  await expect.poll(() => menu.evaluate((el) => el.getAnimations({ subtree: true })
    .filter((a) => a.playState === 'running').length)).toBe(0);
  await expect(menu.locator('[aria-busy="true"], [class*="animate-"]')).toHaveCount(0);
  await page.keyboard.press('Escape');
  await expect(menu).toHaveCount(0);
}
