import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import ActivityRailHost from '../../../test/mocks/ActivityRailTestHost.svelte';
import { buildPane } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { getToasts, removeToast } from '../../stores/toast.svelte';
import { setupEventListeners } from '../../stores/events';
import { noteThread } from '../../transport/entityIndex';
import { emitWailsEvent } from '../../../test/mocks/wailsio-runtime';
import { __resetActivityRailUiPrefsForTest } from '../../stores/liveTodoState.svelte';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import type { ThreadAgentRow, ThreadAgentStop } from '../../stores/bindings';
import { replaceQueueForThread, resetForTest as resetSendQueue, type QueueItem } from '../../stores/sendQueue.svelte';

const navigate = vi.hoisted(() => vi.fn(async () => {}));
vi.mock('../../stores/threadItemNavigation', () => ({ navigateToThreadItem: navigate }));
const opened = vi.hoisted(() => vi.fn());
// The title each open names its thread by, which the opener's toast uses
// when the thread is gone by the time it is read.
const openedTitles = vi.hoisted(() => [] as string[]);
// Both tray openers make their placer on the click and hand it the resolved
// thread: the pane's agent thread companion.
type Place = () => (thread: { id: string }) => Promise<unknown>;
vi.mock('../chat/agentThreadOrigin', async (importOriginal) => {
  const original = await importOriginal<typeof import('../chat/agentThreadOrigin')>();
  const open = (origin: { threadId: string; title?: string }, place?: Place) => () => {
    opened(origin.threadId);
    openedTitles.push(origin.title ?? '');
    void place?.()({ id: origin.threadId });
  };
  return {
    ...original,
    agentThreadOriginChip: (origin: { threadId: string }, _viewer: unknown, place?: Place) => ({ label: 'chip', open: open(origin, place) }),
    agentThreadOpener: (origin: { threadId: string }, _viewer: unknown, place?: Place) => ({ computer: '', open: open(origin, place) }),
  };
});
const companion = vi.hoisted(() => vi.fn(async (_source: unknown, _thread: unknown) => {}));
vi.mock('../../stores/agentThreadCompanion', () => ({
  agentThreadCompanionPlacer: (source: unknown) => (thread: unknown) => companion(source, thread),
}));

function row(token: string, over: Partial<ThreadAgentRow> = {}): ThreadAgentRow {
  return {
    token, callerThreadId: '', kind: 'spawn', threadId: `t-${token}`, title: `Agent ${token}`,
    provider: 'claude', state: 'running', createdAt: Date.now() - 65_000, depth: 0, ...over,
  } as ThreadAgentRow;
}

