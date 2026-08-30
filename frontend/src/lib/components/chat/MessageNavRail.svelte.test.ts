// Wiring-level coverage for the nav rail component: baseline ticks
// arrive over the binding and render for unloaded history, the strip
// click routes to onJumpToItem with a real tick id, hover raises the
// preview card (locally for loaded ticks, over the RPC for unloaded
// ones), and the visibility gate holds. The pointer→index and merge
// math is covered in messageNavRail.test.ts, and the clipped-strip
// slide + arrow visibility in messageNavRailSync.test.ts. Arrow
// routing is untested here on purpose: arrows render only for an
// overflowing rail, and happy-dom's ResizeObserver never delivers a
// height.
import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { makeThread } from '../../../test/helpers/chat';
import type { Item } from '../../types/models';
import type { TimelineNode } from '../../utils/subagentGrouping';
import { createThreadPane, type ThreadPane } from '../../stores/thread.svelte';
import MessageNavRail from './MessageNavRail.svelte';
import {
  NAV_RAIL_TICK_LEFT_PX,
  TICK_FULL_WIDTH_PX,
  TICK_REST_WIDTH_PX,
} from './messageNavRail';

function item(partial: Partial<Item>): Item {
  return {
    id: 'i',
    threadId: 't',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'user_text',
    role: 'user',
    status: 'completed',
    summary: '',
    createdAt: 0,
    updatedAt: 0,
    ...partial,
  } as Item;
}

function leaf(partial: Partial<Item>): TimelineNode {
  return { kind: 'leaf', item: item(partial) } as TimelineNode;
}

function makePane(overrides: Partial<ThreadPane> = {}): ThreadPane {
  const pane = {
    threadId: 't1',
    switchGeneration: 0,
    hasMoreHistory: false,
    hasMoreNewer: false,
    items: [] as Item[],
    getItemById: (id: string) => pane.items.find((it) => it.id === id),
    ...overrides,
  } as unknown as ThreadPane;
  return pane;
}

const threeMessageNodes: TimelineNode[] = [
  leaf({ id: 'u1', summary: 'first ask', turnIndex: 0 }),
  leaf({ id: 'a1', kind: 'assistant_text', role: 'assistant', summary: 'reply one', turnIndex: 0, itemIndex: 1 }),
  leaf({ id: 'u2', summary: 'second ask', turnIndex: 1 }),
  leaf({ id: 'u3', summary: 'third ask', turnIndex: 2 }),
];

function renderRail(props: Record<string, unknown> = {}, baseline: unknown[] = []) {
  setBindingMock('GetThreadUserMessageTicks', async () => baseline);
  const onJumpToItem = vi.fn();
  const utils = render(MessageNavRail, {
    props: {
      pane: makePane(),
      nodes: threeMessageNodes,
      getListRef: () => undefined,
      onJumpToItem,
      ...props,
    },
  });
  return { ...utils, onJumpToItem };
}

