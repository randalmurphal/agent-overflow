// Probes for a sidebar that must stay still while a paired computer is
// unreachable: the closed Projects section gains no status line and runs no
// animation, and the Projects menu is where the computer's state is said.
import { expect, type Page } from '@playwright/test';

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

/**
 * Run the reconnect attempt this page's banner counts down to: wait for the
 * countdown, the ladder's armed attempt, and move the page clock to it. The
 * banner rounds the wait up to whole seconds, so the move reaches the
 * attempt and stops short of the one after it, which the ladder arms only
 * once this one fails. The page clock must be installed.
 */
export async function runNextReconnect(page: Page): Promise<void> {
  const countdown = page.getByTestId('transport-status-banner').getByText(/^Reconnecting in \d+s/);
  await expect(countdown).toBeVisible();
  const seconds = Number(/(\d+)s/.exec(await countdown.innerText())![1]);
  await page.clock.fastForward(seconds * 1000);
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