async function openTray(rows: (threadId: string) => ThreadAgentRow[], pastRows: (threadId: string) => ThreadAgentRow[] = () => []) {
  noteThread('thread-1', '', 2);
  const pane = await buildPane();
  const listed = rows(pane.threadId!);
  const past = pastRows(pane.threadId!);
  setBindingMock('ListThreadAgentRows', async () => ({ rows: listed, hidden: false }));
  setBindingMock('ListThreadPastAgentRows', async (_id: string, after: { token: string }, limit: number) => {
    const start = after.token ? past.findIndex((r) => r.token === after.token) + 1 : 0;
    const page = past.slice(start, start + limit);
    const last = page[page.length - 1];
    return { rows: page, total: past.length, next: start + limit < past.length && last ? { settledAt: last.settledAt, token: last.token } : undefined };
  });
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
    setBindingMock('ListThreadPastAgentRows', async () => ({ rows: [], total: 0 }));
    navigate.mockClear();
    opened.mockClear();
    companion.mockClear();
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

  for (const compact of [false, true]) {
    it(`${compact ? 'under compact, ' : ''}shows a row whose worker keeps working after its request settled as open: its state, activity, blocked line and Stop, counted once`, async () => {
      setCompactLayoutForTest(compact);
      const { view, toggle } = await openTray((caller) => [
        row('live', {
          callerThreadId: caller, state: 'replied', settledAt: Date.now() - 60_000, worker: 'running', openThreadId: 't-live',
          activity: 'Watching the deploy', answer: 'The deploy started.', answerKind: 'reply',
        }),
        row('send', { callerThreadId: caller, kind: 'send', threadId: 't-live' }),
        row('waits', {
          callerThreadId: caller, state: 'finished', settledAt: Date.now() - 60_000, worker: 'blocked',
          blockedOn: 'approval', blockedDetail: 'Bash: deploy', blockedRoutedTo: 'Lead',
        }),
      ]);
      expect(view.getByTestId('activity-rail-agents-count').textContent?.trim()).toBe('2');
      await fireEvent.click(toggle);
      const [live, , waits] = await view.findAllByTestId('agent-tray-row');
      expect(live.dataset.settled).toBeUndefined();
      expect(within(live).getByTestId('agent-tray-row-state').textContent).toBe('running');
      expect(within(live).getByTestId('agent-tray-row-activity').textContent).toBe('Watching the deploy');
      expect(within(live).getByTestId('agent-tray-row-stop')).toBeTruthy();
      expect(within(waits).getByTestId('agent-tray-row-state').textContent).toBe('blocked');
      expect(within(waits).getByTestId('agent-tray-row-blocked').textContent).toBe('Needs approval for Bash: deploy, waiting on Lead');
      expect(within(waits).getByTestId('agent-tray-row-stop')).toBeTruthy();
      expect(view.getByTestId('activity-rail-agents-running-label').textContent?.trim()).toBe('1 running');
      // Its reply stays on the row.
      await fireEvent.click(within(live).getByTestId('agent-tray-row-details'));
      expect(within(live).getByTestId('agent-tray-row-digest').textContent).toContain('The deploy started.');
      // Open, its click opens the worker rather than the answer.
      await fireEvent.click(within(live).getByTestId('agent-tray-row-open'));
      expect(opened).toHaveBeenCalledWith('t-live');
      expect(navigate).not.toHaveBeenCalled();
    });
  }

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
    expect(companion).toHaveBeenCalledWith(pane, { id: 't-child' });

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
    expect(companion).toHaveBeenCalledWith(pane, { id: 'sender' });
    const withdraw = vi.fn(async (): Promise<ThreadAgentStop> => ({ token: 'thread-request:tok', outcome: 'stopped', incoming: true }) as ThreadAgentStop);
    setBindingMock('StopThreadAgentIncoming', withdraw);
    await fireEvent.click(within(r).getByTestId('agent-incoming-row-stop'));
    await waitFor(() => expect(withdraw).toHaveBeenCalledWith(pane.threadId, 'thread-request:tok'));
    await waitFor(() => expect(toastTexts()).toEqual([
      'success: Withdrew the incoming request before this thread\'s agent read it.',
    ]));
  });

  it('lists Past below the open rows, newest first, with no Stop, paged by Show more', async () => {
    const settledAt = Date.now() - 3 * 60_000;
    const { view, toggle } = await openTray(
      (caller) => [row('child', { callerThreadId: caller })],
      (caller) => Array.from({ length: 30 }, (_, index) => row(`p${index}`, {
        callerThreadId: caller, state: index === 1 ? 'errored' : 'finished', settledAt: settledAt - index,
        createdAt: settledAt - index - 65_000, kind: index === 2 ? 'ask' : 'spawn', answer: `Answer ${index}`,
      })),
    );
    await fireEvent.click(toggle);
    const pastToggle = await view.findByTestId('activity-rail-agents-past-toggle');
    expect(view.getByTestId('activity-rail-agents-past-count').textContent).toBe('30');
    expect(pastToggle.getAttribute('aria-expanded')).toBe('false');
    expect(view.getAllByTestId('agent-tray-row').map((r) => r.dataset.token)).toEqual(['child']);
    await fireEvent.click(pastToggle);
    expect(pastToggle.getAttribute('aria-expanded')).toBe('true');
    const list = await view.findByTestId('activity-rail-agents-past');
    await waitFor(() => expect(within(list).getAllByTestId('agent-tray-row')).toHaveLength(8));
    const rows = within(list).getAllByTestId('agent-tray-row');
    expect(rows.map((r) => r.dataset.token)).toEqual(['p0', 'p1', 'p2', 'p3', 'p4', 'p5', 'p6', 'p7']);
    for (const r of rows) {
      expect(r.dataset.past).toBe('true');
      expect(within(r).queryByTestId('agent-tray-row-stop')).toBeNull();
      expect(within(r).getByTestId('agent-tray-row-open-pane')).toBeTruthy();
    }
    expect(within(rows[0]).getByTestId('agent-tray-row-elapsed').textContent).toBe('3m ago');
    expect(within(rows[0]).getByTestId('agent-tray-row-elapsed').getAttribute('title')).toBe('Ran 1m 5s');
    expect(within(rows[1]).getByTestId('agent-tray-row-state').textContent).toBe('failed');
    expect(within(rows[2]).getByTestId('agent-tray-row-kind').textContent).toBe('ask');
    await fireEvent.click(within(rows[2]).getByTestId('agent-tray-row-details'));
    expect(within(rows[2]).getByTestId('agent-tray-row-digest').id).toBe('agent-tray-row-digest-past-p2');
    const more = view.getByTestId('activity-rail-agents-past-more');
    expect(more.textContent?.trim()).toBe('Show 20 more…');
    await fireEvent.click(more);
    await waitFor(() => expect(within(list).getAllByTestId('agent-tray-row')).toHaveLength(28));
    expect(view.getByTestId('activity-rail-agents-past-more').textContent?.trim()).toBe('Show 2 more…');
    await fireEvent.click(pastToggle);
    expect(view.queryByTestId('activity-rail-agents-past')).toBeNull();
  });

  it('shows for a thread whose agents are all past, with no count, and collapses Past when the body closes', async () => {
    const { view, toggle } = await openTray(() => [], (caller) => [row('p0', { callerThreadId: caller, state: 'finished', settledAt: Date.now() })]);
    // Nothing is open: the chip names the segment and counts nothing.
    expect(view.queryByTestId('activity-rail-agents-count')).toBeNull();
    expect(toggle.getAttribute('aria-label')).toBe('Agents');
    await fireEvent.click(toggle);
    expect(view.getByTestId('activity-rail-agents-running-label').textContent?.trim()).toBe('idle');
    expect(view.queryByTestId('activity-rail-agents-stop-all')).toBeNull();
    await fireEvent.click(await view.findByTestId('activity-rail-agents-past-toggle'));
    await view.findByTestId('activity-rail-agents-past');
    await fireEvent.click(toggle);
    await fireEvent.click(toggle);
    const pastToggle = await view.findByTestId('activity-rail-agents-past-toggle');
    expect(pastToggle.getAttribute('aria-expanded')).toBe('false');
    expect(view.queryByTestId('activity-rail-agents-past')).toBeNull();
  });

  it('opens any row\'s thread beside this one from Open in Agent Pane, an answered ask\'s the thread asked, named as that', async () => {
    const { pane, view, toggle } = await openTray(
      (caller) => [
        row('child', { callerThreadId: caller }),
        row('done', { callerThreadId: caller, state: 'replied', settledAt: Date.now(), wakeItemId: 'user:wake-1' }),
      ],
      (caller) => [row('asked', {
        callerThreadId: caller, kind: 'ask', threadId: 'scratch-copy', askedThreadId: 't-origin', title: 'Release notes',
        state: 'replied', settledAt: Date.now(), answer: 'It is complete.',
      })],
    );
    await fireEvent.click(toggle);
    const rows = await view.findAllByTestId('agent-tray-row');
    const openPane = within(rows[0]).getByTestId('agent-tray-row-open-pane');
    expect(openPane.getAttribute('title')).toBe('Open in agent pane');
    expect(openPane.getAttribute('aria-label')).toBe('Open in Agent Pane');
    await fireEvent.click(openPane);
    expect(companion).toHaveBeenLastCalledWith(pane, { id: 't-child' });
    // A settled row's icon opens its thread, not its answer, and keeps the row.
    await fireEvent.click(within(rows[1]).getByTestId('agent-tray-row-open-pane'));
    expect(companion).toHaveBeenLastCalledWith(pane, { id: 't-done' });
    expect(navigate).not.toHaveBeenCalled();
    expect(view.getAllByTestId('agent-tray-row').map((r) => r.dataset.token)).toEqual(['child', 'done']);
    await fireEvent.click(await view.findByTestId('activity-rail-agents-past-toggle'));
    const past = await within(await view.findByTestId('activity-rail-agents-past')).findByTestId('agent-tray-row');
    // The ask's copy is gone: the icon offers the thread asked, as that.
    const askedPane = within(past).getByTestId('agent-tray-row-open-pane');
    expect(askedPane.getAttribute('title')).toBe('Open the thread asked in agent pane');
    expect(askedPane.getAttribute('aria-label')).toBe('Open Release notes, the thread asked, in Agent Pane');
    await fireEvent.click(askedPane);
    expect(companion).toHaveBeenLastCalledWith(pane, { id: 't-origin' });
    expect(opened).not.toHaveBeenCalledWith('scratch-copy');
    // Its main click shows the answer, and opens nothing.
    const calls = companion.mock.calls.length;
    const main = within(past).getByTestId('agent-tray-row-open');
    expect(main.getAttribute('aria-label')).toBe('Show the answer from Release notes');
    await fireEvent.click(main);
    expect(await within(past).findByTestId('agent-tray-row-digest')).toHaveTextContent('It is complete.');
    expect(companion.mock.calls.length).toBe(calls);
  });

  it('opens a running ask\'s copy, its worker, and once the copy is gone shows its answer instead', async () => {
    let state: ThreadAgentRow[] = [];
    const { pane, view, toggle } = await openTray((caller) => {
      state = [row('ask', {
        callerThreadId: caller, kind: 'ask', threadId: 'ask-copy', openThreadId: 'ask-copy', askedThreadId: 't-origin',
        title: 'Release notes', activity: 'Reading CHANGELOG.md',
      })];
      return state;
    });
    await fireEvent.click(toggle);
    const askRow = await view.findByTestId('agent-tray-row');
    const main = within(askRow).getByTestId('agent-tray-row-open');
    expect(main.getAttribute('aria-label')).toBe('Open the copy asking Release notes');
    const openPane = within(askRow).getByTestId('agent-tray-row-open-pane');
    expect(openPane.getAttribute('aria-label')).toBe('Open in Agent Pane');
    await fireEvent.click(main);
    expect(companion).toHaveBeenLastCalledWith(pane, { id: 'ask-copy' });
    // A click that lands after the copy's delete, before the row's next
    // read, finds no copy: the opener's toast names it as the copy, never
    // as the thread asked, and opens nothing.
    expect(openedTitles.at(-1)).toBe('the copy asking Release notes');
    await fireEvent.click(openPane);
    expect(companion).toHaveBeenLastCalledWith(pane, { id: 'ask-copy' });
    expect(companion).not.toHaveBeenCalledWith(pane, { id: 't-origin' });

    // The copy answered and was deleted: the row's next read names no copy.
    // Its click shows the answer and keeps the row; its icon offers the
    // thread asked.
    setBindingMock('GetThreadAgentRows', async () => [{
      token: 'ask', listed: true, row: { ...state[0], openThreadId: '', activity: '', state: 'replied', settledAt: Date.now(), answer: 'It is complete.' },
    }]);
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId, id: 'ask' });
    await waitFor(() => expect(view.getByTestId('agent-tray-row').dataset.settled).toBe('true'));
    const settled = view.getByTestId('agent-tray-row');
    const calls = companion.mock.calls.length;
    await fireEvent.click(within(settled).getByTestId('agent-tray-row-open'));
    expect(await within(settled).findByTestId('agent-tray-row-digest')).toHaveTextContent('It is complete.');
    expect(companion.mock.calls.length).toBe(calls);
    expect(view.getAllByTestId('agent-tray-row').map((r) => r.dataset.token)).toEqual(['ask']);
    await fireEvent.click(within(settled).getByTestId('agent-tray-row-open-pane'));
    expect(companion).toHaveBeenLastCalledWith(pane, { id: 't-origin' });
  });

  it('offers Show in Sidebar on an owned worker of this computer only, and drops it once the worker has a row', async () => {
    const { view, toggle } = await openTray((caller) => [
      row('owned', { callerThreadId: caller, placement: 'owned' }),
      row('listed', { callerThreadId: caller, placement: 'standalone' }),
      row('archived', { callerThreadId: caller, placement: 'owned', archived: true }),
      row('remote', { callerThreadId: caller, placement: 'owned', computerId: 'peer', computer: 'Laptop' }),
      row('send', { callerThreadId: caller, kind: 'send', placement: 'owned' }),
      // An assistant's helper is outside every project: no placement to give.
      row('helper', { callerThreadId: caller }),
    ]);
    const place = setBindingMock('SetThreadPlacement', vi.fn(async (ids: string[], placement: string) => ids.map((id) => ({ id, placement }))));
    await fireEvent.click(toggle);
    const rows = await view.findAllByTestId('agent-tray-row');
    expect(rows.map((r) => Boolean(within(r).queryByTestId('agent-tray-row-show-in-sidebar')))).toEqual([true, false, false, false, false, false]);
    expect(within(rows[5]).getByTestId('agent-tray-row-open-pane')).toBeTruthy();
    const show = within(rows[0]).getByTestId('agent-tray-row-show-in-sidebar');
    expect(show.getAttribute('title')).toBe('Show in sidebar');
    expect(show.getAttribute('aria-label')).toBe('Show in Sidebar');
    await fireEvent.click(show);
    await waitFor(() => expect(place).toHaveBeenCalledWith(['t-owned'], 'standalone'));
    await waitFor(() => expect(within(rows[0]).queryByTestId('agent-tray-row-show-in-sidebar')).toBeNull());
    expect(toastTexts()).toEqual([]);
  });

  it('keeps Show in Sidebar and says why when the placement is refused', async () => {
    const { view, toggle } = await openTray(() => [], (caller) => [row('w', { callerThreadId: caller, placement: 'owned', state: 'finished', settledAt: Date.now() })]);
    setBindingMock('SetThreadPlacement', async () => { throw new Error('store: the thread is outside every project'); });
    await fireEvent.click(toggle);
    await fireEvent.click(await view.findByTestId('activity-rail-agents-past-toggle'));
    const past = await within(await view.findByTestId('activity-rail-agents-past')).findByTestId('agent-tray-row');
    await fireEvent.click(within(past).getByTestId('agent-tray-row-show-in-sidebar'));
    await waitFor(() => expect(toastTexts()).toEqual(['error: Could not show Agent w in the sidebar: store: the thread is outside every project']));
    expect(within(past).getByTestId('agent-tray-row-show-in-sidebar').hasAttribute('disabled')).toBe(false);
  });

  for (const compact of [false, true]) {
    it(`${compact ? 'under compact, ' : ''}shows a failed first read with Retry, which reads the list and Past again`, async () => {
      setCompactLayoutForTest(compact);
      noteThread('thread-1', '', 2);
      const pane = await buildPane();
      setBindingMock('ListThreadAgentRows', async () => { throw new Error('ledger offline'); });
      setBindingMock('ListThreadPastAgentRows', async () => { throw new Error('ledger offline'); });
      const view = render(ActivityRailHost, { props: { pane } });
      const toggle = await view.findByTestId('activity-rail-agents-toggle');
      expect(view.queryByTestId('activity-rail-agents-count')).toBeNull();
      await fireEvent.click(toggle);
      const error = await view.findByTestId('activity-rail-agents-error');
      expect(within(error).getByTestId('row-error-msg').textContent).toBe('Could not read agents: ledger offline');
      const pastError = view.getByTestId('activity-rail-agents-past-error');
      expect(within(pastError).getByTestId('row-error-msg').textContent).toBe('Could not read past agents: ledger offline');
      const retry = within(error).getByTestId('activity-rail-agents-error-retry');
      expect(retry.getAttribute('title')).toBe('Retry');
      expect(retry.getAttribute('aria-label')).toBe('Retry reading agents');
      expect(within(pastError).getByTestId('activity-rail-agents-past-error-retry').getAttribute('aria-label')).toBe('Retry reading past agents');

      setBindingMock('ListThreadAgentRows', async () => ({ rows: [row('child', { callerThreadId: pane.threadId! })], hidden: false }));
      await fireEvent.click(retry);
      await waitFor(() => expect(view.getAllByTestId('agent-tray-row').map((r) => r.dataset.token)).toEqual(['child']));
      expect(view.queryByTestId('activity-rail-agents-error')).toBeNull();
      expect(view.getByTestId('activity-rail-agents-past-error')).toBeTruthy();
      setBindingMock('ListThreadPastAgentRows', async () => ({ rows: [row('p0', { state: 'finished', settledAt: Date.now() })], total: 1 }));
      await fireEvent.click(view.getByTestId('activity-rail-agents-past-error-retry'));
      await waitFor(() => expect(view.getByTestId('activity-rail-agents-past-count').textContent).toBe('1'));
      expect(view.queryByTestId('activity-rail-agents-past-error')).toBeNull();
    });
  }

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
