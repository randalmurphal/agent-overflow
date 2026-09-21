import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import BackgroundTaskTrayDigest from './BackgroundTaskTrayDigest.svelte';
import { installTimelineScopeCapability, installPaneMocks, makeItem, makeThread } from '../../../test/helpers/chat';
import { flushMicrotasks } from '../../../test/helpers/threadPane';
import { createThreadPane, type ThreadPane } from '../../stores/thread.svelte';
import { registerPaneForTest, resetPanesForTest } from '../../stores/panes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { resetCompanionPanesForTest } from '../../stores/companionPanes.svelte';
import { __resetAgentPaneStateForTest } from '../../stores/agentPane.svelte';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { makeSettings } from '../../../test/helpers/settings';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import type { Item } from '../../types/models';
import type { TrayTask } from '../../utils/backgroundTray';
import type { PagedItems } from '../../../../bindings/agent-overflow/internal/store/models';

const THREAD_ID = 'thread-tray';

function claudeLaunch(overrides: Partial<Item> = {}): Item {
  return makeItem({
    id: 'launch-1',
    itemIndex: 0,
    kind: 'tool_call',
    toolName: 'Agent',
    role: 'assistant',
    status: 'running',
    isBackground: true,
    threadId: THREAD_ID,
    summary: 'Agent: exploring',
    payloadMeta: JSON.stringify({
      toolName: 'Agent',
      input: { description: 'Explore the parser', subagent_type: 'Explore' },
    }),
    ...overrides,
  });
}

function codexLaunch(): Item {
  return makeItem({
    id: 'spawn-1',
    itemIndex: 0,
    kind: 'tool_call',
    toolName: 'collab_agent',
    role: 'assistant',
    status: 'running',
    threadId: THREAD_ID,
    summary: 'spawn agent',
    meta: JSON.stringify({
      input: {
        tool: 'spawn_agent',
        receiverThreadIds: ['child-thread'],
        newAgentNickname: 'Scanner',
        prompt: 'Inspect renderer coverage',
      },
    }),
  });
}

function children(parentId: string): Item[] {
  return [
    makeItem({ id: 'prompt', itemIndex: 1, threadId: THREAD_ID, parentId, kind: 'user_text', summary: 'Explore the parser' }),
    makeItem({ id: 'thinking', itemIndex: 2, threadId: THREAD_ID, parentId, kind: 'thinking', summary: 'private reasoning' }),
    makeItem({ id: 'read-1', itemIndex: 3, threadId: THREAD_ID, parentId, kind: 'tool_call', toolName: 'Read', status: 'completed', summary: 'parser.ts' }),
    makeItem({ id: 'grep-1', itemIndex: 4, threadId: THREAD_ID, parentId, kind: 'tool_call', toolName: 'Grep', status: 'running', summary: 'tokenize' }),
    makeItem({ id: 'prose', itemIndex: 5, threadId: THREAD_ID, parentId, kind: 'assistant_text', summary: 'so far so good' }),
    makeItem({ id: 'main-after', itemIndex: 6, threadId: THREAD_ID, summary: 'main thread prose' }),
  ];
}

function taskFor(launch: Item): TrayTask {
  return { rowId: launch.id, anchor: launch, launch, completion: null, status: 'running', elapsedMs: 1_000, depth: 0 };
}

async function setup(items: Item[]): Promise<ThreadPane> {
  installPaneMocks(items);
  const pane = createThreadPane({ paneId: 'main' });
  registerPaneForTest('main', pane);
  await pane.switchThread(makeThread({ id: THREAD_ID }));
  setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 400 }]);
  return pane;
}

