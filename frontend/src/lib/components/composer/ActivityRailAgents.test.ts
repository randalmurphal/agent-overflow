import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import { tick } from 'svelte';
import ActivityRailHost from '../../../test/mocks/ActivityRailTestHost.svelte';
import { buildPane, makeThread } from '../../../test/helpers/chat';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { getToasts, removeToast } from '../../stores/toast.svelte';
import { setupEventListeners } from '../../stores/events';
import { noteThread } from '../../transport/entityIndex';
import { emitWailsEvent } from '../../../test/mocks/wailsio-runtime';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import type { ThreadAgentRow, ThreadAgentStop } from '../../stores/bindings';
import { replaceQueueForThread, type QueueItem } from '../../stores/sendQueue.svelte';
import { withEmptyDraftCleanup } from '../../stores/emptyDraftCleanup';
import { getPinnedBackend } from '../../transport/backends';
import { HOME_BACKEND } from '../../transport/backendKey';
import { REMOTE_BACKEND_UUID, resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { OBSERVE_SCOPES, grantBackendScopes, pairViewOnly, resetToLocalPage, revokeBackendScopes } from '../../../test/helpers/scopes';
import { SCOPES } from '../../transport/scopes';
import { formatCalendarDate, formatTimeOfDay } from '../../utils/format';

const BENCH_UUID = '11111111-2222-4333-8444-555555555555';
const UNGRANTED = 'Not granted to this device';
const FULL_SCOPES = SCOPES.filter((scope) => scope !== 'session' && scope !== 'host');

// An attached computer, with the grant its paired session carries.
async function stagePaired(id: string, backendId: string, name: string, scopes: readonly string[] = FULL_SCOPES) {
  stageBackend({ id, backendId, name });
  await grantBackendScopes(id, scopes);
}
function unstagePaired() {
  for (const id of ['studio', 'bench']) revokeBackendScopes(id);
  resetStagedBackends();
}

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

// The rail's 1 Hz clock on a fake interval, its time skewed forward by the
// test through Date.now. `tickClock` runs one tick and renders it.
function fakeRailClock(): { skew(ms: number): void; tickClock(): Promise<void> } {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
  const realNow = Date.now.bind(Date);
  let skew = 0;
  vi.spyOn(Date, 'now').mockImplementation(() => realNow() + skew);
  return {
    skew: (ms) => { skew += ms; },
    tickClock: async () => {
      await vi.advanceTimersByTimeAsync(1_000);
      await tick();
    },
  };
}

function toastTexts(): string[] {
  return getToasts().map((toast) => `${toast.type}: ${toast.message}`);
}

describe('the Agents segment', () => {
  let stopEvents = () => {};
  beforeEach(() => {
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
    stopEvents();
    vi.restoreAllMocks();
    vi.useRealTimers();
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
    it(`${compact ? 'under compact, ' : ''}shows what a spawned thread did as a subagent's row does: its tools and tokens beside its latest tool call while it runs, the final counts once it settled`, async () => {
      setCompactLayoutForTest(compact);
      const { view, toggle } = await openTray((caller) => [
        row('working', { callerThreadId: caller, toolCalls: 12, tokens: 18_240, activity: 'tail -f deploy.log' }),
        row('one', { callerThreadId: caller, toolCalls: 1 }),
        row('done', { callerThreadId: caller, state: 'finished', settledAt: Date.now(), toolCalls: 30, tokens: 1_240_000, activity: 'stale' }),
        row('send', { callerThreadId: caller, kind: 'send' }),
        row('blocked', { callerThreadId: caller, state: 'blocked', blockedOn: 'question', blockedDetail: 'Which branch?', toolCalls: 2, tokens: 900 }),
      ]);
      await fireEvent.click(toggle);
      const [working, one, done, send, blocked] = await view.findAllByTestId('agent-tray-row');
      expect(within(working).getByTestId('agent-tray-row-tools').textContent).toBe('12 tools');
      expect(within(working).getByTestId('agent-tray-row-tokens').textContent).toBe('18.2k tokens');
      expect(within(working).getByTestId('agent-tray-row-activity').textContent).toBe('tail -f deploy.log');
      expect(within(working).getByTestId('agent-tray-row-elapsed').textContent).toBe('1m 5s');
      expect(within(one).getByTestId('agent-tray-row-tools').textContent).toBe('1 tool');
      expect(within(one).queryByTestId('agent-tray-row-tokens')).toBeNull();
      expect(within(done).getByTestId('agent-tray-row-metrics').textContent?.replace(/\s+/g, ' ').trim()).toBe('30 tools · 1.2M tokens');
      expect(within(done).queryByTestId('agent-tray-row-activity')).toBeNull();
      expect(within(send).queryByTestId('agent-tray-row-metrics')).toBeNull();
      expect(within(send).queryByTestId('agent-tray-row-details-line')).toBeNull();
      expect(within(blocked).getByTestId('agent-tray-row-tools').textContent).toBe('2 tools');
      expect(within(blocked).getByTestId('agent-tray-row-blocked').textContent).toBe('Asks: Which branch?');
    });
  }

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

  it('says a late reply a paired computer dropped before it was collected arrived and when, with the first answer kept', async () => {
    const arrived = new Date(2026, 10, 16, 21, 5).getTime();
    const { view, toggle } = await openTray((caller) => [row('far', {
      callerThreadId: caller, state: 'finished', settledAt: Date.now(), computerId: 'pc', computer: 'Laptop',
      answer: 'The build is green.', answerKind: 'final', lateReplyExpiredAt: arrived,
    })]);
    await fireEvent.click(toggle);
    const far = await view.findByTestId('agent-tray-row');
    expect(within(far).getByTestId('agent-tray-row-state').textContent).toBe('finished, late reply expired');
    await fireEvent.click(within(far).getByTestId('agent-tray-row-details'));
    const digest = within(far).getByTestId('agent-tray-row-digest');
    expect(within(digest).getByTestId('agent-tray-row-digest-answer').textContent).toBe('The build is green.');
    const late = within(digest).getByTestId('agent-tray-row-digest-late-expired').textContent ?? '';
    expect(late).toContain(formatCalendarDate(arrived));
    expect(late).toContain(formatTimeOfDay(arrived));
    expect(late).toContain('expired there before it was collected');
  });

  it('offers the digest of a row whose only news is a late reply that expired', async () => {
    const { view, toggle } = await openTray((caller) => [row('far', {
      callerThreadId: caller, state: 'finished', settledAt: Date.now(), lateReplyExpiredAt: Date.now() - 60_000,
    })]);
    await fireEvent.click(toggle);
    const far = await view.findByTestId('agent-tray-row');
    await fireEvent.click(within(far).getByTestId('agent-tray-row-details'));
    expect(within(far).getByTestId('agent-tray-row-digest-late-expired')).toBeTruthy();
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

  it('waits out an emptied draft\'s delete of the thread before a Stop, and stops nothing it deleted', async () => {
    const { pane, view, toggle } = await openTray((caller) => [row('a', { callerThreadId: caller }), row('b', { callerThreadId: caller })]);
    const stop = vi.fn(async (): Promise<ThreadAgentStop> => ({ token: 'a', outcome: 'ended' }) as ThreadAgentStop);
    const stopAll = vi.fn(async (): Promise<ThreadAgentStop[]> => []);
    setBindingMock('StopThreadAgentRow', stop);
    setBindingMock('StopThreadAgentRows', stopAll);
    await fireEvent.click(toggle);
    for (const deleted of [false, true]) {
      stop.mockClear();
      stopAll.mockClear();
      let answer!: () => void;
      const answered = new Promise<void>((resolve) => { answer = resolve; });
      const cleanup = withEmptyDraftCleanup(pane.threadId!, async () => { await answered; return deleted; }, async () => {});
      await fireEvent.click((await view.findAllByTestId('agent-tray-row-stop'))[0]);
      await fireEvent.click(view.getByTestId('activity-rail-agents-stop-all'));
      await new Promise((resolve) => setTimeout(resolve, 5));
      expect(stop).not.toHaveBeenCalled();
      expect(stopAll).not.toHaveBeenCalled();
      answer();
      await cleanup;
      await new Promise((resolve) => setTimeout(resolve, 5));
      expect([deleted, stop.mock.calls.length, stopAll.mock.calls.length]).toEqual([deleted, deleted ? 0 : 1, deleted ? 0 : 1]);
    }
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

  it('says on the row when a paired computer settled a spawn at its turn end and cannot report rest', async () => {
    const { view, toggle } = await openTray((caller) => [
      row('older', { callerThreadId: caller, computerId: 'peer', computer: 'Laptop', state: 'finished', settledAt: Date.now(), worker: 'idle', restUnreported: true }),
      row('newer', { callerThreadId: caller, computerId: 'peer', computer: 'Laptop', state: 'finished', settledAt: Date.now(), worker: 'idle' }),
    ]);
    await fireEvent.click(toggle);
    const [older, newer] = await view.findAllByTestId('agent-tray-row');
    expect(within(older).getByTestId('agent-tray-row-rest-unreported').textContent).toBe(
      'Laptop runs an older version: it reported this when the turn ended and cannot say whether the thread came to rest',
    );
    expect(within(newer).queryByTestId('agent-tray-row-rest-unreported')).toBeNull();
  });

  it("shows a spawn's runtime: its turns' time, moving while a turn is open, never back within it, the server's once it ends", async () => {
    // The rail's 1 Hz tick and its refresh read the skewed clock.
    const clock = fakeRailClock();
    const first = await openTray((caller) => [
      row('worker', { callerThreadId: caller, runtimeMs: 4_000, turnOpen: true, turnStartedAt: 1_000 }),
    ]);
    let { pane, view } = first;
    await fireEvent.click(first.toggle);
    const elapsed = () => within(view.getByTestId('agent-tray-row')).getByTestId('agent-tray-row-elapsed').textContent;
    // Not the 65 s since the request: the time its turns ran.
    expect(elapsed()).toBe('4s');
    clock.skew(3_000);
    // 7 s, plus the real time the test took to get here.
    await clock.tickClock();
    expect(elapsed()).toMatch(/^[78]s$/);

    // A read while the turn still runs, its computer's clock behind this
    // one's: the row does not go back from what it showed.
    setBindingMock('GetThreadAgentRows', async () => [{
      token: 'worker', listed: true, row: row('worker', { callerThreadId: pane.threadId!, runtimeMs: 5_000, turnOpen: true, turnStartedAt: 1_000, activity: 'Reading' }),
    }]);
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId, id: 'worker' });
    await waitFor(() => expect(within(view.getByTestId('agent-tray-row')).getByTestId('agent-tray-row-activity').textContent).toBe('Reading'));
    expect(elapsed()).toMatch(/^[78]s$/);

    // Its turn ended at 6.5 s as its computer read it: that is its runtime,
    // shown as it is, below what the open turn's count reached, and it
    // stands still between turns.
    const settled = row('worker', { callerThreadId: pane.threadId!, runtimeMs: 6_500, worker: 'waiting' });
    let reads = 0;
    setBindingMock('GetThreadAgentRows', async () => {
      reads += 1;
      return [{ token: 'worker', listed: true, row: settled }];
    });
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId, id: 'worker' });
    await waitFor(() => expect(within(view.getByTestId('agent-tray-row')).getByTestId('agent-tray-row-state').textContent).toBe('waiting'));
    expect(elapsed()).toBe('6s');
    clock.skew(60_000);
    await clock.tickClock();
    expect(elapsed()).toBe('6s');
    // A later read of the same ended turn shows the same figure.
    const readsBefore = reads;
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId, id: 'worker' });
    await vi.waitFor(() => expect(reads).toBe(readsBefore + 1));
    await new Promise((resolve) => setImmediate(resolve));
    await clock.tickClock();
    expect(elapsed()).toBe('6s');

    // A new mount shows the same figure; the segment stays open across it.
    view.unmount();
    ({ pane, view } = await openTray((caller) => [row('worker', { callerThreadId: caller, runtimeMs: 6_500, worker: 'waiting' })]));
    await waitFor(() => expect(elapsed()).toBe('6s'));

    // Its next turn moves it on from its new read.
    setBindingMock('GetThreadAgentRows', async () => [{
      token: 'worker', listed: true, row: row('worker', { callerThreadId: pane.threadId!, runtimeMs: 9_000, turnOpen: true, turnStartedAt: 2_000 }),
    }]);
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId, id: 'worker' });
    await waitFor(() => expect(elapsed()).toBe('9s'));
  });

  it("starts a new turn's runtime from the server's figure though no read saw the last turn end", async () => {
    const clock = fakeRailClock();
    const { pane, view, toggle } = await openTray((caller) => [
      row('worker', { callerThreadId: caller, runtimeMs: 30_000, turnOpen: true, turnStartedAt: 1_000 }),
    ]);
    await fireEvent.click(toggle);
    const elapsed = () => within(view.getByTestId('agent-tray-row')).getByTestId('agent-tray-row-elapsed').textContent;
    clock.skew(10_000);
    await clock.tickClock();
    expect(elapsed()).toMatch(/^4[01]s$/);
    // That turn ended at 12 s of runtime and the next one has run 1 s;
    // no read saw the end between them.
    setBindingMock('GetThreadAgentRows', async () => [{
      token: 'worker', listed: true, row: row('worker', { callerThreadId: pane.threadId!, runtimeMs: 13_000, turnOpen: true, turnStartedAt: 2_000 }),
    }]);
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId, id: 'worker' });
    await waitFor(() => expect(elapsed()).toBe('13s'));
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
    expect(more.textContent?.trim()).toBe('Show 20 more of 22…');
    expect(view.queryByTestId('activity-rail-agents-past-less')).toBeNull();
    for (let round = 0; round < 2; round++) {
      await fireEvent.click(view.getByTestId('activity-rail-agents-past-more'));
      await waitFor(() => expect(within(list).getAllByTestId('agent-tray-row')).toHaveLength(28));
      expect(view.getByTestId('activity-rail-agents-past-more').textContent?.trim()).toBe('Show 2 more…');
      // Show less goes back to the first page; the count stays.
      await fireEvent.click(view.getByTestId('activity-rail-agents-past-less'));
      await waitFor(() => expect(within(list).getAllByTestId('agent-tray-row')).toHaveLength(8));
      expect(within(list).getAllByTestId('agent-tray-row').map((r) => r.dataset.token)).toEqual(['p0', 'p1', 'p2', 'p3', 'p4', 'p5', 'p6', 'p7']);
      expect(view.queryByTestId('activity-rail-agents-past-less')).toBeNull();
      expect(view.getByTestId('activity-rail-agents-past-more').textContent?.trim()).toBe('Show 20 more of 22…');
      expect(view.getByTestId('activity-rail-agents-past-count').textContent).toBe('30');
    }
    await fireEvent.click(pastToggle);
    expect(view.queryByTestId('activity-rail-agents-past')).toBeNull();
  });

  it('says how many Past rows are left beside the next page only when more than a page is left', async () => {
    const ledger = (count: number) => (caller: string) => Array.from({ length: count }, (_, index) => row(`p${index}`, {
      callerThreadId: caller, state: 'finished', settledAt: Date.now() - index,
    }));
    const { view, toggle } = await openTray(() => [], ledger(8 + 20 + 1));
    await fireEvent.click(toggle);
    await fireEvent.click(await view.findByTestId('activity-rail-agents-past-toggle'));
    const more = await view.findByTestId('activity-rail-agents-past-more');
    expect(more.textContent?.trim()).toBe('Show 20 more of 21…');
    await fireEvent.click(more);
    await waitFor(() => expect(view.getByTestId('activity-rail-agents-past-more').textContent?.trim()).toBe('Show 1 more…'));
    view.unmount();

    // The segment stays open across the mount.
    const exact = await openTray(() => [], ledger(8 + 20));
    await fireEvent.click(await exact.view.findByTestId('activity-rail-agents-past-toggle'));
    expect((await exact.view.findByTestId('activity-rail-agents-past-more')).textContent?.trim()).toBe('Show 20 more…');
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

  for (const compact of [false, true]) {
    it(`${compact ? 'under compact, ' : ''}offers Show in Sidebar on an owned worker on a paired computer this window is attached to, and places it there`, async () => {
      setCompactLayoutForTest(compact);
      await stagePaired('studio', REMOTE_BACKEND_UUID, 'Studio');
      await stagePaired('bench', BENCH_UUID, 'Bench');
      try {
        const { view, toggle } = await openTray((caller) => [
          row('studio', { callerThreadId: caller, placement: 'owned', computerId: REMOTE_BACKEND_UUID, computer: 'Studio' }),
          // Its own worker on a third computer, nested under it.
          row('bench', { callerThreadId: 't-studio', depth: 1, placement: 'owned', computerId: BENCH_UUID, computer: 'Bench' }),
          // A computer this window is not attached to: it cannot place it.
          row('away', { callerThreadId: caller, placement: 'owned', computerId: 'unpaired-uuid', computer: 'Old laptop' }),
          row('away-listed', { callerThreadId: caller, placement: 'standalone', computerId: REMOTE_BACKEND_UUID, computer: 'Studio' }),
        ]);
        const targets: (string | null)[] = [];
        const place = setBindingMock('SetThreadPlacement', vi.fn(async (ids: string[], placement: string) => {
          targets.push(getPinnedBackend());
          return ids.map((id) => ({ id, placement }));
        }));
        await fireEvent.click(toggle);
        const rows = await view.findAllByTestId('agent-tray-row');
        expect(rows.map((r) => Boolean(within(r).queryByTestId('agent-tray-row-show-in-sidebar')))).toEqual([true, true, false, false]);
        await fireEvent.click(within(rows[0]).getByTestId('agent-tray-row-show-in-sidebar'));
        await waitFor(() => expect(place).toHaveBeenCalledWith(['t-studio'], 'standalone'));
        await fireEvent.click(within(rows[1]).getByTestId('agent-tray-row-show-in-sidebar'));
        await waitFor(() => expect(place).toHaveBeenCalledWith(['t-bench'], 'standalone'));
        expect(targets).toEqual(['studio', 'bench']);
        await waitFor(() => expect(within(rows[0]).queryByTestId('agent-tray-row-show-in-sidebar')).toBeNull());
        expect(toastTexts()).toEqual([]);
      } finally {
        unstagePaired();
      }
    });
  }

  // The placement a row shows is the one the computer holding the worker
  // said: its frames reach this window at once, while this computer's list
  // relays its cache of that computer, which can answer from before a
  // placement made from a window. Show, a late cached answer, Remove from
  // another window, Show again, on one mounted tray.
  for (const compact of [false, true]) {
    it(`${compact ? 'under compact, ' : ''}keeps the placement the worker's computer said over a cached answer from before it`, async () => {
      setCompactLayoutForTest(compact);
      await stagePaired('studio', REMOTE_BACKEND_UUID, 'Studio');
      try {
        // This computer's cache of Studio's rows.
        let cached: 'owned' | 'standalone' = 'owned';
        const worker = (caller: string, placement: string) => row('studio', {
          callerThreadId: caller, placement, computerId: REMOTE_BACKEND_UUID, computer: 'Studio',
        });
        const { view, toggle, pane } = await openTray((caller) => [worker(caller, cached)]);
        const list = setBindingMock('ListThreadAgentRows', async () => ({ rows: [worker(pane.threadId!, cached)], hidden: false }));
        // What Studio announces of its own thread, on its connection.
        const announce = (placement: 'owned' | 'standalone') => emitWailsEvent('thread:updated', {
          action: placement === 'owned' ? 'full' : 'listed',
          thread: makeThread({ id: 't-studio', title: 'Agent studio', placement }),
        }, REMOTE_BACKEND_UUID);
        setBindingMock('SetThreadPlacement', vi.fn(async (ids: string[], placement: string) => {
          announce(placement as 'owned' | 'standalone');
          return ids.map((id) => ({ id, placement }));
        }));
        // A read of the list, as a frame about the thread's agents starts one.
        const reread = async () => {
          const before = list.mock.calls.length;
          emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId });
          await waitFor(() => expect(list.mock.calls.length).toBeGreaterThan(before));
          await new Promise((resolve) => setTimeout(resolve, 0));
        };
        await fireEvent.click(toggle);
        const offered = async () => Boolean(within(await view.findByTestId('agent-tray-row')).queryByTestId('agent-tray-row-show-in-sidebar'));
        await waitFor(async () => expect(await offered()).toBe(true));

        await fireEvent.click(within(await view.findByTestId('agent-tray-row')).getByTestId('agent-tray-row-show-in-sidebar'));
        await waitFor(async () => expect(await offered()).toBe(false));
        // The cache still says owned: a read that started after the click.
        await reread();
        expect(await offered()).toBe(false);

        // Remove from Sidebar in Studio's window; this computer's cache has
        // caught up with the Show and not yet with the Remove.
        cached = 'standalone';
        announce('owned');
        await waitFor(async () => expect(await offered()).toBe(true));
        await reread();
        expect(await offered()).toBe(true);

        // Show again, and a cached answer from before it.
        cached = 'owned';
        await fireEvent.click(within(await view.findByTestId('agent-tray-row')).getByTestId('agent-tray-row-show-in-sidebar'));
        await waitFor(async () => expect(await offered()).toBe(false));
        await reread();
        expect(await offered()).toBe(false);
        expect(toastTexts()).toEqual([]);
      } finally {
        unstagePaired();
      }
    });
  }

  // The computer sends a write's frame ahead of its reply, so a frame that
  // arrived after Show in Sidebar was sent is a newer write: Remove from
  // Sidebar in another window, here before the reply.
  // Stop, Stop All and a withdrawal ride `threads:operate` on this thread's
  // computer, Show in Sidebar on the worker's. A device without it keeps
  // them, inert and saying why.
  for (const compact of [false, true]) {
    it(`${compact ? 'under compact, ' : ''}in a view-only session keeps every write inert, saying why, and sends none`, async () => {
      setCompactLayoutForTest(compact);
      await pairViewOnly();
      try {
        const stop = setBindingMock('StopThreadAgentRow', vi.fn(async () => ({}) as ThreadAgentStop));
        const stopAll = setBindingMock('StopThreadAgentRows', vi.fn(async () => []));
        const withdraw = setBindingMock('StopThreadAgentIncoming', vi.fn(async () => ({}) as ThreadAgentStop));
        const place = setBindingMock('SetThreadPlacement', vi.fn(async () => []));
        const { view, toggle, pane } = await openTray((caller) => [row('w', { callerThreadId: caller, placement: 'owned' })]);
        replaceQueueForThread(pane.threadId!, [{
          id: 'q1', sendId: 'thread-request:tok', threadId: pane.threadId!, message: 'Check the changelog.',
          attachmentIds: [], enqueuedAt: Date.now(), agentMessage: true,
          originThread: { computerId: '', computerName: '', threadId: 'sender', title: 'Sender', token: 'tok', kind: 'send' },
        } as QueueItem]);
        await fireEvent.click(toggle);
        const r = await view.findByTestId('agent-tray-row');
        const writes = [
          view.getByTestId('activity-rail-agents-stop-all'),
          within(r).getByTestId('agent-tray-row-stop'),
          within(r).getByTestId('agent-tray-row-show-in-sidebar'),
          within(await view.findByTestId('agent-incoming-row')).getByTestId('agent-incoming-row-stop'),
        ] as HTMLButtonElement[];
        for (const write of writes) {
          expect(write.disabled, write.dataset.testid).toBe(true);
          expect(write.title, write.dataset.testid).toBe(UNGRANTED);
          await fireEvent.click(write);
        }
        await new Promise((resolve) => setTimeout(resolve, 0));
        for (const call of [stop, stopAll, withdraw, place]) expect(call).not.toHaveBeenCalled();
        expect(within(r).getByTestId('agent-tray-row-open-pane').hasAttribute('disabled')).toBe(false);
      } finally {
        resetToLocalPage();
      }
    });
  }

  it('judges Show in Sidebar by the worker\'s computer and Stop by this thread\'s', async () => {
    await stagePaired('studio', REMOTE_BACKEND_UUID, 'Studio', OBSERVE_SCOPES);
    await stagePaired('bench', BENCH_UUID, 'Bench');
    try {
      const { view, toggle } = await openTray((caller) => [
        row('studio', { callerThreadId: caller, placement: 'owned', computerId: REMOTE_BACKEND_UUID, computer: 'Studio' }),
        row('bench', { callerThreadId: caller, placement: 'owned', computerId: BENCH_UUID, computer: 'Bench' }),
      ]);
      await fireEvent.click(toggle);
      const [studio, bench] = await view.findAllByTestId('agent-tray-row');
      const show = (r: HTMLElement) => within(r).getByTestId('agent-tray-row-show-in-sidebar') as HTMLButtonElement;
      expect(show(studio).disabled).toBe(true);
      expect(show(studio).title).toBe(UNGRANTED);
      expect(show(bench).disabled).toBe(false);
      for (const r of [studio, bench]) expect((within(r).getByTestId('agent-tray-row-stop') as HTMLButtonElement).disabled).toBe(false);

      // A view-only device here can still place a worker on a computer that granted it.
      await pairViewOnly();
      await waitFor(() => expect((within(studio).getByTestId('agent-tray-row-stop') as HTMLButtonElement).disabled).toBe(true));
      expect(show(bench).disabled).toBe(false);
    } finally {
      resetToLocalPage();
      unstagePaired();
    }
  });

  it('keeps a placement the worker\'s computer announced after Show in Sidebar was sent over its reply', async () => {
    await stagePaired('studio', REMOTE_BACKEND_UUID, 'Studio');
    try {
      const { view, toggle } = await openTray((caller) => [
        row('studio', { callerThreadId: caller, placement: 'owned', computerId: REMOTE_BACKEND_UUID, computer: 'Studio' }),
      ]);
      const announce = (placement: 'owned' | 'standalone') => emitWailsEvent('thread:updated', {
        action: placement === 'owned' ? 'full' : 'listed',
        thread: makeThread({ id: 't-studio', title: 'Agent studio', placement }),
      }, REMOTE_BACKEND_UUID);
      setBindingMock('SetThreadPlacement', async (ids: string[]) => {
        announce('standalone');
        announce('owned');
        return ids.map((id) => ({ id, placement: 'standalone' }));
      });
      await fireEvent.click(toggle);
      const shown = await view.findByTestId('agent-tray-row');
      await fireEvent.click(within(shown).getByTestId('agent-tray-row-show-in-sidebar'));
      await new Promise((resolve) => setTimeout(resolve, 0));
      await waitFor(() => expect(within(view.getByTestId('agent-tray-row')).getByTestId('agent-tray-row-show-in-sidebar').hasAttribute('disabled')).toBe(false));
      expect(within(view.getByTestId('agent-tray-row')).queryByTestId('agent-tray-row-show-in-sidebar')).not.toBeNull();
    } finally {
      unstagePaired();
    }
  });

  it('offers Show in Sidebar on an owned worker of this computer, a send\'s row that counts its run included, and drops it once the worker has a row', async () => {
    const { view, toggle } = await openTray((caller) => [
      row('owned', { callerThreadId: caller, placement: 'owned' }),
      row('listed', { callerThreadId: caller, placement: 'standalone' }),
      row('archived', { callerThreadId: caller, placement: 'owned', archived: true }),
      // A paired computer this window is not attached to.
      row('remote', { callerThreadId: caller, placement: 'owned', computerId: 'peer', computer: 'Laptop' }),
      // A send to the caller's own worker carries the worker's placement.
      row('send', { callerThreadId: caller, kind: 'send', threadId: 't-sent', placement: 'owned' }),
      // An assistant's helper is outside every project: no placement to give.
      row('helper', { callerThreadId: caller }),
    ]);
    const targets: (string | null)[] = [];
    const place = setBindingMock('SetThreadPlacement', vi.fn(async (ids: string[], placement: string) => {
      targets.push(getPinnedBackend());
      return ids.map((id) => ({ id, placement }));
    }));
    await fireEvent.click(toggle);
    const rows = await view.findAllByTestId('agent-tray-row');
    expect(rows.map((r) => Boolean(within(r).queryByTestId('agent-tray-row-show-in-sidebar')))).toEqual([true, false, false, false, true, false]);
    expect(within(rows[5]).getByTestId('agent-tray-row-open-pane')).toBeTruthy();
    const show = within(rows[0]).getByTestId('agent-tray-row-show-in-sidebar');
    expect(show.getAttribute('title')).toBe('Show in sidebar');
    expect(show.getAttribute('aria-label')).toBe('Show in Sidebar');
    await fireEvent.click(show);
    await waitFor(() => expect(place).toHaveBeenCalledWith(['t-owned'], 'standalone'));
    expect(targets).toEqual([HOME_BACKEND]);
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
