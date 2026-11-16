// The browser chrome's real layout in Chromium: tabs share the strip evenly
// between their maximum and minimum width, the tab row scrolls past that with
// the new-tab button still on screen and the active tab in view, and the
// address row fits a narrow pane.

import { afterEach, describe, expect, it } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import '../../../app.css';
import BrowserTabStrip, { TAB_MAX_WIDTH, TAB_MIN_WIDTH } from './BrowserTabStrip.svelte';
import BrowserAddressBar from './BrowserAddressBar.svelte';
import { raf } from '../../../test/helpers/browserFrames';
import { rawProps } from '../../../test/helpers/rawProps.svelte';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';

let host: HTMLDivElement | null = null;
let app: ReturnType<typeof mount> | null = null;

afterEach(async () => {
  if (app) await unmount(app);
  app = null;
  host?.remove();
  host = null;
});

function pages(count: number) {
  return Array.from({ length: count }, (_, i) => ({
    id: `p${i}`,
    url: `https://example.test/${i}`,
    title: `A fairly long page title number ${i}`,
    canGoBack: false,
    canGoForward: false,
  }));
}

async function mountStrip(width: number, count: number, active: number): Promise<HTMLElement> {
  host = document.createElement('div');
  host.style.cssText = `position:fixed;top:0;left:0;width:${width}px`;
  document.body.append(host);
  const list = pages(count);
  app = mount(BrowserTabStrip, {
    target: host,
    props: { sessionName: '', pages: list as never, activePageId: list[active].id, onAct: async () => {}, onNewTab: () => {} },
  });
  flushSync();
  await raf();
  await raf();
  return host;
}

function box(el: Element): DOMRect {
  return el.getBoundingClientRect();
}

const WIDTHS = [320, 480, 800, 1400];
const COUNTS = [1, 4, 8];

describe('browser tab strip layout', () => {
  for (const width of WIDTHS) {
    for (const count of COUNTS) {
      it(`${count} tab(s) in a ${width}px pane share the strip evenly, then scroll`, async () => {
        const root = await mountStrip(width, count, count - 1);
        const strip = root.querySelector<HTMLElement>('[data-testid="browser-tab-strip"]')!;
        const scroller = root.querySelector<HTMLElement>('[data-testid="browser-tab-scroller"]')!;
        const newTab = root.querySelector<HTMLElement>('[data-testid="browser-new-tab"]')!;
        const tabs = [...root.querySelectorAll<HTMLElement>('[data-tab-id]')];
        expect(tabs).toHaveLength(count);

        const widths = tabs.map((tab) => box(tab).width);
        for (const w of widths) {
          expect(Math.abs(w - widths[0])).toBeLessThanOrEqual(0.1);
          expect(w).toBeGreaterThanOrEqual(TAB_MIN_WIDTH - 0.5);
          expect(w).toBeLessThanOrEqual(TAB_MAX_WIDTH + 0.5);
        }

        // The new-tab button is whole and inside the strip, right after the
        // tab row, however many tabs there are.
        const stripBox = box(strip);
        const newBox = box(newTab);
        expect(newBox.left).toBeGreaterThanOrEqual(stripBox.left);
        expect(newBox.right).toBeLessThanOrEqual(stripBox.right);
        expect(newBox.left - box(scroller).right).toBeLessThanOrEqual(8);
        expect(newBox.left - box(scroller).right).toBeGreaterThanOrEqual(0);

        const overflowing = scroller.scrollWidth > scroller.clientWidth + 1;
        const space = box(root.querySelector('[data-testid="browser-tab-strip-space"]')!).width;
        // Tabs shrink only once the strip is full, and scroll only at their
        // minimum width.
        if (widths[0] < TAB_MAX_WIDTH - 0.5) expect(space).toBeLessThanOrEqual(8.5);
        if (overflowing) expect(widths[0]).toBeCloseTo(TAB_MIN_WIDTH, 0);
        else expect(space).toBeGreaterThanOrEqual(7.5);
        // The cases a reader can check by hand.
        if (count === 1) expect(widths[0]).toBeCloseTo(TAB_MAX_WIDTH, 0);
        if (count === 8 && width <= 480) expect(overflowing).toBe(true);
        if (count === 4 && width >= 1400) expect(widths[0]).toBeCloseTo(TAB_MAX_WIDTH, 0);
        if (count === 8 && width === 800) {
          expect(overflowing).toBe(false);
          expect(widths[0]).toBeGreaterThan(TAB_MIN_WIDTH);
          expect(widths[0]).toBeLessThan(TAB_MAX_WIDTH);
        }

        // The active (last) tab is scrolled into view.
        const activeBox = box(tabs[count - 1]);
        const scrollerBox = box(scroller);
        expect(activeBox.left).toBeGreaterThanOrEqual(scrollerBox.left - 0.5);
        expect(activeBox.right).toBeLessThanOrEqual(scrollerBox.right + 0.5);

        // A tab too narrow for a readable title shows its icon alone.
        const title = tabs[0].querySelector<HTMLElement>('[data-testid="browser-tab-title"]')!;
        if (widths[0] < 88) expect(getComputedStyle(title).display).toBe('none');
        else expect(box(title).width).toBeGreaterThan(20);
        // The strip never scrolls the whole pane sideways.
        expect(strip.scrollWidth).toBeLessThanOrEqual(strip.clientWidth + 1);
      });
    }
  }

  it('keeps the active tab in view as it changes and as the pane narrows', async () => {
    host = document.createElement('div');
    host.style.cssText = 'position:fixed;top:0;left:0;width:320px';
    document.body.append(host);
    const props = rawProps({ sessionName: '', pages: pages(8) as never, activePageId: 'p0', onAct: async () => {}, onNewTab: () => {} });
    app = mount(BrowserTabStrip, { target: host, props });
    flushSync();
    await raf();
    const root = host;
    const scroller = root.querySelector<HTMLElement>('[data-testid="browser-tab-scroller"]')!;
    const tab = (i: number) => root.querySelector<HTMLElement>(`[data-tab-id="p${i}"]`)!;
    const inView = (i: number) => {
      const t = box(tab(i));
      const s = box(scroller);
      return t.left >= s.left - 0.5 && t.right <= s.right + 0.5;
    };
    expect(scroller.scrollLeft).toBe(0);
    expect(inView(0)).toBe(true);
    expect(inView(6)).toBe(false);
    props.activePageId = 'p6';
    flushSync();
    await raf();
    expect(inView(6)).toBe(true);
    expect(scroller.scrollLeft).toBeGreaterThan(0);
    props.activePageId = 'p1';
    flushSync();
    await raf();
    expect(inView(1)).toBe(true);
    // A vertical wheel scrolls the row sideways.
    const before = scroller.scrollLeft;
    const wheel = new WheelEvent('wheel', { deltaY: 100, bubbles: true, cancelable: true });
    scroller.dispatchEvent(wheel);
    expect(wheel.defaultPrevented).toBe(true);
    expect(scroller.scrollLeft).toBeGreaterThan(before);
    // The strip narrowing keeps the active tab in view.
    props.activePageId = 'p7';
    flushSync();
    await raf();
    root.style.width = '250px';
    await raf();
    await raf();
    expect(inView(7)).toBe(true);
  });
});

