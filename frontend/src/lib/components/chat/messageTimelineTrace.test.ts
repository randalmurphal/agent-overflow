import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
  clearUiRenderTrace,
  getUiRenderTraceRecords,
  setUiRenderTraceEnabled,
} from '../../utils/uiRenderTrace';
import { startTimelineRowResizeTrace } from './messageTimelineTrace';

class FireableResizeObserver {
  static instances: FireableResizeObserver[] = [];

  observed: Element[] = [];
  disconnected = false;

  constructor(private readonly callback: ResizeObserverCallback) {
    FireableResizeObserver.instances.push(this);
  }

  observe(target: Element): void {
    this.observed.push(target);
  }

  unobserve(target: Element): void {
    this.observed = this.observed.filter((candidate) => candidate !== target);
  }

  disconnect(): void {
    this.disconnected = true;
    this.observed = [];
  }

  fire(target: Element, height: number): void {
    this.callback([
      {
        target,
        contentRect: { height } as DOMRectReadOnly,
      } as ResizeObserverEntry,
    ], this as unknown as ResizeObserver);
  }
}

describe('messageTimelineTrace', () => {
  let originalRO: typeof ResizeObserver | undefined;

  beforeEach(() => {
    clearUiRenderTrace();
    setUiRenderTraceEnabled(true);
    FireableResizeObserver.instances = [];
    originalRO = globalThis.ResizeObserver;
    (globalThis as unknown as { ResizeObserver: typeof FireableResizeObserver }).ResizeObserver
      = FireableResizeObserver;
  });

  afterEach(() => {
    if (originalRO) {
      (globalThis as unknown as { ResizeObserver: typeof ResizeObserver }).ResizeObserver
        = originalRO;
    } else {
      delete (globalThis as unknown as { ResizeObserver?: typeof ResizeObserver }).ResizeObserver;
    }
    clearUiRenderTrace();
    setUiRenderTraceEnabled(false);
  });

  it('records row resize traces after the initial measurement', () => {
    const root = document.createElement('div');
    root.innerHTML = `
      <div data-row-index="7">
        <div data-item-id="item-7">
          <pre class="shiki"><code data-streamdown-code="code-1">hello</code></pre>
        </div>
      </div>
    `;
    const row = root.querySelector('[data-row-index]')!;

    const stop = startTimelineRowResizeTrace(root);
    const ro = FireableResizeObserver.instances[0];
    expect(ro?.observed).toContain(row);

    ro?.fire(row, 20);
    expect(getUiRenderTraceRecords()).toEqual([]);

    ro?.fire(row, 35);
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
        contentTags: 'shiki,sd-code',
      },
    });
    expect(records[0]?.data).toMatchObject({
      descendants: {
        preChildCount: 1,
        preTextLen: 5,
        sdCodeId: 'code-1',
      },
    });

    stop();
    expect(ro?.disconnected).toBe(true);
  });

  it('tracks rows added after startup and unobserves removed rows', async () => {
    const root = document.createElement('div');
    const stop = startTimelineRowResizeTrace(root);
    const ro = FireableResizeObserver.instances[0];

    const row = document.createElement('div');
    row.dataset.rowIndex = '2';
    row.innerHTML = '<div data-item-id="added">Added row</div>';
    root.appendChild(row);
    await Promise.resolve();

    expect(ro?.observed).toContain(row);

    root.removeChild(row);
    await Promise.resolve();

    expect(ro?.observed).not.toContain(row);
    stop();
  });
});
