import type { Locator, Page } from '@playwright/test';
import { expect } from '@playwright/test';

/** The title floor a narrow chat header keeps: 8rem. */
export const TITLE_FLOOR_PX = 128;

/** The pane strip does not scroll sideways. */
export async function expectStripFits(page: Page): Promise<void> {
  const strip = page.locator('section[data-pane-id]').first().locator('xpath=..');
  await expect.poll(() => strip.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
}

/**
 * Each pane's title is whole, or at least the floor wide and truncated with
 * an ellipsis, or under compact faded on the edge that has text past it; a header under 36rem leaves the
 * project crumb out; and no header overflows its pane.
 */
export async function expectHeadersShareTheWidth(panes: Locator[]): Promise<void> {
  for (const pane of panes) {
    const title = pane.getByTestId('chat-header-title');
    const geometry = await title.evaluate((el) => {
      // Under compact the text sits in a swipeable scroller whose mask
      // fades the edge that has text past it.
      const scroller = el.querySelector<HTMLElement>('.pane-title-fade');
      const text = scroller ?? el;
      const style = getComputedStyle(text);
      // The fade spans about two characters (2em) at the title's size; a
      // narrower one reads as a letter cut in half.
      const raw = style.getPropertyValue('--fade-right').trim();
      const fontPx = parseFloat(style.fontSize);
      const fadeRight = raw.endsWith('em') ? parseFloat(raw) * fontPx : parseFloat(raw) || 0;
      const mask = style.getPropertyValue('mask-image') || style.getPropertyValue('-webkit-mask-image');
      return {
        client: el.clientWidth,
        overflows: text.scrollWidth > text.clientWidth + 1,
        cut: scroller
          ? scroller.scrollLeft <= 1 && fadeRight >= 2 * fontPx - 0.5 && mask.includes('gradient')
          : style.textOverflow === 'ellipsis' && style.overflowX === 'hidden',
      };
    });
    if (geometry.overflows) {
      expect(geometry.client, 'a truncated title keeps the floor').toBeGreaterThanOrEqual(TITLE_FLOOR_PX);
      expect(geometry.cut, 'a truncated title ends in an ellipsis or a fade').toBe(true);
    }
    const header = pane.getByTestId('chat-header');
    // The compact facts line carries the project on a row of its own.
    const desktop = (await pane.getByTestId('chat-header-title-row').count()) === 0;
    if (desktop && await header.evaluate((el) => el.clientWidth < 576)) {
      await expect(pane.getByTestId('chat-header-project'), 'a narrow header leaves the crumb out').toBeHidden();
    }
    const paneBox = (await pane.boundingBox())!;
    const overflow = await header.evaluate((el) => {
      const right = Math.max(...[...el.querySelectorAll('*')].map((child) => child.getBoundingClientRect().right));
      return right;
    });
    expect(overflow, 'nothing in the header runs past the pane').toBeLessThanOrEqual(paneBox.x + paneBox.width + 1);
  }
}

export const LEAD = 'Release lead for the quarterly platform migration';
export const WORKER = 'Changelog worker for the quarterly platform migration';

export function pane(page: Page, kind: string): Locator {
  return page.locator(`section[data-pane-kind="${kind}"]`);
}
