import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import { flushSync, tick } from 'svelte';
import ThinkingBlock from './ThinkingBlock.svelte';
import { __setSmoothingClockForTest } from '../../stores/thread.svelte';
import type { SmoothingClock } from '../../markdown/smoothing/PerItemSmoother';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

class FakeSmoothingClock implements SmoothingClock {
  private current = 0;
  private nextHandle = 1;
  private pending = new Map<number, () => void>();
  now(): number { return this.current; }
  schedule(cb: () => void): number {
    const h = this.nextHandle++;
    this.pending.set(h, cb);
    return h;
  }
  cancel(h: number): void { this.pending.delete(h); }
  tickFrame(ms: number): void {
    this.current += ms;
    const toFire = [...this.pending.values()];
    this.pending.clear();
    for (const cb of toFire) cb();
  }
  pendingCount(): number { return this.pending.size; }
}

describe('<ThinkingBlock>', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  afterEach(() => {
    cleanup();
  });

  it('renders body text and timestamp inline; no standalone region', () => {
    const { container, queryByRole } = render(ThinkingBlock, {
      props: {
        item: makeItem({
          kind: 'thinking',
          summary: 'reasoning content',
          payloadId: 'thinking-payload',
          createdAt: Date.UTC(2026, 0, 1, 14, 32, 0),
        }),
      },
    });
    const body = container.querySelector('[data-testid="thinking-body"]');
    expect(body?.textContent).toBe('reasoning content');
    expect(queryByRole('region', { name: 'Thinking Content' })).toBeNull();
    expect(container.querySelector('time[datetime]')).not.toBeNull();
  });

  it('renders the brain icon in the gutter', () => {
    // The think row used to share the checklist icon, which read as
    // "todo list" next to actual TodoWrite rows. The brain icon is a
    // distinct visual that doesn't collide with any other tool kind.
    const { container } = render(ThinkingBlock, {
      props: {
        item: makeItem({
          kind: 'thinking',
          summary: 'reasoning content',
          payloadId: 'thinking-payload',
        }),
      },
    });
    const icon = container.querySelector('svg[data-icon]');
    expect(icon?.getAttribute('data-icon')).toBe('brain');
    expect(icon?.getAttribute('aria-label')).toBe('think');
  });

  it('tail-clamps the body to 3 lines via max-height when collapsed', () => {
    const { container } = render(ThinkingBlock, {
      props: {
        item: makeItem({
          kind: 'thinking',
          status: 'completed',
          summary: 'reasoning content',
          payloadId: 'thinking-payload',
        }),
      },
    });
    const body = container.querySelector('[data-testid="thinking-body"]');
    expect(body?.className).toMatch(/max-h-\[3lh\]/);
    expect(body?.className).toMatch(/overflow-hidden/);
  });

  it('removes the max-height cap once expanded', async () => {
    setBindingMock('GetPayloadData', async () => ({ data: 'full reasoning text' }));
    const { container, getByRole } = render(ThinkingBlock, {
      props: {
        item: makeItem({
          kind: 'thinking',
          status: 'completed',
          summary: 'reasoning content',
          payloadId: 'thinking-payload',
        }),
      },
    });

    await fireEvent.click(getByRole('button', { name: /toggle thinking block/i }));
    await tick();

    const body = container.querySelector('[data-testid="thinking-body"]');
    expect(body?.className).not.toMatch(/max-h-\[3lh\]/);
    expect(body?.className).not.toMatch(/overflow-hidden/);
  });

  it('stays tail-clamped through the streaming → settled boundary', async () => {
    // Exercises the streaming → completed transition through the pane
    // reducer so the live-tail path (pane.liveThinkingTailForItem) and
    // the smoother disposal path both fire. Without the `pane` prop
    // ThinkingBlock falls through to `item.summary` and never reads
    // the live tail, so the test silently bypassed the very code path
    // its name implies it covers.
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    try {
      const thinking = makeItem({
        id: 'think:0:0',
        kind: 'thinking',
        status: 'streaming',
        summary: '',
        payloadId: 'thinking-payload',
        updatedAt: 1,
      });
      const pane = await buildPane(makeThread({ id: 'thread-1' }), [thinking]);

      const { container, rerender } = render(ThinkingBlock, {
        props: { pane, item: pane.items[0] },
      });

      pane.applyItemDelta({
        threadId: 'thread-1',
        itemId: 'think:0:0',
        kind: 'thinking',
        delta: 'live delta text ',
        updatedAt: 2,
      });
      let safety = 500;
      while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
      await rerender({ pane, item: pane.items[0] });
      await tick();

      let body = container.querySelector('[data-testid="thinking-body"]');
      expect(body?.className).toMatch(/max-h-\[3lh\]/);
      expect(body?.textContent).toContain('live delta text');
      expect(pane.liveThinkingTailForItem('think:0:0')).not.toBeNull();

      pane.applyItemPatch({
        threadId: 'thread-1',
        itemId: 'think:0:0',
        kind: 'thinking',
        patch: { status: 'completed', updatedAt: 3 },
      });
      await rerender({ pane, item: pane.items[0] });
      await tick();

      body = container.querySelector('[data-testid="thinking-body"]');
      expect(body?.className).toMatch(/max-h-\[3lh\]/);
      expect(body?.textContent).toContain('live delta text');
      // Smoother disposed on the bare-status patch — body now reads
      // the persisted summary, not the (now-cleared) live tail.
      expect(pane.liveThinkingTailForItem('think:0:0')).toBeNull();
    } finally {
      __setSmoothingClockForTest(undefined);
    }
  });

  it('renders the live item summary as the body content during streaming', () => {
    const { container } = render(ThinkingBlock, {
      props: {
        item: makeItem({
          kind: 'thinking',
          status: 'streaming',
          summary: 'live delta text',
          payloadId: 'thinking-payload',
        }),
      },
    });
    const body = container.querySelector('[data-testid="thinking-body"]');
    expect(body?.textContent).toContain('live delta text');
  });

  it('exposes the copy button when there is non-empty content', () => {
    const { getByLabelText } = render(ThinkingBlock, {
      props: {
        item: makeItem({
          kind: 'thinking',
          summary: 'reasoning content',
          payloadId: 'thinking-payload',
        }),
      },
    });
    expect(getByLabelText('Copy thinking')).toBeInTheDocument();
  });

  it('omits the copy button while streaming', () => {
    const { queryByLabelText } = render(ThinkingBlock, {
      props: {
        item: makeItem({
          kind: 'thinking',
          status: 'streaming',
          summary: 'live partial reasoning',
          payloadId: 'thinking-payload',
        }),
      },
    });
    expect(queryByLabelText('Copy thinking')).toBeNull();
  });

  it('copies the full payload via the getter, even without an explicit expand', async () => {
    setBindingMock('GetPayloadData', async () => ({ data: 'loaded reasoning text' }));
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
      writable: true,
    });

    const { getByLabelText } = render(ThinkingBlock, {
      props: {
        item: makeItem({
          kind: 'thinking',
          summary: 'preview only',
          payloadId: 'thinking-payload',
        }),
      },
    });

    await fireEvent.click(getByLabelText('Copy thinking'));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith('loaded reasoning text'));
  });

  it('streams into the expanded full body and refetches current payload after collapse', async () => {
    const thinking = makeItem({
      id: 'think:0:0',
      kind: 'thinking',
      status: 'streaming',
      summary: 'seed',
      payloadId: 'thinking-payload',
      updatedAt: 1,
    });
    const pane = await buildPane(makeThread({ id: 'thread-1' }), [thinking]);
    const payloads = ['seed', 'seed live collapsed'];
    const getPayloadData = setBindingMock('GetPayloadData', async () => ({
      data: payloads.shift() ?? 'seed live collapsed',
    }));

    const { container, getByRole, rerender } = render(ThinkingBlock, {
      props: { pane, item: pane.items[0] },
    });

    await fireEvent.click(getByRole('button', { name: /toggle thinking block/i }));
    await waitFor(() => expect(container.querySelector('[data-testid="thinking-body"]')?.textContent).toBe('seed'));

    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'think:0:0',
      kind: 'thinking',
      delta: ' live',
      updatedAt: 2,
    });
    // Smoothing routes streaming text through a per-item rAF smoother;
    // flush it so the live-payload tail update (driven by the smoother
    // reveal callback) lands before the assertion.
    pane.__flushItemSmoothersForTest();
    await rerender({ pane, item: pane.items[0] });
    await tick();
    expect(container.querySelector('[data-testid="thinking-body"]')?.textContent).toBe('seed live');
    expect(getPayloadData).toHaveBeenCalledTimes(1);

    await fireEvent.click(getByRole('button', { name: /toggle thinking block/i }));
    await tick();
    expect(container.querySelector('[data-testid="thinking-body"]')?.className).toMatch(/max-h-\[3lh\]/);

    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'think:0:0',
      kind: 'thinking',
      delta: ' collapsed',
      updatedAt: 3,
    });
    pane.__flushItemSmoothersForTest();
    await rerender({ pane, item: pane.items[0] });
    await tick();
    expect(container.querySelector('[data-testid="thinking-body"]')?.textContent).toBe('seed live collapsed');

    await fireEvent.click(getByRole('button', { name: /toggle thinking block/i }));
    await waitFor(() => expect(container.querySelector('[data-testid="thinking-body"]')?.textContent).toBe('seed live collapsed'));
    expect(getPayloadData).toHaveBeenCalledTimes(2);
  });

  it('keeps the live tail visible when an expanded streaming payload read is stale', async () => {
    const thinking = makeItem({
      id: 'think:0:0',
      kind: 'thinking',
      status: 'streaming',
      summary: 'live tail',
      payloadId: 'thinking-payload',
      updatedAt: 1,
    });
    const pane = await buildPane(makeThread({ id: 'thread-1' }), [thinking]);
    setBindingMock('GetPayloadData', async () => ({ data: 'full payload before ' }));

    const { container, getByRole } = render(ThinkingBlock, {
      props: { pane, item: pane.items[0] },
    });

    await fireEvent.click(getByRole('button', { name: /toggle thinking block/i }));

    await waitFor(() => {
      expect(container.querySelector('[data-testid="thinking-body"]')?.textContent)
        .toBe('full payload before live tail');
    });
  });

  it('repairs a stale expanded streaming payload before appending the next live delta', async () => {
    const thinking = makeItem({
      id: 'think:0:0',
      kind: 'thinking',
      status: 'streaming',
      summary: 'live tail',
      payloadId: 'thinking-payload',
      updatedAt: 1,
    });
    const pane = await buildPane(makeThread({ id: 'thread-1' }), [thinking]);
    setBindingMock('GetPayloadData', async () => ({ data: 'full payload before ' }));

    const { container, getByRole, rerender } = render(ThinkingBlock, {
      props: { pane, item: pane.items[0] },
    });

    await fireEvent.click(getByRole('button', { name: /toggle thinking block/i }));
    await waitFor(() => {
      expect(container.querySelector('[data-testid="thinking-body"]')?.textContent)
        .toBe('full payload before live tail');
    });

    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'think:0:0',
      kind: 'thinking',
      delta: ' more',
      updatedAt: 2,
    });
    pane.__flushItemSmoothersForTest();
    await rerender({ pane, item: pane.items[0] });
    await tick();

    expect(container.querySelector('[data-testid="thinking-body"]')?.textContent)
      .toBe('full payload before live tail more');
  });

  it('renders smoother reveals past the 400-rune cap without calling __flushItemSmoothersForTest', async () => {
    // Discriminator for the past-400-runes regression. Drives the
    // per-item smoother via a fake rAF clock and asserts the rendered
    // DOM body text grows past `THINKING_TAIL_RUNES` (=400) — the
    // bound on `item.summary` — without the test-only flush helper
    // (which clears the live-tail map and forces a fallback read of
    // the bounded summary). Proves the full reactivity chain works
    // end-to-end:
    //   PerItemSmoother.onReveal
    //     → pane.itemLiveThinkingTail.set (SvelteMap)
    //     → pane.liveThinkingTailForItem(id) read inside
    //     → ThinkingBlock's `bodyText` $derived
    //     → DOM textContent
    // If this passes while item.summary stays <= 400, the collapsed
    // view is reading the live tail rather than the trimmed summary.
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    try {
      const thinking = makeItem({
        id: 'think:0:0',
        threadId: 'thread-1',
        kind: 'thinking',
        role: 'assistant',
        status: 'streaming',
        summary: '',
        payloadId: 'thinking-payload',
        updatedAt: 1,
      });
      const pane = await buildPane(makeThread({ id: 'thread-1' }), [thinking]);

      const { container } = render(ThinkingBlock, {
        props: { pane, item: pane.items[0] },
      });

      const words: string[] = [];
      for (let i = 0; i < 100; i++) words.push(`tok${String(i).padStart(2, '0')}`);
      // Sample at 30 / 60 / 100 to prove per-tick DOM growth — a
      // regression where Svelte batched updates to end-of-stream
      // would leave the early samples empty and only fill at the
      // final flush.
      const samples: number[] = [];
      const sampleAt = new Set([30, 60, 100]);
      for (let i = 0; i < words.length; i++) {
        pane.applyItemDelta({
          threadId: 'thread-1',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: words[i] + ' ',
          updatedAt: 100 + i,
        });
        clock.tickFrame(16);
        if (sampleAt.has(i + 1)) {
          flushSync();
          const body = container.querySelector('[data-testid="thinking-body"]');
          samples.push(body?.textContent?.length ?? 0);
        }
      }
      while (clock.pendingCount() > 0) clock.tickFrame(16);

      flushSync();
      await tick();

      expect(pane.items[0].summary.length).toBeLessThanOrEqual(400);
      const liveTail = pane.liveThinkingTailForItem('think:0:0');
      expect(liveTail).not.toBeNull();
      expect(liveTail!.length).toBeGreaterThan(400);

      const body = container.querySelector('[data-testid="thinking-body"]');
      expect(body).not.toBeNull();
      const rendered = body!.textContent ?? '';
      expect(rendered.length).toBeGreaterThan(400);
      // The DOM should mirror the live tail, not the trimmed summary.
      expect(rendered.length).toBe(liveTail!.length);
      // Mid-stream samples must show monotonic growth and must already
      // exceed the 400-rune cap by the final mid-stream checkpoint —
      // otherwise the DOM only catches up at the final flush (the
      // exact symptom the user reported).
      expect(samples).toHaveLength(3);
      expect(samples[0]).toBeGreaterThan(0);
      expect(samples[1]).toBeGreaterThan(samples[0]);
      expect(samples[2]).toBeGreaterThan(samples[1]);
      expect(samples[2]).toBeGreaterThan(400);
    } finally {
      __setSmoothingClockForTest(undefined);
    }
  });

  it('copies the refreshed completed payload when a row settles while expanded', async () => {
    const thinking = makeItem({
      id: 'think:0:0',
      kind: 'thinking',
      status: 'streaming',
      summary: 'seed',
      payloadId: 'thinking-payload',
      updatedAt: 1,
    });
    const pane = await buildPane(makeThread({ id: 'thread-1' }), [thinking]);
    const payloads = ['seed', 'seed final'];
    setBindingMock('GetPayloadData', async () => ({
      data: payloads.shift() ?? 'seed final',
    }));
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
      writable: true,
    });

    const { getByRole, getByLabelText, rerender } = render(ThinkingBlock, {
      props: { pane, item: pane.items[0] },
    });

    await fireEvent.click(getByRole('button', { name: /toggle thinking block/i }));
    await waitFor(() => expect(getByRole('button', { name: /toggle thinking block/i }).getAttribute('aria-expanded')).toBe('true'));

    pane.upsertItem({
      ...pane.items[0],
      status: 'completed',
      summary: 'seed final',
      updatedAt: 2,
    });
    await rerender({ pane, item: pane.items[0] });
    await fireEvent.click(getByLabelText('Copy thinking'));

    await waitFor(() => expect(writeText).toHaveBeenCalledWith('seed final'));
  });
});