describe('<BackgroundTaskTrayDigest>', () => {
  beforeEach(async () => {
    installTimelineScopeCapability();
    resetBindingMocks();
    resetPanesForTest();
    resetPaneLayoutForTest();
    resetCompanionPanesForTest();
    __resetAgentPaneStateForTest();
    setBindingMock('GetSettings', async () => makeSettings({ activityRunDefault: 'expanded' }));
    setBindingMock('ListSubagentDescendants', async () => []);
    await loadSettings();
  });

  afterEach(() => {
    cleanup();
    __resetAgentPaneStateForTest();
    vi.restoreAllMocks();
  });

  it('shows a failed load without claiming there is no activity, and retries', async () => {
    const launch = claudeLaunch();
    const items = [launch, ...children(launch.id)];
    const pane = await setup(items);
    const report = vi.spyOn(console, 'error').mockImplementation(() => {});
    const page = await getBindingMock('ListThreadSliceAround')!(THREAD_ID, '', 200, { selection: { scopeRootId: launch.id, tools: true } }) as PagedItems;
    setBindingMock('SyncThreadWindow', async () => { throw new Error('Activity unavailable'); });
    const view = render(BackgroundTaskTrayDigest, { props: { pane, task: taskFor(launch), id: 'digest-1' } });
    await waitFor(() => expect(view.getByRole('alert')).toHaveTextContent('Activity unavailable'));
    expect(report).toHaveBeenCalledWith(expect.stringContaining('timeline window:'), expect.objectContaining({ message: 'Activity unavailable' }));
    expect(view.queryByText('No tool activity yet.')).toBeNull();
    setBindingMock('SyncThreadWindow', async () => ({ status: 'stale', page, generation: 'test-generation' }));
    await fireEvent.click(view.getByText('Retry'));
    await waitFor(() => expect(view.getByTestId('background-task-tray-row-digest').querySelector('[data-item-id="read-1"]')).toBeTruthy());
    expect(view.queryByRole('alert')).toBeNull();
  });

  for (const [provider, launch] of [['claude', claudeLaunch()], ['codex', codexLaunch()]] as const) {
    it(`renders a ${provider} agent's tools only under the tray row, nothing from the main thread`, async () => {
      const pane = await setup([launch, ...children(launch.id)]);
      const { getByTestId, queryByText } = render(BackgroundTaskTrayDigest, {
        props: { pane, task: taskFor(launch), id: 'digest-1' },
      });

      const digest = getByTestId('background-task-tray-row-digest');
      await waitFor(() => expect(digest.querySelectorAll('[data-item-id]').length).toBeGreaterThanOrEqual(2));
      const ids = [...digest.querySelectorAll('[data-item-id]')].map((el) => el.getAttribute('data-item-id'));
      expect(ids).toEqual(['read-1', 'grep-1']);
      expect(queryByText('private reasoning')).toBeNull();
      expect(queryByText('so far so good')).toBeNull();
      expect(queryByText('Explore the parser')).toBeNull();
      expect(queryByText('main thread prose')).toBeNull();
      expect(digest.querySelector('[data-testid="subagent-group-body"]')?.id).toBe('digest-1');
      expect(digest.getAttribute('data-scope-id')).toBe(launch.id);
    });
  }

  it('keeps its own tools when the host evicts them and releases on unmount', async () => {
    const launch = claudeLaunch();
    const pane = await setup([launch, ...children(launch.id)]);
    const view = render(BackgroundTaskTrayDigest, { props: { pane, task: taskFor(launch), id: 'digest-1' } });
    await waitFor(() => expect(view.getByTestId('background-task-tray-row-digest').querySelector('[data-item-id="read-1"]')).toBeTruthy());
    pane.removeItemById('read-1', THREAD_ID);
    await tick();
    expect(view.getByTestId('background-task-tray-row-digest').querySelector('[data-item-id="read-1"]')).toBeTruthy();
    view.unmount();
    expect(view.queryByTestId('background-task-tray-row-digest')).toBeNull();
  });

  it('scopes a Claude resume carrier to its transcript root', async () => {
    const launch = claudeLaunch({ meta: JSON.stringify({ subagentDescendantCount: 4 }) });
    const carrier = makeItem({
      id: 'carrier-1',
      itemIndex: 7,
      threadId: THREAD_ID,
      kind: 'tool_call',
      toolName: 'SendMessage',
      status: 'running',
      isBackground: true,
      summary: 'resume',
      meta: JSON.stringify({ task_id: 'task-resume', transcript_root_id: 'launch-1' }),
    });
    const pane = await setup([launch, ...children(launch.id), carrier]);
    const { getByTestId } = render(BackgroundTaskTrayDigest, {
      props: { pane, task: taskFor(carrier), id: 'digest-carrier' },
    });
    const digest = getByTestId('background-task-tray-row-digest');
    expect(digest.getAttribute('data-scope-id')).toBe('launch-1');
    await waitFor(() => expect(digest.querySelectorAll('[data-item-id]').length).toBeGreaterThanOrEqual(2));
    await tick();
  });

  it('loads tools without wholesale descendant hydration', async () => {
    const listDescendants = vi.fn(async () => []);
    setBindingMock('ListSubagentDescendants', listDescendants);
    const launch = claudeLaunch({ meta: JSON.stringify({ task_id: 'task-1', subagentDescendantCount: 9 }) });
    const pane = await setup([launch, ...children(launch.id)]);
    const view = render(BackgroundTaskTrayDigest, { props: { pane, task: taskFor(launch), id: 'digest-1' } });
    await waitFor(() => expect(view.getByTestId('background-task-tray-row-digest').querySelector('[data-item-id="read-1"]')).toBeTruthy());
    expect(listDescendants).not.toHaveBeenCalled();
  });

  it('loads an out-of-window scope without changing the host window', async () => {
    const launch = claudeLaunch();
    const pane = await setup([makeItem({ id: 'only-main', itemIndex: 20, threadId: THREAD_ID, summary: 'tail' })]);
    installPaneMocks([launch, ...children(launch.id)]);
    const { getByTestId, unmount } = render(BackgroundTaskTrayDigest, { props: { pane, task: taskFor(launch), id: 'digest-1' } });
    await waitFor(() => expect(getByTestId('background-task-tray-row-digest').querySelector('[data-item-id="read-1"]')).toBeTruthy());
    expect(pane.items.map(item => item.id)).toEqual(['only-main']);
    unmount();
    expect(pane.items.map(item => item.id)).toEqual(['only-main']);
  });

  it('replaces its window when the source thread changes even if row IDs repeat', async () => {
    const launch = claudeLaunch();
    const pane = await setup([launch, ...children(launch.id)]);
    const view = render(BackgroundTaskTrayDigest, { props: { pane, task: taskFor(launch), id: 'digest-1' } });
    await waitFor(() => expect(view.getByTestId('background-task-tray-row-digest').querySelector('[data-item-id="read-1"]')).toBeTruthy());
    const nextLaunch = claudeLaunch({ threadId: 'next-thread' });
    installPaneMocks([nextLaunch, makeItem({ id: 'next-tool', threadId: 'next-thread', parentId: launch.id, kind: 'tool_call', toolName: 'Read', summary: 'next.ts' })]);
    const sync = setBindingMock('SyncThreadWindow', async (threadId: string, request: { selection?: unknown }) => ({
      status: 'stale', generation: 'test-generation',
      page: await getBindingMock('ListThreadSliceAround')!(threadId, '', 200, { selection: request.selection }),
    }));
    await pane.switchThread(makeThread({ id: 'next-thread' }));
    await waitFor(() => expect(view.getByTestId('background-task-tray-row-digest').querySelector('[data-item-id="next-tool"]')).toBeTruthy());
    expect(view.getByTestId('background-task-tray-row-digest').querySelector('[data-item-id="read-1"]')).toBeNull();
    expect(sync.mock.calls.some(([threadId, request]) => threadId === 'next-thread'
      && (request as { selection?: { scopeRootId?: string } }).selection?.scopeRootId === launch.id)).toBe(true);
  });

  it('keeps a new scope boundary busy when the previous scope fetch finishes', async () => {
    const launch = claudeLaunch();
    const other = claudeLaunch({ id: 'other-launch' });
    const pane = await setup([launch, other, ...children(launch.id)]);
    const page = await getBindingMock('ListThreadSliceAround')!(THREAD_ID, '', 200, { selection: { scopeRootId: launch.id, tools: true } }) as PagedItems;
    setBindingMock('SyncThreadWindow', async (_thread: string, request: { selection: { scopeRootId: string } }) => ({
      status: 'stale', generation: 'test-generation',
      page: { ...page, hasMore: true, hasMoreOlder: true,
        scope: { root: request.selection.scopeRootId === launch.id ? launch : other, lifecycle: launch },
        items: page.items.map(item => ({ ...item, parentId: request.selection.scopeRootId })) },
    }));
    const pending: ((value: unknown) => void)[] = [];
    setBindingMock('ListItemsBeforeCursor', () => new Promise(resolve => pending.push(resolve)));
    const view = render(BackgroundTaskTrayDigest, { props: { pane, task: taskFor(launch), id: 'digest-1' } });
    await waitFor(() => expect(view.getByText('Load earlier activities')).toBeEnabled());
    await fireEvent.click(view.getByText('Load earlier activities'));
    await waitFor(() => expect(pending).toHaveLength(1));
    await view.rerender({ pane, task: taskFor(other), id: 'digest-1' });
    await waitFor(() => expect(view.getByText('Load earlier activities')).toBeEnabled());
    await fireEvent.click(view.getByText('Load earlier activities'));
    await waitFor(() => expect(pending).toHaveLength(2));
    expect(view.getByText('Loading…')).toBeDisabled();
    pending[0]({ ...page, items: [] });
    await flushMicrotasks();
    await tick();
    expect(view.getByText('Loading…')).toBeDisabled();
    pending[1]({ ...page, items: [] });
    await waitFor(() => expect(view.queryByText('Loading…')).toBeNull());
  });
});
