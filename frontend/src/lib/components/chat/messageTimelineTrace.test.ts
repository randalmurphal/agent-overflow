import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
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

describe('messageTimelineTrace', () => {
  beforeEach(() => {
    clearUiRenderTrace();
    setUiRenderTraceEnabled(true);
  });

  afterEach(() => {
    clearUiRenderTrace();
    setUiRenderTraceEnabled(false);
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
    let height = 20;
    row.getBoundingClientRect = () => ({ height }) as DOMRect;

    const stop = startTimelineRowResizeTrace(root);

    await nextFrame();
    expect(getUiRenderTraceRecords()).toEqual([]);

    height = 35;
    row.querySelector('code')!.textContent = 'hello world';
    await nextFrame();

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

    const row = document.createElement('div');
    row.dataset.rowIndex = '2';
    row.innerHTML = '<div data-item-id="added">Added row</div>';
    let height = 10;
    row.getBoundingClientRect = () => ({ height }) as DOMRect;
    root.appendChild(row);
    await nextFrame();

    height = 22;
    row.querySelector('[data-item-id]')!.textContent = 'Added row with more text';
    await nextFrame();
    expect(getUiRenderTraceRecords()).toHaveLength(1);

    root.removeChild(row);
    await nextFrame();
    clearUiRenderTrace();

    height = 35;
    row.querySelector('[data-item-id]')!.textContent = 'Detached row changed';
    await nextFrame();

    expect(getUiRenderTraceRecords()).toEqual([]);
    stop();
    root.remove();
  });

  it('measures only rows dirtied by descendant mutations after baseline', async () => {
    const root = document.createElement('div');
    document.body.appendChild(root);
    root.innerHTML = `
      <div data-row-index="1"><div data-item-id="row-1">First row</div></div>
      <div data-row-index="2"><div data-item-id="row-2">Second row</div></div>
    `;
    const [first, second] = Array.from(root.querySelectorAll<HTMLElement>('[data-row-index]'));
    let firstHeight = 20;
    let secondHeight = 30;
    const firstMeasure = vi.fn(() => ({ height: firstHeight }) as DOMRect);
    const secondMeasure = vi.fn(() => ({ height: secondHeight }) as DOMRect);
    first.getBoundingClientRect = firstMeasure;
    second.getBoundingClientRect = secondMeasure;

    const stop = startTimelineRowResizeTrace(root);
    await nextFrame();
    const firstBaselineCalls = firstMeasure.mock.calls.length;
    const secondBaselineCalls = secondMeasure.mock.calls.length;

    firstHeight = 45;
    first.querySelector('[data-item-id]')!.textContent = 'First row with more text';
    await nextFrame();

    expect(firstMeasure.mock.calls.length).toBeGreaterThan(firstBaselineCalls);
    expect(secondMeasure.mock.calls.length).toBe(secondBaselineCalls);
    expect(getUiRenderTraceRecords()).toHaveLength(1);
    expect(getUiRenderTraceRecords()[0]).toMatchObject({
      label: 'timeline.row.resize',
      data: { rowIndex: '1', itemId: 'row-1', prevHeight: 20, newHeight: 45 },
    });

    stop();
    root.remove();
  });
});
