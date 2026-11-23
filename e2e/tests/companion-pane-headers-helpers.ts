import type { Locator, Page } from '@playwright/test';
import { expect } from '@playwright/test';

/** The title floor a narrow chat header keeps: 8rem. */
export const TITLE_FLOOR_PX = 128;

/** The widest a desktop project crumb gets: 14rem. */
const CRUMB_CAP_PX = 224;

/** The pane strip does not scroll sideways. */
export async function expectStripFits(page: Page): Promise<void> {
  const strip = page.locator('section[data-pane-id]').first().locator('xpath=..');
  await expect.poll(() => strip.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
}

/**
 * Each pane's title is whole, or at least the floor wide and truncated with
 * an ellipsis, or under compact faded on the edge that has text past it; a
 * desktop header leaves the project crumb out under 36rem and otherwise
 * shows the whole project name (a name past the crumb's 14rem cap holds
 * that width and ellipsizes), and keeps its cluster on the title's row
 * exactly when the cluster fits beside the title at its floor; and no header
 * overflows its pane.
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
    if (desktop) {
      const crumb = pane.getByTestId('chat-header-project');
      // The container query reads the header's content box.
      const narrow = await header.evaluate((el) => {
        const style = getComputedStyle(el);
        return el.clientWidth - parseFloat(style.paddingLeft) - parseFloat(style.paddingRight) < 576;
      });
      if (narrow) {
        await expect(crumb, 'a narrow header leaves the crumb out').toBeHidden();
      } else {
        // Every pane these specs open is a thread of a project.
        await expect(crumb, 'a header with room shows the project crumb').toBeVisible();
        // The row never shrinks the crumb: its name is whole, or it is a
        // name past the cap, which holds the cap's width.
        const shown = await crumb.evaluate((el) => {
          const label = el.querySelector('span')!;
          return { whole: label.scrollWidth <= label.clientWidth, width: el.getBoundingClientRect().width };
        });
        if (!shown.whole) {
          expect(shown.width, 'only a name past the cap is cut, and it keeps the cap').toBeGreaterThanOrEqual(CRUMB_CAP_PX - 1);
        }
      }
      // The row is placed by floors: the title's is 8rem or its own width.
      // A header that fits its cluster beside that keeps one row; one that
      // cannot moves the cluster, whole, to a second row.
      const rows = await header.evaluate((el, floor) => {
        const style = getComputedStyle(el);
        const room = el.clientWidth - parseFloat(style.paddingLeft) - parseFloat(style.paddingRight);
        const gap = parseFloat(style.columnGap) || 0;
        const titleEl = el.querySelector<HTMLElement>('[data-testid="chat-header-title"]')!;
        const titleBottom = titleEl.getBoundingClientRect().bottom;
        const inFlow = ([...el.children] as HTMLElement[]).filter((child) => {
          const childStyle = getComputedStyle(child);
          return childStyle.display !== 'none' && childStyle.position !== 'absolute';
        });
        const cluster = inFlow[inFlow.length - 1];
        // What the cluster needs to sit on one row: its laid-out width says
        // nothing, since a cluster capped at the pane's width wraps inside it.
        const controls = ([...cluster.children] as HTMLElement[]).filter((child) => child.getBoundingClientRect().width > 0);
        const clusterGap = parseFloat(getComputedStyle(cluster).columnGap) || 0;
        const clusterNeeded = controls.reduce((sum, child) => sum + child.getBoundingClientRect().width, 0)
          + clusterGap * Math.max(0, controls.length - 1);
        const needed = inFlow.reduce((sum, child) => sum + (child === titleEl
          ? Math.min(floor, titleEl.scrollWidth)
          : child.getBoundingClientRect().width), 0) + gap * (inFlow.length - 1);
        return {
          spare: room - needed,
          wrapped: cluster.getBoundingClientRect().top >= titleBottom - 1,
          clusterRows: new Set(controls
            .map((child) => Math.round(child.getBoundingClientRect().top + child.getBoundingClientRect().height / 2))).size,
          clusterSpare: room - clusterNeeded,
        };
      }, TITLE_FLOOR_PX);
      // Within a pixel of the boundary either placement is a rounding call.
      if (Math.abs(rows.spare) > 1) {
        expect(rows.wrapped, 'the cluster takes a second row only when it cannot fit beside the title at its floor').toBe(rows.spare < 0);
      }
      if (Math.abs(rows.clusterSpare) > 1) {
        expect(rows.clusterRows > 1, 'the cluster wraps inside itself only in a pane narrower than its controls').toBe(rows.clusterSpare < 0);
      }
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
