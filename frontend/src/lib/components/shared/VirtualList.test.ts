import { describe, it, expect, beforeAll, beforeEach } from 'vitest';
import { render } from '@testing-library/svelte';
import { tick } from 'svelte';
import VirtualList from './VirtualList.svelte';

// happy-dom doesn't implement ResizeObserver. Install a minimal polyfill
// that fires once on observe so the initial visible-window calculation
// runs. Tests override clientHeight via the stub below.
class StubResizeObserver {
  private cb: ResizeObserverCallback;
  private element: Element | null = null;
  constructor(cb: ResizeObserverCallback) {
    this.cb = cb;
  }
  observe(el: Element): void {
    this.element = el;
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
  (globalThis as unknown as { ResizeObserver: typeof ResizeObserver }).ResizeObserver =
    StubResizeObserver as unknown as typeof ResizeObserver;
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
});
