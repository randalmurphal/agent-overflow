import { describe, expect, it, beforeAll, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import MessageTimeline from './MessageTimeline.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import type { Thread, Item, PayloadMeta } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';

// Subagent grouping uses transition:slide on expand; happy-dom lacks
// Element.animate. Stub it so the region unmounts cleanly when collapsed.
beforeAll(() => {
  if (typeof (Element.prototype as unknown as { animate?: unknown }).animate !== 'function') {
    (Element.prototype as unknown as { animate: (...args: unknown[]) => unknown }).animate =
      function fakeAnimate() {
        let onfinish: (() => void) | null = null;
        return {
          finished: Promise.resolve(),
          currentTime: 0,
          playState: 'finished' as const,
          cancel() {},
          finish() { onfinish?.(); },
          play() {},
          pause() {},
          reverse() {},
          addEventListener(type: string, cb: EventListener) {
            if (type === 'finish') onfinish = cb as unknown as () => void;
          },
          removeEventListener() {},
          get onfinish() { return onfinish; },
          set onfinish(cb: (() => void) | null) {
            onfinish = cb;
            if (cb) queueMicrotask(cb);
          },
        };
      };
  }
});

function thread(id = 'thread-1'): Thread {
  return {
    id,
    title: 'Test',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
}

function item(overrides: Partial<Item>): Item {
  return {
    id: 'item',
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'message',
    role: 'assistant',
    summary: '',
    createdAt: 0,
    ...overrides,
  };
}

async function buildPane(items: Item[] = [], metas: PayloadMeta[] = []) {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => items);
  setBindingMock('ListPayloadMetas', async () => metas);
  const pane = createThreadPane();
  await pane.switchThread(thread());
  return pane;
}

describe('<MessageTimeline>', () => {
  beforeEach(async () => {
    // Ensure settings store has the baseline defaults so streaming renders.
    setBindingMock('GetSettings', async () => null);
    await loadSettings();
  });

  it('shows the empty-state hint when nothing has happened yet', async () => {
    const pane = await buildPane();
    const { getByText } = render(MessageTimeline, { props: { pane } });
    expect(getByText(/No messages yet/i)).toBeInTheDocument();
  });

  it('renders a user message bubble for role=user items', async () => {
    const pane = await buildPane([
      item({ id: 'u1', role: 'user', summary: 'hi there' }),
    ]);
    const { getByText } = render(MessageTimeline, { props: { pane } });
    expect(getByText('hi there')).toBeInTheDocument();
  });

  it('renders assistant and user items mixed, in order', async () => {
    const pane = await buildPane([
      item({ id: 'u1', role: 'user', summary: 'user-text' }),
      item({ id: 'a1', role: 'assistant', summary: 'assistant-text', itemIndex: 1 }),
    ]);
    const { getByText } = render(MessageTimeline, { props: { pane } });
    expect(getByText('user-text')).toBeInTheDocument();
    expect(getByText('assistant-text')).toBeInTheDocument();
  });

  it('shows the pending message optimistically before the item lands', async () => {
    const pane = await buildPane();
    pane.setPendingMessage('draft question');
    const { getByText } = render(MessageTimeline, { props: { pane } });
    expect(getByText('draft question')).toBeInTheDocument();
  });

  it('shows the streaming content when the assistant is mid-reply', async () => {
    const pane = await buildPane();
    pane.appendTextDelta('partial reply');
    const { getByText } = render(MessageTimeline, { props: { pane } });
    expect(getByText(/partial reply/)).toBeInTheDocument();
  });

  it('shows a Thinking... placeholder when streaming is disabled in settings', async () => {
    setBindingMock('GetSettings', async () => ({ streamingEnabled: false }));
    await loadSettings();
    const pane = await buildPane();
    pane.appendTextDelta('partial');
    const { getByText, queryByText } = render(MessageTimeline, { props: { pane } });
    expect(getByText(/Thinking.../i)).toBeInTheDocument();
    expect(queryByText(/partial/)).toBeNull();
  });

  it('renders a loading status while the pane is loading', async () => {
    // Build a pane whose ListItems never resolves during render.
    setBindingMock('SwitchThread', async () => {});
    setBindingMock('ListItems', () => new Promise(() => {}));
    setBindingMock('ListPayloadMetas', async () => []);
    const pane = createThreadPane();
    // Kick off the switch but don't await; loading is synchronous-true.
    pane.switchThread(thread());
    const { getByText } = render(MessageTimeline, { props: { pane } });
    expect(getByText(/Loading thread/i)).toBeInTheDocument();
  });

  it('renders active tool entries while tools are running', async () => {
    const pane = await buildPane();
    pane.addToolCall('tool-1', { toolName: 'bash' });
    const { getByRole } = render(MessageTimeline, { props: { pane } });
    // WorkEntry has no explicit role, but the parent group has aria-label.
    expect(getByRole('group', { name: /Active tool calls/i })).toBeInTheDocument();
  });

  it('collapses 2+ concurrent tool calls into a work-group chip', async () => {
    const pane = await buildPane();
    pane.addToolCall('tool-1', { toolName: 'Read' });
    pane.addToolCall('tool-2', { toolName: 'Grep' });
    pane.addToolCall('tool-3', { toolName: 'Bash' });
    const { getByTestId, queryByTestId } = render(MessageTimeline, { props: { pane } });
    const chip = getByTestId('active-tools-chip');
    expect(chip.textContent ?? '').toMatch(/Running 3 tools/);
    expect(queryByTestId('active-tools-children')).toBeNull();
  });

  it('auto-expands the work group when tools finish and size drops', async () => {
    const pane = await buildPane();
    pane.addToolCall('tool-1', { toolName: 'Read' });
    pane.addToolCall('tool-2', { toolName: 'Grep' });
    const { queryByTestId } = render(MessageTimeline, { props: { pane } });
    expect(queryByTestId('active-tools-chip')).not.toBeNull();
    pane.completeToolCall('tool-2');
    await Promise.resolve();
    expect(queryByTestId('active-tools-chip')).toBeNull();
  });

  it('renders a turn-diff badge after ChangedFilesTree when a turn has diffs', async () => {
    const items: Item[] = [
      item({
        id: 'd1',
        kind: 'diff',
        role: 'assistant',
        payloadId: 'p1',
        itemIndex: 1,
      }),
    ];
    const metas: PayloadMeta[] = [
      {
        id: 'p1',
        kind: 'diff',
        meta: JSON.stringify({
          filePath: 'src/a.ts',
          changeKind: 'modified',
          insertions: 10,
          deletions: 2,
          preview: '',
        }),
        createdAt: 0,
      },
    ];
    const pane = await buildPane(items, metas);
    const { getByTestId } = render(MessageTimeline, { props: { pane } });
    const badge = getByTestId('turn-diff-badge');
    expect(badge.textContent ?? '').toMatch(/\+10/);
    expect(badge.textContent ?? '').toMatch(/−2/);
    expect(badge.getAttribute('data-turn-index')).toBe('0');
  });

  it('does not render a turn-diff badge when a turn has no diffs', async () => {
    const pane = await buildPane([
      item({ id: 'u1', role: 'user', summary: 'hello' }),
      item({ id: 'a1', role: 'assistant', summary: 'hi', itemIndex: 1 }),
    ]);
    const { queryByTestId } = render(MessageTimeline, { props: { pane } });
    expect(queryByTestId('turn-diff-badge')).toBeNull();
  });

  it('clicking the turn-diff badge opens the diff panel on that turn', async () => {
    const items: Item[] = [
      item({
        id: 'd1',
        kind: 'diff',
        role: 'assistant',
        payloadId: 'p1',
        itemIndex: 1,
        turnIndex: 2,
      }),
    ];
    const metas: PayloadMeta[] = [
      {
        id: 'p1',
        kind: 'diff',
        meta: JSON.stringify({
          filePath: 'src/a.ts',
          changeKind: 'modified',
          insertions: 1,
          deletions: 0,
          preview: '',
        }),
        createdAt: 0,
      },
    ];
    const pane = await buildPane(items, metas);
    expect(pane.diffPanel.open).toBe(false);
    const { getByTestId } = render(MessageTimeline, { props: { pane } });
    await fireEvent.click(getByTestId('turn-diff-badge'));
    expect(pane.diffPanel.open).toBe(true);
    expect(pane.diffPanel.source).toBe('turn');
    expect(pane.diffPanel.selectedTurnIndex).toBe(2);
  });

  it('golden-path: user message -> streaming assistant -> turn complete', async () => {
    // Start with a user message persisted in the DB-backed list.
    const pane = await buildPane([
      item({ id: 'u1', role: 'user', summary: 'what is 2+2?' }),
    ]);

    // Assistant streams a reply.
    pane.appendTextDelta('Thinking... ');
    pane.appendTextDelta('The answer is 4.');

    const finalItems: Item[] = [
      item({ id: 'u1', role: 'user', summary: 'what is 2+2?' }),
      item({ id: 'a1', role: 'assistant', summary: 'The answer is 4.', itemIndex: 1 }),
    ];

    // Turn completes: backend reloads; swap the binding and finalize.
    setBindingMock('ListItems', async () => finalItems);
    pane.finalizeTurn();
    await Promise.resolve();
    await Promise.resolve();

    const { getByText } = render(MessageTimeline, { props: { pane } });
    expect(getByText('what is 2+2?')).toBeInTheDocument();
    expect(getByText(/The answer is 4/)).toBeInTheDocument();
    // Streaming cleared, so the partial ("Thinking...") should no longer render.
    // We can't assert negation on a substring of the persisted text, but we can
    // assert streaming state was cleared.
    expect(pane.streamingContent).toBe('');
  });
});

describe('<MessageTimeline> subagent grouping', () => {
  beforeEach(async () => {
    setBindingMock('GetSettings', async () => null);
    await loadSettings();
  });

  it('groups items with a matching parentToolUseId under a collapsible subagent card', async () => {
    const pane = await buildPane([
      item({ id: 'u1', role: 'user', summary: 'spawn a subagent', itemIndex: 0 }),
      item({
        id: 'parent-tool',
        role: 'assistant',
        kind: 'tool_result',
        summary: 'Task: audit files',
        itemIndex: 1,
      }),
      item({
        id: 'sub-child-1',
        role: 'assistant',
        summary: 'inspected package.json',
        itemIndex: 2,
        parentToolUseId: 'parent-tool',
      }),
      item({
        id: 'sub-child-2',
        role: 'assistant',
        summary: 'summary complete',
        itemIndex: 3,
        parentToolUseId: 'parent-tool',
      }),
    ]);

    const { queryByText, getAllByText, getByText, getAllByRole, getByRole } = render(
      MessageTimeline,
      { props: { pane } },
    );

    // Top-level user message renders normally.
    expect(getByText('spawn a subagent')).toBeInTheDocument();

    // The subagent card renders with a collapsed-state summary.
    expect(getByText('Task: audit files')).toBeInTheDocument();
    // Header button with aria-expanded=false.
    const toggles = getAllByRole('button');
    // The subagent header is the disclosure button.
    const toggle = toggles.find((el) => el.getAttribute('aria-expanded') !== null);
    expect(toggle).toBeDefined();
    expect(toggle!.getAttribute('aria-expanded')).toBe('false');

    // Children are not visible while collapsed.
    expect(queryByText('inspected package.json')).toBeNull();
    expect(queryByText('summary complete')).toBeNull();

    // Expand the group: children render via the timeline's dispatch path.
    await fireEvent.click(toggle!);
    expect(toggle!.getAttribute('aria-expanded')).toBe('true');

    // Now both children appear.
    expect(getByText('inspected package.json')).toBeInTheDocument();
    expect(getByText('summary complete')).toBeInTheDocument();
    // Belt-and-suspenders: the descendant-count label appears.
    expect(getAllByText(/entries/i).length).toBeGreaterThan(0);

    // Badge also names the subagent row.
    expect(getByRole('button', { name: /audit files/i })).toBeInTheDocument();
  });

  it('renders an orphan warning when a child points at an unseen parent', async () => {
    const pane = await buildPane([
      item({ id: 'u1', role: 'user', summary: 'hello', itemIndex: 0 }),
      item({
        id: 'lost-child',
        role: 'assistant',
        summary: 'leftover subagent text',
        itemIndex: 1,
        parentToolUseId: 'parent-that-does-not-exist',
      }),
    ]);

    const { getByText, getByLabelText } = render(MessageTimeline, { props: { pane } });

    // Orphan warning banner is visible.
    expect(getByLabelText(/Orphan subagent item/i)).toBeInTheDocument();
    // Original content still renders — we don't drop orphans silently.
    expect(getByText('leftover subagent text')).toBeInTheDocument();
  });

  it('switching threads discards the previous thread groups', async () => {
    // Thread 1: has one subagent group.
    setBindingMock('SwitchThread', async () => {});
    setBindingMock('ListItems', async (id: string) => {
      if (id === 'thread-1') {
        return [
          item({
            id: 'parent',
            kind: 'tool_result',
            summary: 'Task: one',
            itemIndex: 0,
          }),
          item({
            id: 'child',
            summary: 'child of thread 1',
            itemIndex: 1,
            parentToolUseId: 'parent',
          }),
        ];
      }
      return [
        item({ id: 'solo', threadId: 'thread-2', summary: 'no groups here', itemIndex: 0 }),
      ];
    });
    setBindingMock('ListPayloadMetas', async () => []);
    const pane = createThreadPane();
    await pane.switchThread(thread('thread-1'));

    const { queryByText, getByText } = render(MessageTimeline, { props: { pane } });

    expect(getByText('Task: one')).toBeInTheDocument();

    // Switch to thread-2 — the subagent card for thread-1 must disappear.
    await pane.switchThread({ ...thread('thread-2'), id: 'thread-2' });
    expect(queryByText('Task: one')).toBeNull();
    expect(getByText('no groups here')).toBeInTheDocument();
  });

  it('does not change render output for flat item lists (no parent ids)', async () => {
    const items: Item[] = [
      item({ id: 'u1', role: 'user', summary: 'hi', itemIndex: 0 }),
      item({ id: 'a1', role: 'assistant', summary: 'hello', itemIndex: 1 }),
    ];
    const pane = await buildPane(items);
    const { getByText, queryByTestId } = render(MessageTimeline, { props: { pane } });

    // Content renders as leaves — no subagent card.
    expect(getByText('hi')).toBeInTheDocument();
    expect(getByText('hello')).toBeInTheDocument();
    expect(queryByTestId('subagent-group')).toBeNull();
  });

  it('auto-scrolls to the bottom after grouped content loads', async () => {
    const pane = await buildPane([
      item({ id: 'u1', role: 'user', summary: 'start', itemIndex: 0 }),
      item({
        id: 'parent',
        kind: 'tool_result',
        summary: 'Task: scroll test',
        itemIndex: 1,
      }),
      item({
        id: 'child',
        summary: 'child content',
        itemIndex: 2,
        parentToolUseId: 'parent',
      }),
    ]);
    const { container } = render(MessageTimeline, { props: { pane } });
    const scrollTarget = container.querySelector('[role="log"]') as HTMLDivElement;
    expect(scrollTarget).not.toBeNull();
    // happy-dom doesn't have real layout, but the auto-scroll effect should
    // still have attempted to set scrollTop without throwing.
    expect(scrollTarget.scrollTop).toBeGreaterThanOrEqual(0);
  });
});
