import { beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import { createThreadPane, type SettledTurn } from '../../stores/thread.svelte';
import { getToasts } from '../../stores/toast.svelte';
import MessageTimeline from './MessageTimeline.svelte';

function makeSettledTurn(overrides: Partial<SettledTurn> = {}): SettledTurn {
  return {
    turnId: 'turn-1',
    turnIndex: 0,
    startedAt: 0,
    completedAt: 12_000,
    stopReason: 'end_turn',
    assistantMessageId: null,
    tokenUsage: null,
    aborted: false,
    errorMessage: '',
    ...overrides,
  };
}

beforeAll(() => {
  if (typeof (Element.prototype as unknown as { animate?: unknown }).animate !== 'function') {
    (Element.prototype as unknown as { animate: (...args: unknown[]) => unknown }).animate =
      function fakeAnimate() {
        return {
          finished: Promise.resolve(),
          currentTime: 0,
          playState: 'finished' as const,
          cancel() {}, finish() {}, play() {}, pause() {}, reverse() {},
          addEventListener() {}, removeEventListener() {},
          onfinish: null, oncancel: null,
        };
      };
  }
});

describe('<MessageTimeline>', () => {
  beforeEach(async () => {
    resetBindingMocks();
    setBindingMock('GetSettings', async () => null);
    await loadSettings();
  });

  it('renders the empty state for a blank thread', async () => {
    const pane = await buildPane();
    const { getByText } = render(MessageTimeline, { props: { pane } });

    expect(getByText(/No messages yet/i)).toBeInTheDocument();
  });

  it('renders user, assistant, error, and compaction rows from unified items', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'hi' }),
      makeItem({ id: 'text:0:0', itemIndex: 1, kind: 'assistant_text', summary: 'hello' }),
      makeItem({ id: 'error:0:0', itemIndex: 2, kind: 'error', role: 'system', summary: 'boom' }),
      makeItem({ id: 'compact:1', turnIndex: 1, itemIndex: 0, kind: 'compaction', role: 'system', summary: 'Context compacted' }),
    ]);

    const { getByText } = render(MessageTimeline, { props: { pane } });

    expect(getByText('hi')).toBeInTheDocument();
    expect(getByText('hello')).toBeInTheDocument();
    expect(getByText('boom')).toBeInTheDocument();
    expect(getByText('Context compacted')).toBeInTheDocument();
  });

  it('renders changed-files and turn-diff summaries from tool-result payloads', async () => {
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'tool-1',
        kind: 'tool_call',
        summary: 'Edit src/a.ts',
        payloadId: 'payload-1',
        payloadKind: 'tool_result',
        payloadMeta: JSON.stringify({
          title: 'File change',
          inlineDiff: {
            availability: 'exact_patch',
            files: [
              { path: 'src/a.ts', kind: 'modified', insertions: 5, deletions: 2 },
              { path: 'src/b.ts', kind: 'added', insertions: 3, deletions: 0 },
            ],
          },
        }),
      }),
    ]);

    const { getByText, getByTestId } = render(MessageTimeline, { props: { pane } });

    expect(getByText(/2 files changed/i)).toBeInTheDocument();
    expect(getByTestId('turn-diff-badge').textContent ?? '').toContain('+8');
    expect(getByTestId('turn-diff-badge').textContent ?? '').toContain('−2');
  });

  it('renders proposed plans from payload-bearing tool rows', async () => {
    setBindingMock('GetPayloadData', async () => ({ data: '# Ship it', html: '<h1>Ship it</h1>' }));
    setBindingMock('HighlightMarkdown', async (md: string) => `<div>${md}</div>`);
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        summary: 'Plan',
        payloadId: 'plan-payload',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({
          title: 'Ship it',
          lineCount: 3,
          charCount: 12,
          preview: '# Ship it',
        }),
      }),
    ]);

    const { getAllByText } = render(MessageTimeline, { props: { pane } });

    expect(getAllByText('Ship it').length).toBeGreaterThan(0);
  });

  it('renders one wrapper per root timeline node', async () => {
    const items = Array.from({ length: 50 }, (_, i) =>
      makeItem({
        id: `text:${i}`,
        turnIndex: Math.floor(i / 10),
        itemIndex: i % 10,
        summary: `message ${i}`,
        createdAt: i,
      }),
    );
    const pane = await buildPane(undefined, items);
    const { container } = render(MessageTimeline, { props: { pane } });

    const wrappers = container.querySelectorAll('[data-testid="message-timeline-node"]');
    expect(wrappers.length).toBe(50);
  });

  it('rebuilds turn summaries incrementally via the pane (not per-upsert full scan)', async () => {
    // Regression pin for the task-2 refactor: MessageTimeline must source
    // turnDiffViews from the pane (pane.turnDiffViews) rather than a
    // component-local $derived that rescans pane.items on every upsert.
    // This test injects an item, then upserts a second diff into the same
    // turn, and checks that both contributions land in the rendered badge.
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'tool-1',
        turnIndex: 0,
        itemIndex: 0,
        kind: 'tool_call',
        payloadId: 'payload-1',
        payloadKind: 'diff',
        payloadMeta: JSON.stringify({
          filePath: 'src/a.ts',
          changeKind: 'modified',
          insertions: 3,
          deletions: 1,
          preview: '',
        }),
      }),
    ]);
    const { getByTestId, rerender } = render(MessageTimeline, { props: { pane } });

    expect(getByTestId('turn-diff-badge').textContent ?? '').toContain('+3');

    pane.upsertItem(makeItem({
      id: 'tool-2',
      turnIndex: 0,
      itemIndex: 1,
      kind: 'tool_call',
      payloadId: 'payload-2',
      payloadKind: 'diff',
      payloadMeta: JSON.stringify({
        filePath: 'src/b.ts',
        changeKind: 'added',
        insertions: 2,
        deletions: 0,
        preview: '',
      }),
    }));
    await rerender({ pane });

    expect(getByTestId('turn-diff-badge').textContent ?? '').toContain('+5');
  });

  describe('windowed history', () => {
    // Build a pane driven directly (not via buildPane) so the test can
    // prime ListRecentThreadItems with its own items + hasMore flag. The
    // integration shape is stable: createThreadPane + switchThread reads
    // the paged bindings we stub below.
    async function buildWindowedPane(opts: {
      items: ReturnType<typeof makeItem>[];
      hasMore?: boolean;
      oldestTurnIndex?: number;
    }): Promise<ReturnType<typeof createThreadPane>> {
      const { items, hasMore = false, oldestTurnIndex } = opts;
      const floor =
        oldestTurnIndex ?? (items.length > 0 ? items[0].turnIndex : -1);
      setBindingMock('SwitchThread', async () => {});
      setBindingMock('ListRecentThreadItems', async () => ({
        items,
        oldestTurnIndex: floor,
        hasMore,
      }));
      setBindingMock('ListRecentTurns', async () => []);
      const pane = createThreadPane();
      await pane.switchThread(makeThread());
      return pane;
    }

    it('renders the Load older button when pane.hasMoreHistory is true', async () => {
      const pane = await buildWindowedPane({
        items: [makeItem({ id: 'a', turnIndex: 10 })],
        hasMore: true,
        oldestTurnIndex: 10,
      });

      const { getByTestId } = render(MessageTimeline, { props: { pane } });

      const button = getByTestId('load-older-messages') as HTMLButtonElement;
      expect(button.textContent ?? '').toContain('Load older messages');
      expect(button.disabled).toBe(false);
    });

    it('hides the Load older button when pane.hasMoreHistory is false', async () => {
      const pane = await buildWindowedPane({
        items: [makeItem({ id: 'a' })],
        hasMore: false,
      });

      const { queryByTestId } = render(MessageTimeline, { props: { pane } });

      expect(queryByTestId('load-older-messages')).toBeNull();
    });

    it('clicking Load older invokes pane.loadOlder', async () => {
      const pane = await buildWindowedPane({
        items: [makeItem({ id: 'tail', turnIndex: 10 })],
        hasMore: true,
        oldestTurnIndex: 10,
      });
      const loadOlderSpy = vi.spyOn(pane, 'loadOlder').mockResolvedValue();

      const { getByTestId } = render(MessageTimeline, { props: { pane } });
      await fireEvent.click(getByTestId('load-older-messages'));
      await tick();

      expect(loadOlderSpy).toHaveBeenCalledTimes(1);
    });

    it('disables the button while loadOlder is in flight', async () => {
      const pane = await buildWindowedPane({
        items: [makeItem({ id: 'tail', turnIndex: 10 })],
        hasMore: true,
        oldestTurnIndex: 10,
      });
      // Hold ListItemsBeforeTurn open so the store's loadingOlder stays
      // true across the render we want to assert on.
      let release: (value: unknown) => void = () => {};
      const pending = new Promise((resolve) => { release = resolve; });
      setBindingMock('ListItemsBeforeTurn', async () => {
        await pending;
        return { items: [], oldestTurnIndex: 10, hasMore: false };
      });

      const { getByTestId, rerender } = render(MessageTimeline, { props: { pane } });
      void pane.loadOlder();
      // One synchronous task boundary is enough for loadingOlder=true to
      // flip before Svelte paints; rerender makes the $effect re-read
      // the getter.
      await tick();
      await rerender({ pane });

      const button = getByTestId('load-older-messages') as HTMLButtonElement;
      expect(button.disabled).toBe(true);
      expect(button.textContent ?? '').toContain('Loading');

      release({ items: [], oldestTurnIndex: 10, hasMore: false });
      await tick();
    });

    it('scroll intents route through pane.loadUntilItem before touching the DOM', async () => {
      // Covers both directions of the windowed scroll contract:
      //   1) The pane publishes a requestScrollToItem nonce.
      //   2) MessageTimeline's $effect picks that up and calls
      //      pane.loadUntilItem first so the target is guaranteed in
      //      the window before scrollIntoView runs.
      const pane = await buildWindowedPane({
        items: [makeItem({ id: 'a', turnIndex: 5 })],
      });
      const loadSpy = vi.spyOn(pane, 'loadUntilItem').mockResolvedValue(true);

      render(MessageTimeline, { props: { pane } });
      pane.requestScrollToItem('a');
      // Two ticks: one for the $effect to fire, one for the scrollToItem
      // awaits inside it to settle to the point where loadUntilItem was
      // called.
      await tick();
      await tick();

      expect(loadSpy).toHaveBeenCalledWith('a');
    });

    it('surfaces a warning toast when the scroll target no longer exists', async () => {
      const pane = await buildWindowedPane({
        items: [makeItem({ id: 'visible', turnIndex: 5 })],
      });
      vi.spyOn(pane, 'loadUntilItem').mockResolvedValue(false);
      const toastsBefore = getToasts().length;

      render(MessageTimeline, { props: { pane } });
      pane.requestScrollToItem('missing');
      await tick();
      await tick();

      const added = getToasts().slice(toastsBefore);
      expect(added.some((t) => t.type === 'warning')).toBe(true);
    });

    it('preserves the visible anchor row when Load older prepends history', async () => {
      // Scroll-preservation contract: the row the user was reading
      // (captured via prevScrollTop) must stay in the same viewport
      // position after the prepend. We simulate the DOM growth by
      // swapping scrollContainer.scrollHeight before and after
      // loadOlder and assert scrollTop was adjusted by the delta.
      const pane = await buildWindowedPane({
        items: [makeItem({ id: 'tail', turnIndex: 10 })],
        hasMore: true,
        oldestTurnIndex: 10,
      });
      // Simulate a backend response that prepends two older items and
      // advances the floor. The store mutates pane.items via a new
      // Array reference; the DOM grows proportionally.
      setBindingMock('ListItemsBeforeTurn', async () => ({
        items: [
          makeItem({ id: 'older-1', turnIndex: 8 }),
          makeItem({ id: 'older-2', turnIndex: 9 }),
        ],
        oldestTurnIndex: 8,
        hasMore: false,
      }));

      const { getByTestId, container } = render(MessageTimeline, { props: { pane } });
      const scroll = container.querySelector('[role="log"]') as HTMLElement;
      // Stage the "before" geometry. We'll flip scrollHeight after the
      // loadOlder resolves — that's how the delta is observed.
      const initialHeight = 2000;
      const grownHeight = 2600;
      Object.defineProperty(scroll, 'scrollHeight', {
        configurable: true,
        get: () => scrollHeightValue,
      });
      Object.defineProperty(scroll, 'clientHeight', {
        configurable: true,
        get: () => 600,
      });
      let scrollHeightValue = initialHeight;
      scroll.scrollTop = 0; // user is at the top, where Load Older lives

      // Swap scrollHeight the instant loadOlder runs. We tee off the
      // debounced store fetch by waiting for pane.items length to grow.
      const paneItemsLenBefore = pane.items.length;
      const clickPromise = fireEvent.click(getByTestId('load-older-messages'));

      // Give the macrotask a chance — the ListItemsBeforeTurn mock
      // resolves synchronously under microtask scheduling, so by the
      // time fireEvent.click awaits the handler we can swap the height.
      await clickPromise;
      scrollHeightValue = grownHeight;
      await tick();
      await tick();

      expect(pane.items.length).toBeGreaterThan(paneItemsLenBefore);
      // The delta (600 px) should have been re-applied to scrollTop so
      // the row the user was anchored on stays in the same viewport
      // position. With prevScrollTop=0, the new scrollTop == delta.
      expect(scroll.scrollTop).toBe(grownHeight - initialHeight);
    });

    it('does not snap to the bottom after Load older when the window fits the viewport', async () => {
      // Regression guard: the `suppressBottomAutoScroll` flag must
      // keep the near-bottom effect dormant across the prepend AND
      // across the tick where the flag flips back to false. Recompute
      // userNearBottom from the post-prepend scroll position so the
      // effect's re-run sees the refreshed state.
      const pane = await buildWindowedPane({
        items: [makeItem({ id: 'only', turnIndex: 5 })],
        hasMore: true,
        oldestTurnIndex: 5,
      });
      setBindingMock('ListItemsBeforeTurn', async () => ({
        items: [makeItem({ id: 'prepended', turnIndex: 4 })],
        oldestTurnIndex: 4,
        hasMore: false,
      }));

      const { getByTestId, container } = render(MessageTimeline, { props: { pane } });
      const scroll = container.querySelector('[role="log"]') as HTMLElement;
      // Short thread: total content fits the viewport (1000 content
      // px, 600 viewport px → scrollTop=0 IS near-bottom per the 100 px
      // threshold). A stale userNearBottom-true after the load would
      // scroll us to scrollHeight; the fix keeps scrollTop at the
      // handleLoadOlder-computed position.
      let scrollHeightValue = 1000;
      Object.defineProperty(scroll, 'scrollHeight', {
        configurable: true,
        get: () => scrollHeightValue,
      });
      Object.defineProperty(scroll, 'clientHeight', {
        configurable: true,
        get: () => 600,
      });
      // User is at the top when they click Load Older; fire a scroll
      // event so handleScroll runs once to flip userNearBottom to false
      // for a moment, then reset to the top to simulate "at top" which
      // for a short thread also counts as near-bottom (600-0-600=0).
      scroll.scrollTop = 0;
      await fireEvent.scroll(scroll);

      await fireEvent.click(getByTestId('load-older-messages'));
      scrollHeightValue = 1400;
      await tick();
      await tick();
      // Give the auto-scroll effect a rAF to fire if the suppress
      // failed. We don't actually poll rAF here — we just check that
      // scrollTop did not land at scrollHeight-clientHeight.
      const atBottom = scrollHeightValue - 600;
      expect(scroll.scrollTop).not.toBe(atBottom);
      expect(scroll.scrollTop).toBe(1400 - 1000); // delta-preserved
    });

    it('does not apply a scroll delta when Load Older returned no items', async () => {
      // Regression pin: if the backend returns an empty page (e.g.
      // hasMore was stale) AND a concurrent streaming upsert grew
      // the timeline height during the await, the old code would
      // misattribute the streaming height delta to the "prepend"
      // and shift scrollTop. The fix snapshots items.length and
      // skips the delta when nothing was prepended.
      const pane = await buildWindowedPane({
        items: [makeItem({ id: 'only', turnIndex: 10 })],
        hasMore: true,
        oldestTurnIndex: 10,
      });
      // Simulate an end-of-history response: no items, same floor.
      setBindingMock('ListItemsBeforeTurn', async () => ({
        items: [],
        oldestTurnIndex: 10,
        hasMore: false,
      }));

      const { getByTestId, container } = render(MessageTimeline, { props: { pane } });
      const scroll = container.querySelector('[role="log"]') as HTMLElement;
      let scrollHeightValue = 1000;
      Object.defineProperty(scroll, 'scrollHeight', {
        configurable: true,
        get: () => scrollHeightValue,
      });
      Object.defineProperty(scroll, 'clientHeight', {
        configurable: true,
        get: () => 600,
      });
      scroll.scrollTop = 200; // user mid-scroll

      const button = getByTestId('load-older-messages');
      const clickPromise = fireEvent.click(button);
      // Simulate a streaming upsert that grew the timeline DURING
      // the load-older await. In the real app this happens when an
      // agent turn is still emitting text; the scroll-height
      // increases but no items were prepended by loadOlder.
      scrollHeightValue = 1300;
      await clickPromise;
      await tick();
      await tick();

      // Items unchanged — loadOlder returned an empty page.
      expect(pane.items.length).toBe(1);
      // scrollTop MUST remain where the user left it; the streaming
      // delta of 300 px should NOT be re-applied.
      expect(scroll.scrollTop).toBe(200);
    });
  });

  describe('completion divider integration', () => {
    it('renders the divider before the matching assistant_text leaf', async () => {
      const pane = await buildPane(undefined, [
        makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'hi' }),
        makeItem({
          id: 'text:0:0',
          itemIndex: 1,
          kind: 'assistant_text',
          summary: 'final answer',
        }),
      ]);
      pane.settleTurn(
        makeSettledTurn({
          assistantMessageId: 'text:0:0',
          startedAt: 0,
          completedAt: 12_000,
        }),
      );

      const { getByTestId, container } = render(MessageTimeline, { props: { pane } });

      const divider = getByTestId('completion-divider');
      expect(divider).toBeInTheDocument();

      // Pin the reading order: divider sits BEFORE the assistant leaf.
      // The leaf is wrapped in a [data-item-id] div inside a
      // [data-testid="message-timeline-node"] wrapper; the divider
      // must appear in document order ahead of that wrapper.
      const assistantLeafWrapper = container.querySelector('[data-item-id="text:0:0"]');
      expect(assistantLeafWrapper).not.toBeNull();
      // Node-ordering compare: DOCUMENT_POSITION_FOLLOWING = 4.
      const following = divider.compareDocumentPosition(assistantLeafWrapper!) & 4;
      expect(following).toBe(4);
    });

    it('renders zero dividers when latestSettledTurn is null', async () => {
      const pane = await buildPane(undefined, [
        makeItem({ id: 'text:0:0', kind: 'assistant_text', summary: 'hi' }),
      ]);

      const { queryAllByTestId } = render(MessageTimeline, { props: { pane } });

      expect(queryAllByTestId('completion-divider')).toHaveLength(0);
    });

    it('renders zero dividers when latestSettledTurn.assistantMessageId is null', async () => {
      // A turn that aborted before any assistant_text was emitted carries
      // assistantMessageId=null. The divider lookup must no-op rather
      // than matching against a null and attaching itself to the first
      // leaf it sees.
      const pane = await buildPane(undefined, [
        makeItem({ id: 'text:0:0', kind: 'assistant_text', summary: 'partial' }),
      ]);
      pane.settleTurn(makeSettledTurn({ assistantMessageId: null, aborted: true }));

      const { queryAllByTestId } = render(MessageTimeline, { props: { pane } });

      expect(queryAllByTestId('completion-divider')).toHaveLength(0);
    });

    it('does not render the divider when no leaf id matches assistantMessageId', async () => {
      // Historical case: the turn projection has an assistantMessageId that
      // isn't present in the items list yet (delayed load, or an id that
      // got pruned). The divider stays hidden rather than attaching to
      // the first / last assistant leaf it finds.
      const pane = await buildPane(undefined, [
        makeItem({ id: 'text:0:0', kind: 'assistant_text', summary: 'a' }),
      ]);
      pane.settleTurn(
        makeSettledTurn({ assistantMessageId: 'text:9:9', startedAt: 0, completedAt: 1_000 }),
      );

      const { queryAllByTestId } = render(MessageTimeline, { props: { pane } });

      expect(queryAllByTestId('completion-divider')).toHaveLength(0);
    });

    it('shows "Interrupted" label when the settled turn is aborted', async () => {
      const pane = await buildPane(undefined, [
        makeItem({ id: 'text:0:0', kind: 'assistant_text', summary: 'hi' }),
      ]);
      pane.settleTurn(
        makeSettledTurn({
          assistantMessageId: 'text:0:0',
          aborted: true,
          stopReason: 'interrupted',
        }),
      );

      const { getByTestId } = render(MessageTimeline, { props: { pane } });

      expect(getByTestId('completion-divider-label').textContent).toContain('Interrupted');
    });

    it('shows "Error" label with inline errorMessage for an errored turn', async () => {
      const pane = await buildPane(undefined, [
        makeItem({ id: 'text:0:0', kind: 'assistant_text', summary: 'hi' }),
      ]);
      pane.settleTurn(
        makeSettledTurn({
          assistantMessageId: 'text:0:0',
          stopReason: 'error',
          errorMessage: 'rate_limited',
        }),
      );

      const { getByTestId } = render(MessageTimeline, { props: { pane } });

      expect(getByTestId('completion-divider-label').textContent).toContain('Error');
      expect(getByTestId('completion-divider-error').textContent).toBe('rate_limited');
    });
  });
});
