import { describe, it, expect, afterAll, afterEach, beforeAll, beforeEach, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import { tick } from 'svelte';
import VirtualList from './VirtualList.svelte';

// happy-dom doesn't implement ResizeObserver. Install a minimal polyfill
// that fires once on observe so the initial visible-window calculation
// runs. Tests override clientHeight via the stub below.
class StubResizeObserver {
  private cb: ResizeObserverCallback;
  constructor(cb: ResizeObserverCallback) {
    this.cb = cb;
  }
  observe(el: Element): void {
    // Fire synchronously with the current bounding rect. Tests can
    // override getBoundingClientRect before mounting to control the
    // visible window size.
    const rect = el.getBoundingClientRect();
    this.cb(
      [
        {
          target: el,
          contentRect: rect,
          borderBoxSize: [],
          contentBoxSize: [],
          devicePixelContentBoxSize: [],
        } as unknown as ResizeObserverEntry,
      ],
      this as unknown as ResizeObserver,
    );
  }
  unobserve(): void {}
  disconnect(): void {}
}

beforeAll(() => {
  vi.stubGlobal('ResizeObserver', StubResizeObserver);
});
afterAll(() => {
  vi.unstubAllGlobals();
});
// The geometry stub shadows Element.prototype's for one test at a time.
afterEach(() => {
  delete (HTMLElement.prototype as Partial<HTMLElement>).getBoundingClientRect;
});

function stubDim(viewportHeight: number) {
  Object.defineProperty(HTMLElement.prototype, 'getBoundingClientRect', {
    configurable: true,
    value() {
      return {
        x: 0,
        y: 0,
        top: 0,
        left: 0,
        bottom: viewportHeight,
        right: 800,
        width: 800,
        height: viewportHeight,
        toJSON() {
          return {};
        },
      };
    },
  });
}

beforeEach(() => {
  // Reset to a sensible default; each test can override.
  stubDim(500);
});

function makeItems(n: number): Array<{ id: string; label: string }> {
  return Array.from({ length: n }, (_, i) => ({ id: `item-${i}`, label: `Row ${i}` }));
}

describe('<VirtualList>', () => {
  it('renders only the visible window (not all 5000 rows)', async () => {
    const items = makeItems(5000);
    stubDim(500); // 500px viewport / 50px rowHeight = 10 visible rows
    const { container } = render(VirtualList as any, {
      props: {
        items,
        rowHeight: 50,
        overscan: 6,
        // Children snippet is passed separately in testing-library for
        // svelte. Use a minimal placeholder that renders the label.
        children: null as unknown as undefined,
      },
    });

    // We can't easily pass a Svelte snippet via testing-library props,
    // so we just assert on the row wrappers emitted by VirtualList
    // itself (each wrapper has the data-testid="virtual-list-row"
    // attribute regardless of what the snippet renders).
    await tick();
    const rows = container.querySelectorAll('[data-testid="virtual-list-row"]');
    // 10 visible rows + 6 overscan on each side (but startIndex clamps
    // to >= 0 at the top, so we get 10 + 6 = 16 on the first window).
    expect(rows.length).toBeLessThan(5000);
    expect(rows.length).toBeLessThan(50);
    expect(rows.length).toBeGreaterThan(0);
  });

  it('spacer reserves the full scroll height', async () => {
    const items = makeItems(1000);
    const { container } = render(VirtualList as any, {
      props: {
        items,
        rowHeight: 48,
        children: null as unknown as undefined,
      },
    });
    await tick();

    const spacer = container.querySelector<HTMLElement>('[data-testid="virtual-list-spacer"]');
    expect(spacer).toBeTruthy();
    const height = spacer!.style.height;
    // 1000 * 48 = 48000px
    expect(height).toBe('48000px');
  });

  it('scrolling updates the rendered window', async () => {
    const items = makeItems(1000);
    stubDim(500);
    const { container } = render(VirtualList as any, {
      props: {
        items,
        rowHeight: 50,
        overscan: 2,
        children: null as unknown as undefined,
      },
    });
    await tick();

    const viewport = container.querySelector<HTMLElement>('[data-testid="virtual-list-viewport"]');
    expect(viewport).toBeTruthy();

    const initialRows = Array.from(
      container.querySelectorAll<HTMLElement>('[data-testid="virtual-list-row"]'),
    );
    const initialIndices = initialRows.map((r) => Number(r.dataset.rowIndex));
    expect(initialIndices[0]).toBe(0);

    // Scroll to the middle of the list: 500 * 50 = 25000px.
    viewport!.scrollTop = 25000;
    viewport!.dispatchEvent(new Event('scroll'));
    await tick();

    const midRows = Array.from(
      container.querySelectorAll<HTMLElement>('[data-testid="virtual-list-row"]'),
    );
    const midIndices = midRows.map((r) => Number(r.dataset.rowIndex));
    // First visible index should be ~500 - overscan.
    expect(midIndices[0]).toBeGreaterThanOrEqual(498 - 5);
    expect(midIndices[0]).toBeLessThanOrEqual(502);
    // None of the last-page rows should be present at the same time.
    expect(midIndices.every((i) => i < 999)).toBe(true);
  });

  it('reacts to items added dynamically (spacer grows)', async () => {
    let items = makeItems(100);
    const { container, rerender } = render(VirtualList as any, {
      props: {
        items,
        rowHeight: 48,
        children: null as unknown as undefined,
      },
    });
    await tick();

    let spacer = container.querySelector<HTMLElement>('[data-testid="virtual-list-spacer"]');
    expect(spacer!.style.height).toBe('4800px');

    items = makeItems(200);
    await rerender({ items, rowHeight: 48, children: null as unknown as undefined });
    await tick();

    spacer = container.querySelector<HTMLElement>('[data-testid="virtual-list-spacer"]');
    expect(spacer!.style.height).toBe('9600px');
  });

  it('reacts to items removed dynamically (spacer shrinks)', async () => {
    let items = makeItems(500);
    const { container, rerender } = render(VirtualList as any, {
      props: {
        items,
        rowHeight: 48,
        children: null as unknown as undefined,
      },
    });
    await tick();

    let spacer = container.querySelector<HTMLElement>('[data-testid="virtual-list-spacer"]');
    expect(spacer!.style.height).toBe('24000px');

    items = makeItems(3);
    await rerender({ items, rowHeight: 48, children: null as unknown as undefined });
    await tick();

    spacer = container.querySelector<HTMLElement>('[data-testid="virtual-list-spacer"]');
    expect(spacer!.style.height).toBe('144px');
    const rows = container.querySelectorAll('[data-testid="virtual-list-row"]');
    expect(rows.length).toBe(3);
  });

  it('shows the end of a list that shrank under a scrolled reader, before any scroll event', async () => {
    stubDim(500);
    const props = { rowHeight: 50, overscan: 2, children: null as unknown as undefined };
    const { container, rerender } = render(VirtualList as any, { props: { ...props, items: makeItems(1000) } });
    await tick();
    const viewport = container.querySelector<HTMLElement>('[data-testid="virtual-list-viewport"]')!;
    viewport.scrollTop = 45000;
    viewport.dispatchEvent(new Event('scroll'));
    await tick();

    // The browser clamps scrollTop and reports it a frame later; the
    // window must not sit past the list's end until then.
    await rerender({ ...props, items: makeItems(40) });
    await tick();
    const indices = Array.from(
      container.querySelectorAll<HTMLElement>('[data-testid="virtual-list-row"]'),
    ).map((row) => Number(row.dataset.rowIndex));
    expect(indices.at(-1)).toBe(39);
    expect(indices.length).toBe(14);
  });

  it('keeps a row\'s element with its item when `key` names the item', async () => {
    const paths = (names: string[]) => names.map((path) => ({ path }));
    const props = {
      rowHeight: 50,
      children: null as unknown as undefined,
      key: (item: { path: string }) => item.path,
    };
    const { container, rerender } = render(VirtualList as any, { props: { ...props, items: paths(['b', 'c']) } });
    await tick();
    const rowsNow = () => Array.from(container.querySelectorAll<HTMLElement>('[data-testid="virtual-list-row"]'));
    const [b, c] = rowsNow();

    await rerender({ ...props, items: paths(['a', 'b', 'c']) });
    await tick();
    const after = rowsNow();
    expect(after).toHaveLength(3);
    expect(after[1]).toBe(b);
    expect(after[2]).toBe(c);
    expect(after.map((row) => row.dataset.rowIndex)).toEqual(['0', '1', '2']);
  });

  it('keeps the focused row mounted outside the window until focus leaves the list', async () => {
    stubDim(500);
    const { container } = render(VirtualList as any, {
      props: { items: makeItems(1000), rowHeight: 50, overscan: 2, children: null as unknown as undefined },
    });
    await tick();
    const viewport = container.querySelector<HTMLElement>('[data-testid="virtual-list-viewport"]')!;
    const rowAt = (index: number) => container.querySelector<HTMLElement>(`[data-row-index="${index}"]`);
    const scrollTo = async (top: number) => {
      viewport.scrollTop = top;
      viewport.dispatchEvent(new Event('scroll'));
      await tick();
    };
    const focused = rowAt(3)!;
    focused.dispatchEvent(new FocusEvent('focusin', { bubbles: true }));
    await tick();

    // Below the window, then above it: the same element, at its own offset.
    await scrollTo(25000);
    expect(rowAt(3)).toBe(focused);
    expect(focused.style.position).toBe('absolute');
    expect(focused.style.top).toBe('150px');
    expect(container.querySelector('[data-testid="virtual-list-row"]')).toBe(focused);
    expect(container.querySelectorAll('[data-testid="virtual-list-row"]').length).toBe(15);

    // Back in the window it is an in-flow row again, still the same element.
    await scrollTo(0);
    expect(rowAt(3)).toBe(focused);
    expect(focused.style.position).toBe('');

    await scrollTo(25000);
    focused.dispatchEvent(new FocusEvent('focusout', { bubbles: true, relatedTarget: document.body }));
    await tick();
    expect(rowAt(3)).toBeNull();
  });

  it('pins a focused row above the window after the rows of the window', async () => {
    stubDim(500);
    const { container } = render(VirtualList as any, {
      props: { items: makeItems(1000), rowHeight: 50, overscan: 2, children: null as unknown as undefined },
    });
    await tick();
    const viewport = container.querySelector<HTMLElement>('[data-testid="virtual-list-viewport"]')!;
    viewport.scrollTop = 25000;
    viewport.dispatchEvent(new Event('scroll'));
    await tick();
    const focused = container.querySelector<HTMLElement>('[data-row-index="505"]')!;
    focused.dispatchEvent(new FocusEvent('focusin', { bubbles: true }));
    viewport.scrollTop = 0;
    viewport.dispatchEvent(new Event('scroll'));
    await tick();

    const rows = Array.from(container.querySelectorAll<HTMLElement>('[data-testid="virtual-list-row"]'));
    expect(rows.at(-1)).toBe(focused);
    expect(focused.style.top).toBe('25250px');
    expect(rows[0].dataset.rowIndex).toBe('0');
  });

  it('forgets a focused row whose item left the list', async () => {
    const props = { rowHeight: 50, overscan: 2, children: null as unknown as undefined };
    stubDim(500);
    const { container, rerender } = render(VirtualList as any, { props: { ...props, items: makeItems(1000) } });
    await tick();
    container.querySelector<HTMLElement>('[data-row-index="3"]')!.dispatchEvent(new FocusEvent('focusin', { bubbles: true }));
    await tick();

    await rerender({ ...props, items: makeItems(1000).filter((item) => item.id !== 'item-3') });
    await tick();
    const viewport = container.querySelector<HTMLElement>('[data-testid="virtual-list-viewport"]')!;
    viewport.scrollTop = 25000;
    viewport.dispatchEvent(new Event('scroll'));
    await rerender({ ...props, items: makeItems(1000) });
    await tick();
    expect(container.querySelector('[data-row-index="3"]')).toBeNull();
  });

  it('names each row\'s place in the whole list', async () => {
    stubDim(500);
    const { container } = render(VirtualList as any, {
      props: { items: makeItems(1000), rowHeight: 50, children: null as unknown as undefined },
    });
    await tick();
    const row = container.querySelector<HTMLElement>('[data-row-index="4"]')!;
    expect(row.getAttribute('role')).toBe('listitem');
    expect(row.getAttribute('aria-setsize')).toBe('1000');
    expect(row.getAttribute('aria-posinset')).toBe('5');
  });
});
