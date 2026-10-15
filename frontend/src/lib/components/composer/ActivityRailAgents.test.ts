import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import ActivityRailHost from '../../../test/mocks/ActivityRailTestHost.svelte';
import { buildPane } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { getToasts, removeToast } from '../../stores/toast.svelte';
import { setupEventListeners } from '../../stores/events';
import { noteThread } from '../../transport/entityIndex';
import { __resetActivityRailUiPrefsForTest } from '../../stores/liveTodoState.svelte';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import type { ThreadAgentRow, ThreadAgentStop } from '../../stores/bindings';
import { replaceQueueForThread, resetForTest as resetSendQueue, type QueueItem } from '../../stores/sendQueue.svelte';

const navigate = vi.hoisted(() => vi.fn(async () => {}));
vi.mock('../../stores/threadItemNavigation', () => ({ navigateToThreadItem: navigate }));
const opened = vi.hoisted(() => vi.fn());
vi.mock('../chat/agentThreadOrigin', async (importOriginal) => {
  const original = await importOriginal<typeof import('../chat/agentThreadOrigin')>();
  return {
    ...original,
    agentThreadOriginChip: (origin: { threadId: string }) => ({ label: 'chip', open: () => opened(origin.threadId) }),
  };
});

function row(token: string, over: Partial<ThreadAgentRow> = {}): ThreadAgentRow {
  return {
    token, callerThreadId: '', kind: 'spawn', threadId: `t-${token}`, title: `Agent ${token}`,
    provider: 'claude', state: 'running', createdAt: Date.now() - 65_000, depth: 0, ...over,
  } as ThreadAgentRow;
}

async function openTray(rows: (threadId: string) => ThreadAgentRow[]) {
  noteThread('thread-1', '', 2);
  const pane = await buildPane();
  const listed = rows(pane.threadId!);
  setBindingMock('ListThreadAgentRows', async () => ({ rows: listed, hidden: false }));
  const view = render(ActivityRailHost, { props: { pane } });
  const toggle = await view.findByTestId('activity-rail-agents-toggle');
  return { pane, view, toggle };
}

function toastTexts(): string[] {
  return getToasts().map((toast) => `${toast.type}: ${toast.message}`);
}