describe('MessageNavRail', () => {
  it('renders a tick per reader-authored user message', () => {
    const { getByTestId } = renderRail();
    const rail = getByTestId('message-nav-rail');
    expect(rail).toBeTruthy();
    // 3 user messages → 3 ticks (the current flag is the marker attribute).
    expect(rail.querySelectorAll('[data-current]').length).toBe(3);
  });

  it('hides entirely for a lone fully-loaded message', () => {
    const { queryByTestId } = renderRail({ nodes: [leaf({ id: 'u1', summary: 'only' })] });
    expect(queryByTestId('message-nav-rail')).toBeNull();
  });

  it('a resting tick carries its width in its box and no inline transform', () => {
    const { getByTestId } = renderRail({ pane: makePane(), nodes: threeMessageNodes });
    const ticks = getByTestId('message-nav-rail').querySelectorAll<HTMLElement>('[data-current]');
    expect(ticks.length).toBe(3);
    for (const el of ticks) {
      expect(el.style.transform).toBe('');
      expect(el.style.width).toBe(`${TICK_REST_WIDTH_PX}px`);
    }
  });

  it('acquires through the resting tick width, then keeps the full fisheye width active', async () => {
    const { getByTestId } = renderRail();
    const strip = getByTestId('nav-rail-strip');

    expect(strip.style.left).toBe(`${NAV_RAIL_TICK_LEFT_PX}px`);
    expect(strip.style.width).toBe(`${TICK_REST_WIDTH_PX}px`);

    await fireEvent.mouseMove(strip);
    expect(strip.style.width).toBe(`${TICK_FULL_WIDTH_PX}px`);

    await fireEvent.mouseLeave(strip);
    expect(strip.style.width).toBe(`${TICK_REST_WIDTH_PX}px`);
  });

  it('renders baseline ticks for unloaded history, spliced under the window', async () => {
    // Two unloaded older messages + one loaded window message.
    const baseline = [
      { id: 'old1', turnIndex: 0, itemIndex: 0 },
      { id: 'old2', turnIndex: 1, itemIndex: 0 },
      { id: 'u9', turnIndex: 5, itemIndex: 0 },
    ];
    const loadedNodes = [leaf({ id: 'u9', summary: 'loaded ask', turnIndex: 5 })];
    const { getByTestId } = renderRail(
      {
        nodes: loadedNodes,
        pane: makePane({
          hasMoreHistory: true,
          items: [item({ id: 'u9', turnIndex: 5 })],
        }),
      },
      baseline,
    );
    await waitFor(() => {
      expect(getByTestId('message-nav-rail').querySelectorAll('[data-current]').length).toBe(3);
    });
  });

  it('does not refetch or blink the baseline when the thread object is replaced with the same id', async () => {
    // The 2026-08-19 shift+tab flash: mode.cycle → UpdateThreadMode →
    // syncThread replaces the pane's thread OBJECT with the same id, and
    // the baseline effect keyed on pane.threadId through a plain getter —
    // no equality cutoff — so every toggle cleared the whole-thread tick
    // list and refetched it, blinking the rail (below NAV_RAIL_MIN_TICKS
    // it unmounted outright). Needs a REAL pane: the fake-object panes
    // above cannot reproduce the $state invalidation.
    let fetches = 0;
    const baseline = [
      { id: 'old1', turnIndex: 0, itemIndex: 0 },
      { id: 'old2', turnIndex: 1, itemIndex: 0 },
    ];
    setBindingMock('GetThreadUserMessageTicks', async () => {
      fetches += 1;
      return baseline;
    });
    const pane = createThreadPane();
    pane.replaceThread(makeThread({ id: 't1', mode: 'chat' }));
    const { getByTestId } = render(MessageNavRail, {
      props: {
        pane,
        nodes: threeMessageNodes,
        getListRef: () => undefined,
        onJumpToItem: vi.fn(),
      },
    });
    const tickCount = () =>
      getByTestId('message-nav-rail').querySelectorAll('[data-current]').length;
    // The loaded window's 3 ticks render; the baseline read ran once.
    // (A real pane with no loaded items reports no window, so the
    // baseline does not splice — the FETCH COUNT is the pin here: the
    // incident's blink was this effect re-running, clearing the baseline
    // and fetching again.)
    await waitFor(() => expect(fetches).toBe(1));
    expect(tickCount()).toBe(3);

    // The mode toggle: same id, new object. No refetch, no tick change.
    pane.replaceThread(makeThread({ id: 't1', mode: 'plan' }));
    await tick();
    await tick();
    expect(fetches).toBe(1);
    expect(tickCount()).toBe(3);

    // A genuine thread switch still reloads the baseline.
    pane.replaceThread(makeThread({ id: 't2', mode: 'chat' }));
    await waitFor(() => expect(fetches).toBe(2));
  });

  it('routes a strip click to onJumpToItem with a tick id', async () => {
    const { getByTestId, onJumpToItem } = renderRail();
    await fireEvent.click(getByTestId('nav-rail-strip'));
    expect(onJumpToItem).toHaveBeenCalledWith('u1');
  });

  it('waits once on rail entry, then follows ticks immediately until leave', async () => {
    vi.useFakeTimers();
    const items = [
      item({ id: 'u1', summary: 'first ask' }),
      item({ id: 'a1', kind: 'assistant_text', role: 'assistant', summary: 'reply one', itemIndex: 1 }),
      item({ id: 'u2', summary: 'second ask', turnIndex: 1 }),
      item({ id: 'u3', summary: 'third ask', turnIndex: 2 }),
    ];
    try {
      const { getByTestId, queryByTestId } = renderRail({ pane: makePane({ items }) });
      const strip = getByTestId('nav-rail-strip');
      expect(queryByTestId('nav-rail-preview')).toBeNull();

      await fireEvent.mouseMove(strip);
      expect(queryByTestId('nav-rail-preview'), 'entry transit must stay quiet').toBeNull();
      await vi.advanceTimersByTimeAsync(119);
      await tick();
      expect(queryByTestId('nav-rail-preview')).toBeNull();
      await vi.advanceTimersByTimeAsync(1);
      await tick();
      expect(getByTestId('nav-rail-preview').textContent).toContain('first ask');
      expect(getByTestId('nav-rail-preview').textContent).toContain('reply one');

      // offsetY 28 maps past the vertical grace to the final tick.
      // Once the rail session is active, this update pays no second dwell.
      const moveToThird = new MouseEvent('mousemove', { bubbles: true });
      Object.defineProperty(moveToThird, 'offsetY', { value: 28 });
      strip.dispatchEvent(moveToThird);
      await tick();
      expect(getByTestId('nav-rail-preview').textContent).toContain('third ask');

      await fireEvent.mouseLeave(strip);
      expect(queryByTestId('nav-rail-preview')).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it('cancels an entry transit and charges the grace again on re-entry', async () => {
    vi.useFakeTimers();
    try {
      const { getByTestId, queryByTestId } = renderRail({
        pane: makePane({
          items: [
            item({ id: 'u1', summary: 'first ask' }),
            item({ id: 'u2', summary: 'second ask', turnIndex: 1 }),
          ],
        }),
      });
      const strip = getByTestId('nav-rail-strip');

      await fireEvent.mouseMove(strip);
      await vi.advanceTimersByTimeAsync(60);
      await fireEvent.mouseLeave(strip);
      await vi.advanceTimersByTimeAsync(120);
      await tick();
      expect(queryByTestId('nav-rail-preview')).toBeNull();

      await fireEvent.mouseMove(strip);
      await vi.advanceTimersByTimeAsync(119);
      await tick();
      expect(queryByTestId('nav-rail-preview')).toBeNull();
      await vi.advanceTimersByTimeAsync(1);
      await tick();
      expect(getByTestId('nav-rail-preview').textContent).toContain('first ask');
    } finally {
      vi.useRealTimers();
    }
  });

  it('an unloaded tick fetches its preview over the RPC after a hover dwell', async () => {
    const previewCalls: unknown[][] = [];
    setBindingMock('GetThreadTurnPreview', async (...args: unknown[]) => {
      previewCalls.push(args);
      return { userText: 'ancient ask', assistantText: 'ancient reply' };
    });
    const baseline = [
      { id: 'old1', turnIndex: 0, itemIndex: 0 },
      { id: 'old2', turnIndex: 1, itemIndex: 0 },
    ];
    // Nothing loaded at all: strip hover at railH 0 resolves index 0.
    const { getByTestId } = renderRail(
      { nodes: [], pane: makePane({ hasMoreHistory: true }) },
      baseline,
    );
    await waitFor(() => {
      expect(getByTestId('message-nav-rail').querySelectorAll('[data-current]').length).toBe(2);
    });
    await fireEvent.mouseMove(getByTestId('nav-rail-strip'));
    const card = await waitFor(() => getByTestId('nav-rail-preview'));
    expect(card.textContent).toContain('ancient ask');
    expect(card.textContent).toContain('ancient reply');
    expect(previewCalls).toEqual([['t1', 'old1']]);
  });
});
