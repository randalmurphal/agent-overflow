import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import CompactionReasoning from './CompactionReasoning.svelte';
import { __setSmoothingClockForTest } from '../../stores/thread.svelte';
import type { SmoothingClock } from '../../markdown/smoothing/PerItemSmoother';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

class FakeSmoothingClock implements SmoothingClock {
  private current = 0;
  private nextHandle = 1;
  private pending = new Map<number, () => void>();
  now(): number {
    return this.current;
  }
  schedule(cb: () => void): number {
    const h = this.nextHandle++;
    this.pending.set(h, cb);
    return h;
  }
  cancel(h: number): void {
    this.pending.delete(h);
  }
  tickFrame(ms: number): void {
    this.current += ms;
    const toFire = [...this.pending.values()];
    this.pending.clear();
    for (const cb of toFire) cb();
  }
  pendingCount(): number {
    return this.pending.size;
  }
}

function reasoningItem(overrides = {}) {
  return makeItem({
    kind: 'compaction_reasoning',
    role: 'assistant',
    summary: 'Reviewing the conversation so far.',
    payloadId: 'reasoning-payload',
    ...overrides,
  });
}

describe('<CompactionReasoning>', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  afterEach(() => {
    cleanup();
  });

  it('renders the compaction (list-collapse) icon and the "compact" label', () => {
    // Its own identity — NOT the brain/think row. Discriminated by kind at the
    // timeline, surfaced here as a distinct icon + label.
    const { container, getByTestId } = render(CompactionReasoning, {
      props: { item: reasoningItem() },
    });
    const icon = container.querySelector('svg[data-icon]');
    expect(icon?.getAttribute('data-icon')).toBe('compaction');
    expect(icon?.getAttribute('aria-label')).toBe('compact');
    expect(getByTestId('compaction-reasoning-label').textContent).toBe('compact');
  });

  it('renders the reasoning summary tail-clamped to 3 lines when collapsed', () => {
    const { container } = render(CompactionReasoning, {
      props: { item: reasoningItem({ status: 'completed' }) },
    });
    const body = container.querySelector('[data-testid="compaction-reasoning-body"]');
    expect(body?.textContent).toContain('Reviewing the conversation');
    expect(body?.className).toMatch(/max-h-\[3lh\]/);
    expect(body?.className).toMatch(/overflow-hidden/);
  });

  it('loads the full reasoning and removes the cap on expand', async () => {
    setBindingMock('GetPayloadData', async () => ({
      data: 'Reviewing the conversation so far to decide what still matters.',
    }));
    const { container, getByRole } = render(CompactionReasoning, {
      props: { item: reasoningItem({ status: 'completed' }) },
    });

    await fireEvent.click(getByRole('button', { name: /toggle compaction reasoning/i }));
    await tick();

    const body = container.querySelector('[data-testid="compaction-reasoning-body"]');
    await waitFor(() => expect(body?.textContent).toContain('decide what still matters'));
    expect(body?.className).not.toMatch(/max-h-\[3lh\]/);
  });

  it('smooths a streaming reasoning delta through the pane live tail', async () => {
    // The crux: a compaction_reasoning item must be smoothable
    // (isSmoothLiveContentKind includes it) so the row reads the per-pane live
    // tail and follows it,
    // exactly like thinking. Without that wiring the body would fall back to the
    // trimmed summary and never spring the tail.
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    try {
      const reasoning = reasoningItem({
        id: 'think:0:__ao_compaction_reasoning__:1',
        status: 'streaming',
        summary: '',
        updatedAt: 1,
      });
      const pane = await buildPane(makeThread({ id: 'thread-1' }), [reasoning]);

      const { container, rerender } = render(CompactionReasoning, {
        props: { pane, item: pane.items[0] },
      });

      pane.applyItemDelta({
        threadId: 'thread-1',
        itemId: reasoning.id,
        kind: 'compaction_reasoning',
        delta: 'streaming reasoning tail ',
        updatedAt: 2,
      });
      let safety = 500;
      while (clock.pendingCount() > 0 && safety-- > 0) clock.tickFrame(16);
      await rerender({ pane, item: pane.items[0] });
      await tick();

      const body = container.querySelector('[data-testid="compaction-reasoning-body"]');
      expect(body?.textContent).toContain('streaming reasoning tail');
      expect(body?.className).toMatch(/max-h-\[3lh\]/);
      expect(pane.liveThinkingTailForItem(reasoning.id)).not.toBeNull();

      // Settle parity with thinking: the smoother disposes on the
      // terminal patch but the tail is retained on this
      // content-consistent settle, so the collapsed body keeps its
      // exact string across the boundary.
      pane.applyItemPatch({
        threadId: 'thread-1',
        itemId: reasoning.id,
        kind: 'compaction_reasoning',
        patch: { status: 'completed', updatedAt: 3 },
      });
      await rerender({ pane, item: pane.items[0] });
      await tick();
      expect(pane.liveThinkingTailForItem(reasoning.id)).toBe('streaming reasoning tail ');
      expect(body?.textContent).toContain('streaming reasoning tail');
    } finally {
      __setSmoothingClockForTest(undefined);
    }
  });

  it('omits the copy button while streaming', () => {
    const { queryByLabelText } = render(CompactionReasoning, {
      props: { item: reasoningItem({ status: 'streaming' }) },
    });
    expect(queryByLabelText('Copy reasoning')).toBeNull();
  });
});
