import { beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import { createThreadPane } from '../../stores/thread.svelte';
import {
  projectTurnCompleted,
  projectTurnStarted,
} from '../../stores/threadStatuses.svelte';
import { getToasts } from '../../stores/toast.svelte';
import MessageTimeline, { clearMessageTimelineScrollSnapshotsForTest } from './MessageTimeline.svelte';

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

function inlineAgentMeta(assistantMessageId: string, description: string): string {
  return JSON.stringify({
    toolName: 'Agent',
    assistant_message_id: assistantMessageId,
    is_inline_subagent: true,
    inline_subagent_group_id: assistantMessageId,
    input: { description, subagent_type: 'Explore' },
  });
}

describe('<MessageTimeline>', () => {
  beforeEach(async () => {
    resetBindingMocks();
    clearMessageTimelineScrollSnapshotsForTest();
    setBindingMock('GetSettings', async () => null);
    await loadSettings();
  });

  it('renders the empty state for a blank thread', async () => {
    const pane = await buildPane();
    const { getByText } = render(MessageTimeline, { props: { pane } });

    expect(getByText(/No messages yet/i)).toBeInTheDocument();
  });

  it('keeps active-turn status out of the virtualized history', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'hi' }),
    ]);
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: Date.now() - 3_000 });

    const { getByTestId, queryByTestId } = render(MessageTimeline, { props: { pane } });

    const scroll = getByTestId('message-timeline-scroll');
    expect(scroll.querySelectorAll('[data-testid="message-timeline-node"]')).toHaveLength(1);
    expect(queryByTestId('activity-rail-working')).toBeNull();
  });

  it('hides the empty state while a blank thread is working without mounting live UI', async () => {
    const pane = await buildPane();
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: Date.now() - 3_000 });

    const { queryByTestId, queryByText } = render(MessageTimeline, { props: { pane } });

    expect(queryByTestId('activity-rail-working')).toBeNull();
    expect(queryByText(/No messages yet/i)).toBeNull();
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

  it('dispatches terminal_interaction items to TerminalInteractionRow', async () => {
    // Phase 6: `terminal_interaction` items land in the timeline as
    // muted "Waited for background terminal" markers — a distinct
    // render path from AssistantMessage / ToolCallCard / the compaction
    // divider. Pinning the dispatch here keeps the MessageTimeline
    // switch honest as new kinds get added.
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'waited:pid-42:0:0',
        kind: 'terminal_interaction',
        role: 'assistant',
        summary: 'Waited for background terminal',
      }),
    ]);

    const { getByTestId } = render(MessageTimeline, { props: { pane } });

    const row = getByTestId('terminal-interaction-row');
    expect(row.textContent).toContain('Waited for background terminal');
  });

  it('updates a visible terminal wait carrier when its command completion arrives', async () => {
    const wait = makeItem({
      id: 'waited:pid-42:0:0',
      kind: 'terminal_interaction',
      role: 'assistant',
      status: 'running',
      summary: 'Waiting for background terminal',
      meta: JSON.stringify({ process_id: 'pid-42' }),
    });
    const completion = makeItem({
      id: 'complete-cmd-1',
      itemIndex: 1,
      kind: 'tool_completion',
      toolName: 'command_execution',
      completionOf: 'cmd-1',
      status: 'errored',
      summary: 'Command failed',
      meta: JSON.stringify({ process_id: 'pid-42', wait_carrier_id: wait.id }),
    });
    const completedWait = makeItem({
      ...wait,
      status: 'completed',
      summary: 'Waited for background terminal',
      updatedAt: 1,
    });
    const pane = await buildPane(makeThread({ provider: 'codex' }), [wait]);
    const { getByTestId, queryByTestId } = render(MessageTimeline, { props: { pane } });

    expect(getByTestId('terminal-interaction-row').textContent?.trim()).toBe(
      'Waiting for background terminal',
    );
    expect(queryByTestId('wait-group-children')).toBeNull();

    pane.upsertItems([completion, completedWait]);
    await tick();

    expect(getByTestId('terminal-interaction-row').textContent?.trim()).toBe(
      'Waited for background terminal',
    );
    expect(getByTestId('wait-group-children').textContent).toContain('Command failed');
  });

  it('renders notification rows without routing them through tool lifecycle cards', async () => {
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'notif-1',
        kind: 'notification',
        role: 'system',
        summary: 'Background command "sleep 10" completed',
      }),
    ]);

    const { getByTestId, queryByTestId } = render(MessageTimeline, { props: { pane } });

    expect(getByTestId('notification-row').textContent).toContain('Background command "sleep 10" completed');
    expect(queryByTestId('tool-call-card')).toBeNull();
  });

  it('renders one DiffFileBlock per file for multi-file tool_result rows', async () => {
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

    const { getAllByTestId, queryByTestId } = render(MessageTimeline, { props: { pane } });

    // No outer card chrome, no decision/completion badge, no chip
    // strip. Each file is its own DiffFileBlock keyed by data-file-path.
    const blocks = getAllByTestId('diff-file-block');
    const paths = blocks.map((el) => el.getAttribute('data-file-path'));
    expect(paths).toEqual(['src/a.ts', 'src/b.ts']);
    expect(queryByTestId('turn-diff-badge')).toBeNull();
  });

  it('renders proposed plans from payload-bearing tool rows', async () => {
    setBindingMock('GetPayloadData', async () => ({ data: '# Ship it' }));
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

  it('renders one wrapper per timeline node', async () => {
    // Virtualization is owned by virtua/svelte (`<Virtualizer>`); in production,
    // virtua mounts only the rows that fit the viewport plus an overscan
    // buffer. The test environment runs in happy-dom where all dimensions
    // are 0, so virtua's bufferSize-based windowing would render zero
    // rows; MessageTimeline switches virtua into ssrCount mode under
    // `import.meta.env.MODE === 'test'` so tests can assert on rendered
    // DOM. The contract verified here: every grouped node produces
    // exactly one `[data-testid="message-timeline-node"]` wrapper.
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

  it('renders Claude inline subagents inside a non-collapsible structural wrapper', async () => {
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'agent-1',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'Agent',
        status: 'running',
        summary: 'Agent: one',
        meta: inlineAgentMeta('assistant-1', 'First agent'),
      }),
      makeItem({
        id: 'agent-2',
        itemIndex: 1,
        kind: 'tool_call',
        toolName: 'Agent',
        status: 'running',
        summary: 'Agent: two',
        meta: inlineAgentMeta('assistant-1', 'Second agent'),
      }),
    ]);

    const { getByTestId, getAllByTestId, queryByTestId } = render(MessageTimeline, { props: { pane } });

    expect(getByTestId('inline-subagent-group-label').textContent).toContain('Running Agents');
    expect(getByTestId('inline-subagent-group-meta').textContent).toContain('2 agents');
    expect(queryByTestId('inline-subagent-group-toggle')).toBeNull();
    expect(getAllByTestId('subagent-group')).toHaveLength(2);
    expect(getAllByTestId('subagent-group-preview').map((node) => node.textContent?.trim())).toEqual([
      '└ Initializing...',
      '└ Initializing...',
    ]);
  });

  describe('windowed history', () => {
    // Build a pane driven directly (not via buildPane) so the test can
    // prime the initial-slice binding with its own items + hasMore flag.
    // The integration shape is stable: createThreadPane + switchThread
    // reads the paged binding we stub below.
    async function buildWindowedPane(opts: {
      items: ReturnType<typeof makeItem>[];
      hasMore?: boolean;
      oldestTurnIndex?: number;
    }): Promise<ReturnType<typeof createThreadPane>> {
      const { items, hasMore = false, oldestTurnIndex } = opts;
      const floor =
        oldestTurnIndex ?? (items.length > 0 ? items[0].turnIndex : -1);
      setBindingMock('SwitchThread', async () => {});
      setBindingMock('ListThreadSliceAround', async () => ({
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

    // Stage 1 redesign: the button was restyled off raw border-border
    // onto the subtle border + control radius + ghost-text hover pattern
    // the rest of the app uses. Pin the class list so a lazy edit can't
    // drift it back toward the old heavy chrome.
    it('Load older button uses the redesigned token classes', async () => {
      const pane = await buildWindowedPane({
        items: [makeItem({ id: 'a', turnIndex: 10 })],
        hasMore: true,
        oldestTurnIndex: 10,
      });

      const { getByTestId } = render(MessageTimeline, { props: { pane } });
      const button = getByTestId('load-older-messages');
      const cls = button.className;
      // Post-Button-migration the chrome comes from the primitive's
      // `secondary` variant — we still assert the redesigned design
      // tokens flow through (border-subtle at rest, muted fg, control
      // radius, hover-to-foreground color). hover:border-border is
      // expected on the secondary variant so we don't assert against
      // it here.
      expect(cls).toContain('border-border-subtle');
      expect(cls).toContain('rounded-[var(--radius-control)]');
      expect(cls).toContain('text-fg-muted');
      expect(cls).toContain('hover:text-fg');
    });

    it('clicking Load older invokes pane.loadOlder', async () => {
      const pane = await buildWindowedPane({
        items: [makeItem({ id: 'tail', turnIndex: 10 })],
        hasMore: true,
        oldestTurnIndex: 10,
      });
      const loadOlderSpy = vi.spyOn(pane, 'loadOlder').mockResolvedValue({
        status: 'noop',
        insertedBeforeWindow: false,
        insertedRows: false,
      });

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
  });

  describe('response divider integration', () => {
    it('renders a response divider before assistant text that follows tool activity', async () => {
      const pane = await buildPane(undefined, [
        makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'hi' }),
        makeItem({
          id: 'tool:0:0',
          itemIndex: 1,
          kind: 'tool_call',
          toolName: 'Bash',
          summary: 'ls',
        }),
        makeItem({
          id: 'text:0:0',
          itemIndex: 2,
          kind: 'assistant_text',
          summary: 'final answer',
        }),
      ]);

      const { getByTestId, container } = render(MessageTimeline, { props: { pane } });

      const divider = getByTestId('response-divider');
      expect(divider).toBeInTheDocument();
      // The single assistant_text in this turn is also the final one,
      // and there's no active turn — so the pill is rendered. The
      // labeled and unlabeled branches share a pinned wrapper height,
      // so toggling between them doesn't shift row geometry.
      expect(divider.getAttribute('data-final-response')).toBe('true');
      expect(divider.textContent).toContain('Response');

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

    it('renders zero response dividers when assistant text follows user text directly', async () => {
      const pane = await buildPane(undefined, [
        makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'hi' }),
        makeItem({ id: 'text:0:0', kind: 'assistant_text', summary: 'hi' }),
      ]);

      const { container, queryAllByTestId } = render(MessageTimeline, { props: { pane } });

      expect(queryAllByTestId('response-divider')).toHaveLength(0);
      // Pin the silent contract: a no-tool turn shows no Response cue
      // anywhere — neither a divider nor a stray pill marker.
      expect(container.querySelector('[data-final-response]')).toBeNull();
    });

    it('renders only one response divider for consecutive assistant text after tools', async () => {
      const pane = await buildPane(undefined, [
        makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'hi' }),
        makeItem({
          id: 'tool:0:0',
          itemIndex: 1,
          kind: 'tool_call',
          toolName: 'Bash',
          summary: 'ls',
        }),
        makeItem({
          id: 'text:0:0',
          itemIndex: 2,
          kind: 'assistant_text',
          summary: 'first paragraph',
        }),
        makeItem({
          id: 'text:0:1',
          itemIndex: 3,
          kind: 'assistant_text',
          summary: 'second paragraph',
        }),
      ]);

      const { queryAllByTestId } = render(MessageTimeline, { props: { pane } });

      expect(queryAllByTestId('response-divider')).toHaveLength(1);
    });

    it('shows the "Response" pill only on the final wire round of a settled turn', async () => {
      // Two wire rounds inside one logical turn: each round ends with
      // assistant_text after a tool. Only the SECOND round's divider
      // should carry the "Response" pill — the first round's divider
      // is just a plain line.
      const pane = await buildPane(undefined, [
        makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'hi' }),
        makeItem({
          id: 'tool:0:0',
          itemIndex: 1,
          kind: 'tool_call',
          toolName: 'Bash',
          summary: 'ls',
        }),
        makeItem({
          id: 'text:0:0',
          itemIndex: 2,
          kind: 'assistant_text',
          summary: 'mid-turn observation',
        }),
        makeItem({
          id: 'tool:0:1',
          itemIndex: 3,
          kind: 'tool_call',
          toolName: 'Bash',
          summary: 'cat README',
        }),
        makeItem({
          id: 'text:0:1',
          itemIndex: 4,
          kind: 'assistant_text',
          summary: 'final answer',
        }),
      ]);

      const { queryAllByTestId } = render(MessageTimeline, { props: { pane } });

      const dividers = queryAllByTestId('response-divider');
      expect(dividers).toHaveLength(2);
      expect(dividers[0].getAttribute('data-final-response')).toBe('false');
      expect(dividers[0].textContent).not.toContain('Response');
      expect(dividers[1].getAttribute('data-final-response')).toBe('true');
      expect(dividers[1].textContent).toContain('Response');

      // Pin the structural shape of each branch: unlabeled mode is one
      // full-width line (one `h-px` span), labeled mode is two
      // (line | gap | pill | gap | line). A regression that swaps the
      // conditional or accidentally renders both flank lines without
      // the pill would leave the empty-divider void back in the UI.
      expect(dividers[0].querySelectorAll('span.h-px')).toHaveLength(1);
      expect(dividers[1].querySelectorAll('span.h-px')).toHaveLength(2);

      // Pin the geometry contract: both branches share the same
      // wrapper height class. Without this, virtua re-measures to a
      // different height when an intermediate divider promotes to
      // "final" on settle — exactly the bug the row contract forbids.
      for (const divider of dividers) {
        const inner = divider.querySelector('div');
        expect(inner?.classList.contains('h-[1.625rem]')).toBe(true);
      }
    });

    it('suppresses the "Response" pill while the turn is still in flight', async () => {
      const thread = makeThread();
      const pane = await buildPane(thread, [
        makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'hi' }),
        makeItem({
          id: 'tool:0:0',
          itemIndex: 1,
          kind: 'tool_call',
          toolName: 'Bash',
          summary: 'ls',
        }),
        makeItem({
          id: 'text:0:0',
          itemIndex: 2,
          kind: 'assistant_text',
          summary: 'streaming so far',
        }),
      ]);
      // Mark turn 0 as in flight: more rounds may yet arrive, so the
      // current "last assistant_text" is not necessarily final.
      projectTurnStarted(thread.id, 'turn-0', 0, Date.now());

      const { getByTestId } = render(MessageTimeline, { props: { pane } });

      const divider = getByTestId('response-divider');
      expect(divider.getAttribute('data-final-response')).toBe('false');
      expect(divider.textContent).not.toContain('Response');

      // Once the turn settles, the pill materialises on the SAME
      // divider element — no new rows inserted, no row shell mutation,
      // just the inner branch swapping the continuous line for the
      // labeled "line | gap | pill | gap | line" structure. The
      // wrapper's pinned height (h-[1.625rem]) keeps the row geometry
      // identical across the swap, protecting the load-bearing "no
      // late transcript adornments" contract that the chat row
      // contract spells out.
      projectTurnCompleted(thread.id, 'turn-0');
      await tick();
      const settledDivider = getByTestId('response-divider');
      expect(settledDivider).toBe(divider);
      expect(settledDivider.getAttribute('data-final-response')).toBe('true');
      expect(settledDivider.textContent).toContain('Response');
    });

    it('marks the final assistant_text of every settled turn in a multi-turn thread', async () => {
      // Two completed turns, each ending with an assistant_text after a
      // tool. Both turns are settled (no active turn entry), so each
      // should get a "Response" pill on its own final divider.
      const pane = await buildPane(undefined, [
        makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'hi' }),
        makeItem({
          id: 'tool:0:0',
          itemIndex: 1,
          kind: 'tool_call',
          toolName: 'Bash',
          summary: 'ls',
        }),
        makeItem({
          id: 'text:0:0',
          itemIndex: 2,
          kind: 'assistant_text',
          summary: 'turn 0 final',
        }),
        makeItem({
          id: 'user:1',
          turnIndex: 1,
          itemIndex: 0,
          kind: 'user_text',
          role: 'user',
          summary: 'follow up',
        }),
        makeItem({
          id: 'tool:1:0',
          turnIndex: 1,
          itemIndex: 1,
          kind: 'tool_call',
          toolName: 'Bash',
          summary: 'cat',
        }),
        makeItem({
          id: 'text:1:0',
          turnIndex: 1,
          itemIndex: 2,
          kind: 'assistant_text',
          summary: 'turn 1 final',
        }),
      ]);

      const { queryAllByTestId } = render(MessageTimeline, { props: { pane } });

      const dividers = queryAllByTestId('response-divider');
      expect(dividers).toHaveLength(2);
      for (const divider of dividers) {
        expect(divider.getAttribute('data-final-response')).toBe('true');
        expect(divider.textContent).toContain('Response');
      }
    });

    it('treats an inline subagent group as tool activity for the trailing pill', async () => {
      // Common Claude turn shape: user → inline Agent (subagent) →
      // assistant_text summary. The subagent group counts as tool
      // activity (`nodeRole(group) === 'tool'`), so the trailing
      // assistant_text gets a divider AND the Response pill — exactly
      // like a Bash-then-text turn.
      const pane = await buildPane(undefined, [
        makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'investigate' }),
        makeItem({
          id: 'agent:0',
          itemIndex: 1,
          kind: 'tool_call',
          toolName: 'Agent',
          summary: 'Agent: explore',
          meta: inlineAgentMeta('msg-0', 'explore the auth module'),
        }),
        makeItem({
          id: 'text:0:0',
          itemIndex: 2,
          kind: 'assistant_text',
          summary: 'subagent finished — here is the answer',
        }),
      ]);

      const { getByTestId } = render(MessageTimeline, { props: { pane } });

      const divider = getByTestId('response-divider');
      expect(divider.getAttribute('data-final-response')).toBe('true');
      expect(divider.textContent).toContain('Response');
    });
  });

  describe('integration with utility helpers', () => {
    // The pure contracts live in `notificationFilter.test.ts` and
    // `subagentGrouping.test.ts`. The smoke tests below pin only the
    // wiring — that the filter is plumbed into the grouped-nodes derived,
    // and that the boundary classifier reaches the per-row wrapper class.

    it('drops a redundant task_notification from the rendered timeline', async () => {
      const pane = await buildPane(undefined, [
        makeItem({
          id: 'fg-1',
          itemIndex: 0,
          kind: 'tool_call',
          status: 'completed',
          toolName: 'Bash',
          summary: 'Bash: ls',
          meta: JSON.stringify({ task_id: 'T1' }),
        }),
        makeItem({
          id: 'task-notification:T1',
          itemIndex: 1,
          kind: 'notification',
          role: 'system',
          summary: 'Bash command "ls" completed',
          meta: JSON.stringify({ task_id: 'T1', source: 'task_notification' }),
        }),
      ]);

      const { queryAllByTestId } = render(MessageTimeline, { props: { pane } });

      expect(queryAllByTestId('notification-row')).toHaveLength(0);
    });

    it('applies the boundary mt-4 class to the per-row wrapper at a tool → text boundary', async () => {
      const pane = await buildPane(undefined, [
        makeItem({
          id: 'tool-1',
          itemIndex: 0,
          kind: 'tool_call',
          status: 'completed',
          toolName: 'Bash',
          summary: 'ls',
        }),
        makeItem({ id: 'text-1', itemIndex: 1, kind: 'assistant_text', summary: 'done' }),
      ]);

      const { container } = render(MessageTimeline, { props: { pane } });

      const row = container.querySelector('[data-row-index="1"]');
      if (!row) throw new Error('row 1 not found');
      expect(row.classList.contains('mt-4')).toBe(true);
    });
  });
});
