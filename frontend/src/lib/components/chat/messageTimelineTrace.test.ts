import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
  clearUiRenderTrace,
  getUiRenderTraceRecords,
  setUiRenderTraceEnabled,
} from '../../utils/uiRenderTrace';
import { startTimelineRowResizeTrace } from './messageTimelineTrace';

async function nextFrame(): Promise<void> {
  await Promise.resolve();
  await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
}

class FireableResizeObserver {
  static instances: FireableResizeObserver[] = [];

  private readonly observed = new Set<Element>();

  constructor(private readonly callback: ResizeObserverCallback) {
    FireableResizeObserver.instances.push(this);
  }

  observe(target: Element): void {
    this.observed.add(target);
  }

  unobserve(target: Element): void {
    this.observed.delete(target);
  }

  disconnect(): void {
    this.observed.clear();
  }

  isObserved(target: Element): boolean {
    return this.observed.has(target);
  }

  fire(target: Element, height: number): void {
    if (!this.observed.has(target)) return;
    const entry = {
      target,
      contentRect: { height },
    } as ResizeObserverEntry;
    this.callback([entry], this as unknown as ResizeObserver);
  }
}

function firstResizeObserver(): FireableResizeObserver {
  const observer = FireableResizeObserver.instances[0];
  if (!observer) throw new Error('expected row resize observer');
  return observer;
}

function setResizeObserverUnavailable(): void {
  Object.defineProperty(globalThis, 'ResizeObserver', {
    configurable: true,
    writable: true,
    value: undefined,
  });
}

describe('messageTimelineTrace', () => {
  let originalResizeObserver: typeof ResizeObserver | undefined;

  beforeEach(() => {
    originalResizeObserver = globalThis.ResizeObserver;
    FireableResizeObserver.instances = [];
    globalThis.ResizeObserver = FireableResizeObserver as unknown as typeof ResizeObserver;
    clearUiRenderTrace();
    setUiRenderTraceEnabled(true);
  });

  afterEach(() => {
    if (originalResizeObserver !== undefined) {
      globalThis.ResizeObserver = originalResizeObserver;
    } else {
      setResizeObserverUnavailable();
    }
    FireableResizeObserver.instances = [];
    clearUiRenderTrace();
    setUiRenderTraceEnabled(false);
  });

  it('returns a no-op cleanup when ResizeObserver is unavailable', () => {
    setResizeObserverUnavailable();
    const root = document.createElement('div');
    document.body.appendChild(root);
    root.innerHTML = '<div data-row-index="1"><div data-item-id="row-1">Row</div></div>';

    const stop = startTimelineRowResizeTrace(root);
    stop();

    expect(FireableResizeObserver.instances).toEqual([]);
    expect(getUiRenderTraceRecords()).toEqual([]);
    root.remove();
  });

  it('records row resize traces after the initial measurement', async () => {
    const root = document.createElement('div');
    document.body.appendChild(root);
    root.innerHTML = `
      <div data-row-index="7">
        <div data-item-id="item-7">
          <pre class="shiki"><code data-streamdown-code="code-1">hello</code></pre>
        </div>
      </div>
    `;
    const row = root.querySelector<HTMLElement>('[data-row-index]')!;

    const stop = startTimelineRowResizeTrace(root);
    const observer = firstResizeObserver();

    observer.fire(row, 20);
    expect(getUiRenderTraceRecords()).toEqual([]);

    row.querySelector('code')!.textContent = 'hello world';
    await nextFrame();
    expect(getUiRenderTraceRecords()).toEqual([]);

    observer.fire(row, 35);

    const records = getUiRenderTraceRecords();
    expect(records).toHaveLength(1);
    expect(records[0]).toMatchObject({
      label: 'timeline.row.resize',
      data: {
        rowIndex: '7',
        itemId: 'item-7',
        prevHeight: 20,
        newHeight: 35,
        delta: 15,
      },
    });
    expect(records[0]?.data).not.toHaveProperty('contentTags');
    expect(records[0]?.data).not.toHaveProperty('descendants');

    stop();
    root.remove();
  });

  it('tracks rows added after startup and ignores them after removal', async () => {
    const root = document.createElement('div');
    document.body.appendChild(root);
    const stop = startTimelineRowResizeTrace(root);
    const observer = firstResizeObserver();

    const row = document.createElement('div');
    row.dataset.rowIndex = '2';
    row.innerHTML = '<div data-item-id="added">Added row</div>';
    root.appendChild(row);
    await nextFrame();
    expect(observer.isObserved(row)).toBe(true);
    observer.fire(row, 10);

    row.querySelector('[data-item-id]')!.textContent = 'Added row with more text';
    await nextFrame();
    expect(getUiRenderTraceRecords()).toEqual([]);

    observer.fire(row, 22);
    expect(getUiRenderTraceRecords()).toHaveLength(1);

    root.removeChild(row);
    await nextFrame();
    expect(observer.isObserved(row)).toBe(false);
    clearUiRenderTrace();

    row.querySelector('[data-item-id]')!.textContent = 'Detached row changed';
    await nextFrame();
    observer.fire(row, 35);

    expect(getUiRenderTraceRecords()).toEqual([]);
    stop();
    root.remove();
  });

  it('ignores child-list mutations inside an already tracked row', async () => {
    const root = document.createElement('div');
    document.body.appendChild(root);
    root.innerHTML = `
      <div data-row-index="1"><div data-item-id="row-1">First row</div></div>
    `;
    const row = root.querySelector<HTMLElement>('[data-row-index]')!;

    const stop = startTimelineRowResizeTrace(root);
    const resizeObserver = firstResizeObserver();
    resizeObserver.fire(row, 20);

    const nested = document.createElement('div');
    nested.dataset.rowIndex = 'nested';
    nested.innerHTML = '<div data-item-id="nested">Nested row marker</div>';
    row.appendChild(nested);
    await nextFrame();

    expect(resizeObserver.isObserved(nested)).toBe(false);
    resizeObserver.fire(nested, 40);
    expect(getUiRenderTraceRecords()).toEqual([]);

    stop();
    root.remove();
  });

  it('records only rows whose observed size changed after baseline', async () => {
    const root = document.createElement('div');
    document.body.appendChild(root);
    root.innerHTML = `
      <div data-row-index="1"><div data-item-id="row-1">First row</div></div>
      <div data-row-index="2"><div data-item-id="row-2">Second row</div></div>
    `;
    const [first, second] = Array.from(root.querySelectorAll<HTMLElement>('[data-row-index]'));

    const stop = startTimelineRowResizeTrace(root);
    const observer = firstResizeObserver();
    observer.fire(first, 20);
    observer.fire(second, 30);

    first.querySelector('[data-item-id]')!.textContent = 'First row with more text';
    await nextFrame();
    expect(getUiRenderTraceRecords()).toEqual([]);

    observer.fire(first, 45);
    expect(getUiRenderTraceRecords()).toHaveLength(1);
    expect(getUiRenderTraceRecords()[0]).toMatchObject({
      label: 'timeline.row.resize',
      data: { rowIndex: '1', itemId: 'row-1', prevHeight: 20, newHeight: 45 },
    });

    stop();
    root.remove();
  });
});
