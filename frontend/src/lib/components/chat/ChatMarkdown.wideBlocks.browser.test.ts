import { render, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import '../../../app.css';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import ChatMarkdown from './ChatMarkdown.svelte';
import { resetCodeSpanCacheForTest } from './markdown/codeSpanCache';
import { resetCodeWrapStateForTest } from './markdown/codeWrapState';
import { __resetStreamdownCodeHostForTest } from './markdown/StreamdownCodeHost.svelte';

// Real-Chromium geometry for blocks that can be wider than their column.
// The contract is "pan inside the block's own box, never clip, and change
// nothing for a block that fits", which happy-dom's zero geometry cannot
// prove. The fade is a scroll-driven mask: its fade widths are 0 while the
// timeline is inactive (nothing overflows) and non-zero once it runs.

const COLUMN_PX = 320;

const WIDE_TABLE = [
  '| Name | Description | Severity | Recommendation | Owner |',
  '| --- | --- | --- | --- | --- |',
  '| alpha | Something fairly long here | High | Do the thing | randy |',
  '| beta | short | Low | Nothing | x |',
].join('\n');

const NARROW_TABLE = ['| A | B |', '| --- | --- |', '| 1 | 2 |'].join('\n');

const LONG_LINE =
  'const veryLongVariableName = someFunction(argumentOne, argumentTwo, argumentThree, argumentFour);';

function mount(source: string) {
  const column = document.createElement('div');
  column.style.width = `${COLUMN_PX}px`;
  column.className = 'markdown-body';
  document.body.appendChild(column);
  const view = render(ChatMarkdown, {
    target: column,
    props: { source, pathRefs: [] },
  });
  return { column, view };
}

function fadeWidths(el: Element): { start: string; end: string } {
  const cs = getComputedStyle(el);
  return {
    start: cs.getPropertyValue('--pan-x-fade-start').trim(),
    end: cs.getPropertyValue('--pan-x-fade-end').trim(),
  };
}

beforeEach(() => {
  resetCodeSpanCacheForTest();
  resetCodeWrapStateForTest();
  __resetStreamdownCodeHostForTest();
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => ['none', 'keyword']);
  document.body.innerHTML = '';
});

describe('wide markdown tables', () => {
  it('pans inside the wrapper instead of overflowing the column', async () => {
    const { column } = mount(WIDE_TABLE);
    const wrapper = await waitFor(() => {
      const found = column.querySelector<HTMLElement>('[data-streamdown-table]');
      expect(found).not.toBeNull();
      return found!;
    });
    const table = wrapper.querySelector('table')!;

    expect(getComputedStyle(wrapper).overflowX).toBe('auto');
    expect(wrapper.getBoundingClientRect().width).toBeLessThanOrEqual(COLUMN_PX);
    expect(table.getBoundingClientRect().width).toBeGreaterThan(COLUMN_PX);
    expect(wrapper.scrollWidth).toBeGreaterThan(wrapper.clientWidth);

    // Fade at the far edge only while resting at the start ...
    await waitFor(() => expect(fadeWidths(wrapper).end).not.toBe('0px'));
    expect(fadeWidths(wrapper).start).toBe('0px');
    // ... and at the near edge only once scrolled to the end.
    wrapper.scrollLeft = wrapper.scrollWidth;
    await waitFor(() => expect(fadeWidths(wrapper).start).not.toBe('0px'));
    expect(fadeWidths(wrapper).end).toBe('0px');
  });

  it('leaves a table that fits without a scroller or a fade', async () => {
    const { column } = mount(NARROW_TABLE);
    const wrapper = await waitFor(() => {
      const found = column.querySelector<HTMLElement>('[data-streamdown-table]');
      expect(found).not.toBeNull();
      return found!;
    });
    expect(wrapper.scrollWidth).toBe(wrapper.clientWidth);
    expect(fadeWidths(wrapper)).toEqual({ start: '0px', end: '0px' });
  });
});

describe('fenced code line wrapping', () => {
  it('wraps by default and pans once the block is unwrapped', async () => {
    const { column } = mount('```\n' + LONG_LINE + '\n```');
    // A language-less fence settles synchronously into static HTML.
    const button = await waitFor(() => {
      const found = column.querySelector<HTMLButtonElement>('button[data-static-code-wrap]');
      expect(found).not.toBeNull();
      return found!;
    });
    const pre = column.querySelector<HTMLElement>('.streamdown-code-host pre')!;
    expect(getComputedStyle(pre).whiteSpace).toBe('pre-wrap');
    expect(pre.scrollWidth).toBe(pre.clientWidth);
    expect(pre.getBoundingClientRect().height).toBeGreaterThan(
      parseFloat(getComputedStyle(pre).lineHeight) * 1.5,
    );

    button.click();

    await waitFor(() => expect(getComputedStyle(pre).whiteSpace).toBe('pre'));
    expect(getComputedStyle(pre).overflowX).toBe('auto');
    expect(pre.scrollWidth).toBeGreaterThan(pre.clientWidth);
    expect(pre.getBoundingClientRect().width).toBeLessThanOrEqual(COLUMN_PX);
    await waitFor(() => expect(fadeWidths(pre).end).not.toBe('0px'));

    button.click();
    await waitFor(() => expect(getComputedStyle(pre).whiteSpace).toBe('pre-wrap'));
    expect(pre.scrollWidth).toBe(pre.clientWidth);
  });
});