describe('browser address row layout', () => {
  afterEach(() => setCompactLayoutForTest(false));

  const shown = (el: Element | null): el is HTMLElement => !!el && el.getClientRects().length > 0;

  for (const width of [320, 480, 800]) {
    for (const compact of [false, true]) {
      for (const sizeLabel of ['', '1280×720']) {
        for (const localFile of [false, true]) {
          const name = `${width}px${compact ? ' compact' : ''}${sizeLabel ? ' with the size label' : ''}${localFile ? ' on a local file' : ''}`;
          it(`${name}: a usable address field and full-size buttons, the rest in More`, async () => {
            setCompactLayoutForTest(compact);
            host = document.createElement('div');
            host.style.cssText = `position:fixed;top:0;left:0;width:${width}px`;
            document.body.append(host);
            app = mount(BrowserAddressBar, {
              target: host,
              props: {
                activePage: {
                  id: 'a',
                  url: localFile ? 'file:///repo/report.html' : 'https://example.test/',
                  title: 'Report',
                  canGoBack: true,
                  canGoForward: false,
                } as never,
                sizeLabel,
                promotePaneId: 'browser-main',
                onAct: async () => {},
                onRevealPageFile: async () => {},
                onOpenFile: () => {},
                onLeave: () => {},
              },
            });
            flushSync();
            await raf();
            const row = host.querySelector<HTMLElement>('[data-testid="browser-address-row"]')!;
            const input = host.querySelector<HTMLElement>('input[aria-label="Address"]')!;
            expect(row.scrollWidth).toBeLessThanOrEqual(row.clientWidth + 1);
            expect(box(input).width).toBeGreaterThanOrEqual(width < 480 ? 96 : 200);
            const target = compact ? 36 : 28;
            const buttons = [...row.querySelectorAll<HTMLElement>('button')].filter(shown);
            expect(buttons.length).toBeGreaterThanOrEqual(4);
            for (const button of buttons) {
              expect(box(button).width, button.getAttribute('aria-label') ?? '').toBeCloseTo(target, 0);
              expect(box(button).right).toBeLessThanOrEqual(box(row).right + 0.5);
            }

            const more = host.querySelector<HTMLElement>('[data-testid="browser-more"]');
            const label = host.querySelector('[data-testid="browser-pane-size"]');
            if (width >= 576) {
              expect(shown(more)).toBe(false);
              expect(shown(host.querySelector('[data-testid="browser-open-file"]'))).toBe(true);
              expect(shown(label)).toBe(!!sizeLabel);
              expect(buttons.some((b) => b.getAttribute('aria-label') === 'Show in folder')).toBe(localFile);
              return;
            }
            expect(shown(more)).toBe(true);
            expect(shown(host.querySelector('[data-testid="browser-open-file"]'))).toBe(false);
            expect(shown(label)).toBe(false);
            more!.click();
            flushSync();
            await raf();
            const items = [...document.querySelectorAll<HTMLElement>('[role="menuitem"]')].map((el) => el.textContent?.trim() ?? '');
            expect(items.some((text) => text.includes('Open file'))).toBe(true);
            expect(items.some((text) => text.includes('Open devtools'))).toBe(true);
            expect(items.some((text) => text.includes('Show in folder'))).toBe(localFile);
            expect(items.some((text) => text.includes(`Laid out at ${sizeLabel}`))).toBe(!!sizeLabel);
          });
        }
      }
    }
  }
});
