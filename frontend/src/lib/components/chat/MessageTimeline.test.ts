import { describe, expect, it, beforeEach } from 'vitest';
import { render } from '@testing-library/svelte';
import MessageTimeline from './MessageTimeline.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import type { Thread, Item, PayloadMeta } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';

function thread(id = 'thread-1'): Thread {
  return {
    id,
    title: 'Test',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    interactionMode: 'default',
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
