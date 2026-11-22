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
 * Sample the Projects section every 250ms for `ms`. The pointer moves off
 * the sidebar first and the hover transitions that starts are waited out,
 * so what remains is what the page runs on its own.
 */
export async function watchProjectsQuiet(page: Page, ms: number): Promise<ProjectsQuiet> {
  const viewport = page.viewportSize();
  if (viewport === null) throw new Error('the page has no viewport');
  await page.mouse.move(viewport.width - 2, viewport.height - 2);
  await page.evaluate(() => Promise.all(
    document.getAnimations().filter((a) => a instanceof CSSTransition).map((a) => a.finished.catch(() => undefined)),
  ));
  return page.evaluate(async (duration) => {
    const seen = { samples: 0, statusLines: [] as string[], animations: [] as string[], dimmed: 0 };
    const end = performance.now() + duration;
    while (performance.now() < end) {
      const section = document.querySelector('[data-testid="sidebar-projects-section"]');
      if (section === null) throw new Error('the Projects section is not mounted');
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
      await new Promise((resolve) => setTimeout(resolve, 250));
    }
    return seen;
  }, ms);
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