describe('the Agents segment', () => {
  let stopEvents = () => {};
  beforeEach(() => {
    resetBindingMocks();
    __resetActivityRailUiPrefsForTest();
    for (const toast of getToasts().slice()) removeToast(toast.id);
    setBindingMock('ListLiveBackgroundTasks', async () => []);
    setBindingMock('ListThreadWakes', async () => ({ rows: [], moreReminders: 0 }));
    // The real origin module pulls in the provider account store.
    setBindingMock('ListProviderAccounts', async () => []);
    setBindingMock('GetProviderLoginState', async () => null);
    setBindingMock('GetRateLimitsSnapshots', async () => []);
    navigate.mockClear();
    opened.mockClear();
    stopEvents = setupEventListeners();
  });
  afterEach(() => {
    setCompactLayoutForTest(false);
    resetSendQueue();
    stopEvents();
    vi.restoreAllMocks();
  });

  it('counts open rows, and its body lists them nested with title, provider, state, elapsed and what blocks them', async () => {
    const { view, toggle } = await openTray((caller) => [
      row('child', { callerThreadId: caller }),
      row('grand', { callerThreadId: 't-child', depth: 1, state: 'blocked', blockedOn: 'approval', blockedDetail: 'Bash: rm -rf build', provider: 'codex' }),
      row('send', { callerThreadId: caller, kind: 'send', state: 'finished', settledAt: Date.now() }),
    ]);
    expect(view.getByTestId('activity-rail-agents-count').textContent?.trim()).toBe('2');
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    await fireEvent.click(toggle);
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    expect(toggle.getAttribute('aria-controls')).toBe('activity-rail-agents-body');
    const rows = await view.findAllByTestId('agent-tray-row');
    expect(rows.map((r) => r.dataset.token)).toEqual(['child', 'grand', 'send']);
    const [child, grand, send] = rows;
    expect(within(child).getByTestId('agent-tray-row-title').textContent).toBe('Agent child');
    expect(within(child).getByTestId('agent-tray-row-provider').dataset.provider).toBe('claude');
    expect(within(child).getByTestId('agent-tray-row-state').textContent).toBe('running');
    expect(within(child).getByTestId('agent-tray-row-elapsed').textContent).toBe('1m 5s');
    expect(grand.style.marginLeft).toBe('0.75rem');
    expect(within(grand).getByTestId('agent-tray-row-blocked').textContent).toBe('Needs approval for Bash: rm -rf build');
    expect(within(send).getByTestId('agent-tray-row-state').textContent).toBe('finished');
    expect(within(send).queryByTestId('agent-tray-row-stop')).toBeNull();
    expect(view.getByTestId('activity-rail-agents-running-label').textContent?.trim()).toBe('1 running');
  });

  it('opens a row\'s digest from its details button, with the controls wired for keyboard and screen readers', async () => {
    const { view, toggle } = await openTray((caller) => [row('child', { callerThreadId: caller, activity: 'Reading the logs', lastMessage: 'On it.', lastMessageRole: 'assistant' })]);
    await fireEvent.click(toggle);
    const details = await view.findByTestId('agent-tray-row-details');
    expect(details.tagName).toBe('BUTTON');
    expect(details.getAttribute('aria-expanded')).toBe('false');
    await fireEvent.click(details);
    expect(details.getAttribute('aria-expanded')).toBe('true');
    const digest = view.getByTestId('agent-tray-row-digest');
    expect(digest.id).toBe(details.getAttribute('aria-controls'));
    expect(within(digest).getByTestId('agent-tray-row-digest-activity').textContent).toBe('Reading the logs');
    expect(within(digest).getByTestId('agent-tray-row-digest-last').textContent).toBe('On it.');
    expect(view.getByTestId('agent-tray-row-open').getAttribute('aria-label')).toBe('Open Agent child');
  });

  it('opens an open row\'s thread, and takes a settled row out on its click, scrolling to its answer here', async () => {
    const { pane, view, toggle } = await openTray((caller) => [
      row('child', { callerThreadId: caller, openThreadId: 't-child' }),
      row('done', { callerThreadId: caller, state: 'replied', settledAt: Date.now(), wakeItemId: 'user:wake-1' }),
      row('nested-done', { callerThreadId: 't-child', depth: 1, state: 'finished', settledAt: Date.now(), wakeItemId: 'user:wake-2', threadId: 't-nested' }),
    ]);
    await fireEvent.click(toggle);
    const rows = await view.findAllByTestId('agent-tray-row');
    await fireEvent.click(within(rows[0]).getByTestId('agent-tray-row-open'));
    expect(opened).toHaveBeenCalledWith('t-child');

    await fireEvent.click(within(rows[1]).getByTestId('agent-tray-row-open'));
    expect(navigate).toHaveBeenCalledWith(pane, pane.threadId, 'user:wake-1');
    // The answer to a nested row landed in its own caller, not here.
    await fireEvent.click(within(rows[2]).getByTestId('agent-tray-row-open'));
    expect(navigate).toHaveBeenCalledTimes(1);
    expect(opened).toHaveBeenLastCalledWith('t-nested');
    await waitFor(() => expect(view.getAllByTestId('agent-tray-row').map((r) => r.dataset.token)).toEqual(['child']));
  });

  it('folds a Stop\'s cascade into one toast and keeps a layer that failed on the row', async () => {
    const { pane, view, toggle } = await openTray((caller) => [row('child', { callerThreadId: caller })]);
    const stop = vi.fn(async (): Promise<ThreadAgentStop> => ({
      token: 'child', outcome: 'stopped', interrupted: true, background: 1, nested: 2,
      failures: ['stop its background work: the shell would not exit'],
    }) as ThreadAgentStop);
    setBindingMock('StopThreadAgentRow', stop);
    await fireEvent.click(toggle);
    await fireEvent.click(await view.findByTestId('agent-tray-row-stop'));
    await waitFor(() => expect(stop).toHaveBeenCalledWith(pane.threadId, 'child'));
    await waitFor(() => expect(toastTexts()).toEqual([
      'warning: Stopped Agent child: its turn, 1 background task and 2 agents of its own. Not stopped: stop its background work: the shell would not exit',
    ]));
    expect(view.getByTestId('agent-tray-row-stop-error').textContent).toContain('the shell would not exit');
  });

  it('reports a row that had already finished', async () => {
    const { view, toggle } = await openTray((caller) => [row('child', { callerThreadId: caller })]);
    setBindingMock('StopThreadAgentRow', async () => ({ token: 'child', outcome: 'ended' }) as ThreadAgentStop);
    await fireEvent.click(toggle);
    await fireEvent.click(await view.findByTestId('agent-tray-row-stop'));
    await waitFor(() => expect(toastTexts()).toEqual(['info: Already finished: Agent child.']));
  });

  it('stops every row with one call and one toast', async () => {
    const { pane, view, toggle } = await openTray((caller) => [row('a', { callerThreadId: caller }), row('b', { callerThreadId: caller })]);
    const stopAll = vi.fn(async (): Promise<ThreadAgentStop[]> => [
      { token: 'a', outcome: 'stopped', interrupted: true, background: 2, nested: 1 },
      { token: 'b', outcome: 'ended' },
    ] as ThreadAgentStop[]);
    setBindingMock('StopThreadAgentRows', stopAll);
    await fireEvent.click(toggle);
    await fireEvent.click(await view.findByTestId('activity-rail-agents-stop-all'));
    await waitFor(() => expect(stopAll).toHaveBeenCalledWith(pane.threadId));
    await waitFor(() => expect(toastTexts()).toEqual([
      'success: Stopped 1 agent (1 turn, 2 background tasks and 1 agent of its own). Already finished: 1 agent.',
    ]));
  });

  it('says on the row and its Stop when a paired computer cannot list or stop what a spawn there runs', async () => {
    const reason = 'that computer runs an older version: its background work and agents are not shown or stopped from here';
    const { view, toggle } = await openTray((caller) => [row('remote', { callerThreadId: caller, computerId: 'peer', computer: 'Laptop', nestedUnavailable: reason })]);
    await fireEvent.click(toggle);
    const r = await view.findByTestId('agent-tray-row');
    expect(within(r).getByTestId('agent-tray-row-computer').textContent).toBe('on Laptop');
    expect(within(r).getByTestId('agent-tray-row-degraded').textContent).toContain(reason);
    expect(r.getAttribute('title')).toContain(reason);
    expect(within(r).getByTestId('agent-tray-row-stop').getAttribute('title')).toContain(reason);
  });

  it('under compact, gives the title up to two lines and puts state, elapsed and actions on the next', async () => {
    setCompactLayoutForTest(true);
    const { view, toggle } = await openTray((caller) => [row('child', { callerThreadId: caller })]);
    await fireEvent.click(toggle);
    const r = await view.findByTestId('agent-tray-row');
    const title = within(r).getByTestId('agent-tray-row-title');
    expect(title.classList.contains('line-clamp-2')).toBe(true);
    const controls = within(r).getByTestId('agent-tray-row-controls');
    expect(within(controls).getByTestId('agent-tray-row-state')).toBeTruthy();
    expect(within(controls).getByTestId('agent-tray-row-stop')).toBeTruthy();
  });

  it('shows an incoming message with its sender and kind, opens the sender, and withdraws it on Stop', async () => {
    noteThread('thread-1', '', 2);
    const pane = await buildPane();
    setBindingMock('ListThreadAgentRows', async () => ({ rows: [], hidden: false }));
    replaceQueueForThread(pane.threadId!, [{
      id: 'q1', sendId: 'thread-request:tok', threadId: pane.threadId!, message: 'Check the changelog.',
      attachmentIds: [], enqueuedAt: Date.now() - 5_000, agentMessage: true,
      originThread: { computerId: '', computerName: '', threadId: 'sender', title: 'Sender', token: 'tok', kind: 'send' },
    } as QueueItem]);
    const view = render(ActivityRailHost, { props: { pane } });
    await waitFor(() => expect(view.getByTestId('activity-rail-agents-count').textContent).toBe('1'));
    await fireEvent.click(view.getByTestId('activity-rail-agents-toggle'));
    const r = await view.findByTestId('agent-incoming-row');
    expect(r.getAttribute('data-kind')).toBe('request');
    expect(within(r).getByTestId('agent-incoming-row-sender').textContent).toBe('chip');
    await fireEvent.click(within(r).getByTestId('agent-incoming-row-open'));
    expect(opened).toHaveBeenCalledWith('sender');
    const withdraw = vi.fn(async (): Promise<ThreadAgentStop> => ({ token: 'thread-request:tok', outcome: 'stopped', incoming: true }) as ThreadAgentStop);
    setBindingMock('StopThreadAgentIncoming', withdraw);
    await fireEvent.click(within(r).getByTestId('agent-incoming-row-stop'));
    await waitFor(() => expect(withdraw).toHaveBeenCalledWith(pane.threadId, 'thread-request:tok'));
    await waitFor(() => expect(toastTexts()).toEqual([
      'success: Withdrew the incoming request before this thread\'s agent read it.',
    ]));
  });

  it('is not shown for a thread waiting on nobody', async () => {
    noteThread('thread-1', '', 2);
    const pane = await buildPane();
    const read = setBindingMock('ListThreadAgentRows', async () => ({ rows: [], hidden: false }));
    const view = render(ActivityRailHost, { props: { pane } });
    await waitFor(() => expect(read).toHaveBeenCalled());
    expect(view.queryByTestId('activity-rail-agents-toggle')).toBeNull();
    expect(view.queryByTestId('activity-rail')).toBeNull();
  });
});
