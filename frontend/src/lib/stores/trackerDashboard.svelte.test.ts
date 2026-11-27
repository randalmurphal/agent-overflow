// The Dashboards screen's state: what it evaluates when, on which
// computer, that an answer to something no longer asked is never shown,
// that a board's changes count again at most once every 5 s, nothing
// counted while hidden, and the drill's pages.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, tick } from 'svelte';
import type { TrackerDashboardsView } from '../../../bindings/agent-overflow/internal/app/models';
import type { Answer, DashboardView, DrillAnswer, ProposalView } from '../../../bindings/agent-overflow/internal/trackerdash/models';
import type { WorklogRead } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import {
  createTrackerDashboard,
  DASHBOARD_CHANGE_INTERVAL_MS,
  DASHBOARD_REFRESH_MS,
  type DashboardSource,
  type TrackerDashboard,
} from './trackerDashboard.svelte';
import { getPinnedBackend } from '../transport/backends';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { readFrontendValue } from './frontendStorage';
import { trackerDashboardsUpdated } from './trackerBoardEvents';
import { applySettingsSnapshot } from './settings.svelte';

const VIEW = {
  id: 'overview_sprints',
  title: 'Overview',
  default_range: 'sprints_5',
  ranges: [
    { id: 'this_sprint', label: 'This sprint' },
    { id: 'sprints_5', label: 'Last 5 sprints' },
  ],
  widgets: [],
} as unknown as DashboardView;

/** The list read's answer: the dashboards, the proposals and Ask's sentence. */
function listed(dashboards: DashboardView[], proposals: ProposalView[] = [], ask = ''): TrackerDashboardsView {
  return { dashboards, proposals, ask } as TrackerDashboardsView;
}

function answerFor(range: string, at = 1): Answer {
  return { range: { id: range, label: range, periods: [], previous: '' }, estimates: 'points', at, widgets: [] } as unknown as Answer;
}

/** A binding mock whose answers the test releases one at a time. */
function held<T>(name: string): { calls: unknown[][]; pins: (string | null)[]; answer: (value: T) => void; fail: (err: unknown) => void } {
  const waiting: Array<{ resolve: (v: T) => void; reject: (e: unknown) => void }> = [];
  const calls: unknown[][] = [];
  const pins: (string | null)[] = [];
  setBindingMock(name, (...args: unknown[]) => {
    calls.push(args);
    pins.push(getPinnedBackend());
    return new Promise<T>((resolve, reject) => waiting.push({ resolve, reject }));
  });
  return {
    calls,
    pins,
    answer: (value) => waiting.shift()!.resolve(value),
    fail: (err) => waiting.shift()!.reject(err),
  };
}

async function flush(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await tick();
  flushSync();
}

let source = $state<DashboardSource | null>(null);
let shown = $state(true);
let store: TrackerDashboard | null = null;
const testBoard = { backend: 'peer-1', projectId: 'p1', connection: 'conn-1', board: '1' };

function open(): TrackerDashboard {
  store = createTrackerDashboard({ source: () => source, shown: () => shown });
  flushSync();
  return store;
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'Date'] });
  vi.setSystemTime(new Date(2026, 10, 25, 12, 0));
  source = { backend: 'peer-1', board: testBoard, subscription: 'sub-1', changes: 0 };
  shown = true;
});

afterEach(() => {
  store?.destroy();
  store = null;
  vi.useRealTimers();
});

/** Opens the store and lands its dashboards and its first answer. */
async function opened(): Promise<{ s: TrackerDashboard; evaluate: ReturnType<typeof held<Answer>> }> {
  const list = held<TrackerDashboardsView | null>('TrackerDashboards');
  const evaluate = held<Answer>('EvaluateTrackerDashboard');
  const s = open();
  list.answer(listed([VIEW]));
  await flush();
  evaluate.answer(answerFor('sprints_5'));
  await flush();
  return { s, evaluate };
}

describe('a count that fails', () => {
  it('stops counting and keeps the last answer, stale, beside its error', async () => {
    const { s, evaluate } = await opened();
    s.setRange('this_sprint');
    await flush();
    expect(s.counting).toBe(true);
    evaluate.fail(new Error('dashboard_too_slow'));
    await flush();
    expect(s.counting).toBe(false);
    expect(s.error).toBeInstanceOf(Error);
    expect(s.stale).toBe(true);
    expect(s.answer?.range.id).toBe('sprints_5');
  });
});

describe('when the screen opens', () => {
  it('lists the board\'s dashboards and counts the first at once, on the board\'s computer', async () => {
    const list = held<TrackerDashboardsView | null>('TrackerDashboards');
    const evaluate = held<Answer>('EvaluateTrackerDashboard');
    const s = open();
    expect(list.calls).toEqual([['sub-1']]);
    expect(list.pins).toEqual(['peer-1']);
    list.answer(listed([VIEW]));
    await flush();
    expect(s.dashboard?.id).toBe('overview_sprints');
    expect(s.range).toBe('sprints_5');
    expect(evaluate.calls).toHaveLength(1);
    expect(evaluate.pins).toEqual(['peer-1']);
    expect(evaluate.calls[0][0]).toBe('sub-1');
    expect(evaluate.calls[0][1]).toMatchObject({ dashboard: 'overview_sprints', range: 'sprints_5', mine: false, person: '', epic: '' });
    expect(s.counting).toBe(true);
    evaluate.answer(answerFor('sprints_5'));
    await flush();
    expect(s.answer?.range.id).toBe('sprints_5');
    expect(s.counting).toBe(false);
  });

  it('says a computer whose version has no dashboards has none', async () => {
    const list = held<TrackerDashboardsView | null>('TrackerDashboards');
    const evaluate = held<Answer>('EvaluateTrackerDashboard');
    const s = open();
    list.answer(null);
    await flush();
    expect(s.dashboards).toBe('older');
    expect(evaluate.calls).toHaveLength(0);
  });

  it('counts nothing while hidden, and at once when it shows', async () => {
    shown = false;
    const list = held<TrackerDashboardsView | null>('TrackerDashboards');
    const evaluate = held<Answer>('EvaluateTrackerDashboard');
    open();
    expect(list.calls).toHaveLength(0);
    shown = true;
    flushSync();
    list.answer(listed([VIEW]));
    await flush();
    expect(evaluate.calls).toHaveLength(1);
  });
});

describe('when what is counted changes', () => {
  it('never shows the answer to a range no longer asked for', async () => {
    const { s, evaluate } = await opened();
    source = { ...source!, changes: 1 };
    flushSync();
    vi.advanceTimersByTime(DASHBOARD_CHANGE_INTERVAL_MS);
    await flush();
    expect(evaluate.calls).toHaveLength(2);
    s.setRange('this_sprint');
    flushSync();
    expect(evaluate.calls).toHaveLength(3);
    expect(evaluate.calls[2][1]).toMatchObject({ range: 'this_sprint' });
    expect(s.counting).toBe(true);
    // The board change's answer lands after the range moved.
    evaluate.answer(answerFor('sprints_5', 2));
    await flush();
    expect(s.answer?.at).toBe(1);
    evaluate.answer(answerFor('this_sprint', 3));
    await flush();
    expect(s.answer?.range.id).toBe('this_sprint');
    expect(s.counting).toBe(false);
    expect(readFrontendValue('issues-dashboard-range')).toEqual({ overview_sprints: 'this_sprint' });
  });

  it('keeps Mine and a person apart: one clears the other', async () => {
    const { s, evaluate } = await opened();
    s.narrowPerson({ id: 'acct-2', label: 'User 2' });
    flushSync();
    expect(evaluate.calls.at(-1)![1]).toMatchObject({ mine: false, person: 'acct-2' });
    s.setMine(true);
    flushSync();
    expect(s.person).toBeNull();
    expect(evaluate.calls.at(-1)![1]).toMatchObject({ mine: true, person: '' });
    s.narrowPerson({ id: 'acct-2', label: 'User 2' });
    flushSync();
    expect(s.mine).toBe(false);
  });

  it('counts a subscription made again after a reconnect at once', async () => {
    const { evaluate } = await opened();
    held<TrackerDashboardsView | null>('TrackerDashboards');
    source = { backend: 'peer-1', board: testBoard, subscription: 'sub-2', changes: 0 };
    flushSync();
    expect(evaluate.calls.at(-1)![0]).toBe('sub-2');
  });
});

describe('when the board changes', () => {
  it('counts again at most once every 5 s, however often it changes', async () => {
    const { evaluate } = await opened();
    let answered = 1;
    // 10 s of a change every 250 ms after the first answer.
    for (let i = 1; i <= 40; i += 1) {
      source = { ...source!, changes: i };
      flushSync();
      vi.advanceTimersByTime(250);
      await flush();
      while (answered < evaluate.calls.length) {
        evaluate.answer(answerFor('sprints_5', i));
        answered += 1;
        await flush();
      }
    }
    // One evaluation for each 5 s, not 40.
    expect(evaluate.calls).toHaveLength(3);
    // A change just after an answer waits out the 5 s from it.
    source = { ...source!, changes: 41 };
    flushSync();
    vi.advanceTimersByTime(DASHBOARD_CHANGE_INTERVAL_MS - 1);
    await flush();
    expect(evaluate.calls).toHaveLength(3);
    vi.advanceTimersByTime(1);
    await flush();
    expect(evaluate.calls).toHaveLength(4);
  });

  it('counts nothing for a change while hidden', async () => {
    const { evaluate } = await opened();
    shown = false;
    flushSync();
    source = { ...source!, changes: 1 };
    flushSync();
    vi.advanceTimersByTime(DASHBOARD_REFRESH_MS * 2);
    await flush();
    expect(evaluate.calls).toHaveLength(1);
  });

  it('counts again every 10 minutes while shown, for the numbers that count now', async () => {
    const { evaluate } = await opened();
    vi.advanceTimersByTime(DASHBOARD_REFRESH_MS + 1000);
    await flush();
    expect(evaluate.calls).toHaveLength(2);
  });

  it('counts nothing once the screen is gone', async () => {
    const { s, evaluate } = await opened();
    s.destroy();
    store = null;
    source = { ...source!, changes: 1 };
    flushSync();
    vi.advanceTimersByTime(DASHBOARD_REFRESH_MS * 2);
    await flush();
    expect(evaluate.calls).toHaveLength(1);
  });
});

describe('the drill', () => {
  function drillAnswer(page: number, count: number, restarted = false): DrillAnswer {
    return { title: 'Finished', value: { v: 3 }, unit: 'estimate', estimates: 'points', count, tickets: count, counted: count, page, pages: 3, restarted, rows: [], at: 1 } as unknown as DrillAnswer;
  }

  it('asks a later page with the count the first answered, and takes the page its answer starts again at', async () => {
    const { s } = await opened();
    const drill = held<DrillAnswer>('TrackerDashboardDrill');
    s.openDrill({ widget: 'finished', cell: { bucket: 's7', series: '' } });
    expect(drill.calls[0]).toEqual(['sub-1', expect.objectContaining({ widget: 'finished', cell: { bucket: 's7', series: '' }, rest: false, page: 0, count: 0, range: 'sprints_5' })]);
    drill.answer(drillAnswer(0, 250));
    await flush();
    s.drillPage(1);
    expect(drill.calls[1][1]).toMatchObject({ page: 1, count: 250 });
    expect(s.drill?.loading).toBe(true);
    drill.answer(drillAnswer(0, 251, true));
    await flush();
    expect(s.drill?.page).toBe(0);
    expect(s.drill?.answer?.restarted).toBe(true);
  });

  it('counts the request of the answer it was opened from, not the controls as they are now', async () => {
    const { s } = await opened();
    held<Answer>('EvaluateTrackerDashboard');
    s.setRange('this_sprint');
    await flush();
    // The answer shown is still the last five sprints'.
    expect(s.stale).toBe(true);
    const drill = held<DrillAnswer>('TrackerDashboardDrill');
    s.openDrill({ widget: 'finished', cell: { bucket: 'sprint:s7', series: '' } });
    expect(drill.calls[0][1]).toMatchObject({ range: 'sprints_5' });
    drill.answer(drillAnswer(0, 150));
    await flush();
    s.drillPage(1);
    expect(drill.calls[1][1]).toMatchObject({ range: 'sprints_5', page: 1, count: 150 });
  });

  it('drops a page that lands after the drill closed', async () => {
    const { s } = await opened();
    const drill = held<DrillAnswer>('TrackerDashboardDrill');
    s.openDrill({ widget: 'pace' });
    // The widget's number names no cell.
    expect(drill.calls[0][1]).toMatchObject({ widget: 'pace', cell: null });
    s.closeDrill();
    drill.answer(drillAnswer(0, 3));
    await flush();
    expect(s.drill).toBeNull();
  });

  it('asks for the buckets a widget left out', async () => {
    const { s } = await opened();
    const drill = held<DrillAnswer>('TrackerDashboardDrill');
    s.openDrill({ widget: 'carrying', rest: true });
    expect(drill.calls[0][1]).toMatchObject({ widget: 'carrying', cell: null, rest: true });
  });

  it('reads its first page again when a count of its dashboard answers in another unit, and only then', async () => {
    const { s, evaluate } = await opened();
    const drill = held<DrillAnswer>('TrackerDashboardDrill');
    s.openDrill({ widget: 'pace' });
    drill.answer(drillAnswer(0, 250));
    await flush();
    s.drillPage(1);
    expect(drill.calls).toHaveLength(2);
    // The project's count changed on another computer: the dashboards are
    // told, and the count answers in tickets.
    trackerDashboardsUpdated('peer-1');
    await flush();
    evaluate.answer({ ...answerFor('sprints_5', 2), estimates: 'none' } as Answer);
    await flush();
    expect(drill.calls).toHaveLength(3);
    expect(drill.calls[2][1]).toMatchObject({ widget: 'pace', page: 0, count: 0, range: 'sprints_5' });
    // The page asked in the old unit lands after, and is not shown.
    drill.answer(drillAnswer(1, 250));
    await flush();
    expect(s.drill?.loading).toBe(true);
    drill.answer({ ...drillAnswer(0, 3), estimates: 'none' });
    await flush();
    expect(s.drill?.answer?.estimates).toBe('none');
    expect(s.drill?.page).toBe(0);
    // A count in the same unit leaves the drill as it is.
    trackerDashboardsUpdated('peer-1');
    await flush();
    evaluate.answer({ ...answerFor('sprints_5', 3), estimates: 'none' } as Answer);
    await flush();
    expect(drill.calls).toHaveLength(3);
  });

  it('closes when what is counted changes', async () => {
    const { s } = await opened();
    const drill = held<DrillAnswer>('TrackerDashboardDrill');
    s.openDrill({ widget: 'pace' });
    drill.answer(drillAnswer(0, 3));
    await flush();
    s.setRange('this_sprint');
    expect(s.drill).toBeNull();
  });
});

const MINE_VIEW = { ...VIEW, id: 'd_1', title: 'Bugs' } as unknown as DashboardView;
const NEW_VIEW = { ...VIEW, id: 'd_2', title: 'New' } as unknown as DashboardView;

describe('the board\'s own dashboards', () => {
  it('lays out the proposals and says why Ask cannot start a thread from the list, before any count answers', async () => {
    const list = held<TrackerDashboardsView | null>('TrackerDashboards');
    held<Answer>('EvaluateTrackerDashboard');
    const s = open();
    const proposal = { view: { ...VIEW.widgets[0], id: 'w_9', title: 'Proposed' }, thread: 't-1' } as unknown as ProposalView;
    list.answer(listed([VIEW], [proposal], 'Ask needs a provider.'));
    await flush();
    expect(s.answer).toBeNull();
    expect(s.proposals).toEqual([proposal]);
    expect(s.ask).toBe('Ask needs a provider.');
  });

  it('lists again when the board computer\'s settings change, so Ask\'s sentence follows its providers', async () => {
    const list = held<TrackerDashboardsView | null>('TrackerDashboards');
    held<Answer>('EvaluateTrackerDashboard');
    const s = open();
    list.answer(listed([VIEW], [], 'Ask needs a provider.'));
    await flush();
    expect(list.calls).toHaveLength(1);
    applySettingsSnapshot({ claudeEnabled: true }, 'peer-1');
    await flush();
    expect(list.calls).toHaveLength(2);
    list.answer(listed([VIEW]));
    await flush();
    expect(s.ask).toBe('');
  });

  it('shows no answer of another dashboard: one picked shows none until its own count answers or fails', async () => {
    const list = held<TrackerDashboardsView | null>('TrackerDashboards');
    const evaluate = held<Answer>('EvaluateTrackerDashboard');
    const s = open();
    list.answer(listed([VIEW, MINE_VIEW]));
    await flush();
    evaluate.answer(answerFor('sprints_5'));
    await flush();
    expect(s.answer).not.toBeNull();
    s.pick('d_1');
    await flush();
    expect(s.answer).toBeNull();
    expect(s.stale).toBe(false);
    evaluate.fail(new Error('dashboard_too_slow'));
    await flush();
    expect(s.answer).toBeNull();
    expect(s.error).toBeInstanceOf(Error);
    s.openDrill({ widget: 'w_1' });
    expect(s.drill).toBeNull();
    s.pick('overview_sprints');
    await flush();
    expect(s.answer?.range.id).toBe('sprints_5');
  });

  it('shows the one picked last for the board, and another board starts on the built-in', async () => {
    const list = held<TrackerDashboardsView | null>('TrackerDashboards');
    held<Answer>('EvaluateTrackerDashboard');
    const s = open();
    list.answer(listed([VIEW, MINE_VIEW]));
    await flush();
    expect(s.dashboard?.id).toBe('overview_sprints');
    expect(s.ownShown).toBe(false);
    s.pick('d_1');
    flushSync();
    expect(s.dashboard?.id).toBe('d_1');
    expect(s.ownShown).toBe(true);
    s.destroy();

    const again = open();
    list.answer(listed([VIEW, MINE_VIEW]));
    await flush();
    expect(again.dashboard?.id).toBe('d_1');
    again.destroy();

    source = { backend: 'peer-1', board: { ...testBoard, board: '2' }, subscription: 'sub-2', changes: 0 };
    const other = open();
    list.answer(listed([VIEW, MINE_VIEW]));
    await flush();
    expect(other.dashboard?.id).toBe('overview_sprints');
  });

  it('lists and counts again on a change from the board\'s computer only', async () => {
    const list = held<TrackerDashboardsView | null>('TrackerDashboards');
    const evaluate = held<Answer>('EvaluateTrackerDashboard');
    open();
    list.answer(listed([VIEW]));
    await flush();
    evaluate.answer(answerFor('sprints_5'));
    await flush();
    trackerDashboardsUpdated('home');
    await flush();
    expect(list.calls).toHaveLength(1);
    expect(evaluate.calls).toHaveLength(1);
    trackerDashboardsUpdated('peer-1');
    await flush();
    expect(list.calls).toHaveLength(2);
    expect(evaluate.calls).toHaveLength(2);
  });

  it('shows a new dashboard a Keep made once it is listed, and reads busy meanwhile', async () => {
    const list = held<TrackerDashboardsView | null>('TrackerDashboards');
    held<Answer>('EvaluateTrackerDashboard');
    const keep = held<string>('KeepTrackerWidget');
    const s = open();
    list.answer(listed([VIEW]));
    await flush();
    const kept = s.keep('w_1', { title: 'New' });
    expect(s.busy.has('w_1')).toBe(true);
    expect(keep.calls).toEqual([['sub-1', 'w_1', { title: 'New' }]]);
    expect(keep.pins).toEqual(['peer-1']);
    keep.answer('d_2');
    await flush();
    expect(s.busy.has('w_1')).toBe(false);
    expect(list.calls).toHaveLength(2);
    list.answer(listed([VIEW, NEW_VIEW]));
    await kept;
    flushSync();
    expect(s.dashboard?.id).toBe('d_2');
  });

  it('removes a widget only from a dashboard the project keeps', async () => {
    const list = held<TrackerDashboardsView | null>('TrackerDashboards');
    held<Answer>('EvaluateTrackerDashboard');
    const remove = held<void>('RemoveTrackerWidget');
    const dismiss = held<void>('DismissTrackerWidget');
    const s = open();
    list.answer(listed([VIEW, MINE_VIEW]));
    await flush();
    void s.removeWidget('w_1');
    expect(remove.calls).toEqual([]);
    s.pick('d_1');
    flushSync();
    void s.removeWidget('w_1');
    void s.dismiss('w_9');
    expect(remove.calls).toEqual([['sub-1', 'd_1', 'w_1']]);
    expect(dismiss.calls).toEqual([['sub-1', 'w_9']]);
    remove.answer();
    dismiss.answer();
    await flush();
  });

  it('shows the built-in once the dashboard shown is gone', async () => {
    const list = held<TrackerDashboardsView | null>('TrackerDashboards');
    held<Answer>('EvaluateTrackerDashboard');
    const s = open();
    list.answer(listed([VIEW, MINE_VIEW]));
    await flush();
    s.pick('d_1');
    flushSync();
    trackerDashboardsUpdated('peer-1');
    await flush();
    list.answer(listed([VIEW]));
    await flush();
    expect(s.dashboard?.id).toBe('overview_sprints');
    expect(s.ownShown).toBe(false);
  });

  it('shows the built-in on its own range once the dashboard shown is gone', async () => {
    const list = held<TrackerDashboardsView | null>('TrackerDashboards');
    const evaluate = held<Answer>('EvaluateTrackerDashboard');
    const s = open();
    list.answer(listed([VIEW, MINE_VIEW]));
    await flush();
    s.setRange('this_sprint');
    s.pick('d_1');
    s.setRange('sprints_5');
    flushSync();
    trackerDashboardsUpdated('peer-1');
    await flush();
    list.answer(listed([VIEW]));
    await flush();
    expect(s.dashboard?.id).toBe('overview_sprints');
    expect(s.range).toBe('this_sprint');
    const last = evaluate.calls[evaluate.calls.length - 1][1] as { dashboard: string; range: string };
    expect(last).toMatchObject({ dashboard: 'overview_sprints', range: 'this_sprint' });
  });

  it('starts each dashboard picked on its own range, not the one picked on the last', async () => {
    const list = held<TrackerDashboardsView | null>('TrackerDashboards');
    held<Answer>('EvaluateTrackerDashboard');
    const s = open();
    list.answer(listed([VIEW, MINE_VIEW]));
    await flush();
    s.setRange('this_sprint');
    flushSync();
    s.pick('d_1');
    flushSync();
    expect(s.range).toBe('sprints_5');
    s.pick('overview_sprints');
    flushSync();
    expect(s.range).toBe('this_sprint');
  });

  it('counts nothing for a change of the dashboards while hidden, and lists them again when it shows', async () => {
    const list = held<TrackerDashboardsView | null>('TrackerDashboards');
    const evaluate = held<Answer>('EvaluateTrackerDashboard');
    open();
    list.answer(listed([VIEW]));
    await flush();
    evaluate.answer(answerFor('sprints_5'));
    await flush();
    shown = false;
    flushSync();
    trackerDashboardsUpdated('peer-1');
    await flush();
    vi.advanceTimersByTime(DASHBOARD_CHANGE_INTERVAL_MS * 2);
    await flush();
    expect(list.calls).toHaveLength(1);
    expect(evaluate.calls).toHaveLength(1);
    shown = true;
    flushSync();
    expect(list.calls).toHaveLength(2);
  });

  it('keeps the list beside a failed read of it, and Refresh reads it again', async () => {
    const list = held<TrackerDashboardsView | null>('TrackerDashboards');
    held<Answer>('EvaluateTrackerDashboard');
    const s = open();
    list.answer(listed([VIEW, MINE_VIEW]));
    await flush();
    trackerDashboardsUpdated('peer-1');
    await flush();
    const failure = new Error('offline');
    list.fail(failure);
    await flush();
    expect(s.dashboards).toEqual([VIEW, MINE_VIEW]);
    expect(s.dashboardsError).toBe(failure);
    s.refresh();
    flushSync();
    expect(list.calls).toHaveLength(3);
    list.answer(listed([VIEW]));
    await flush();
    expect(s.dashboardsError).toBeNull();
  });

  it('reads the list again on Refresh after a read that answered, so Ask follows a provider installed since', async () => {
    const list = held<TrackerDashboardsView | null>('TrackerDashboards');
    held<Answer>('EvaluateTrackerDashboard');
    const s = open();
    list.answer(listed([VIEW], [], 'Ask needs a provider.'));
    await flush();
    s.refresh();
    flushSync();
    expect(list.calls).toHaveLength(2);
    list.answer(listed([VIEW]));
    await flush();
    expect(s.ask).toBe('');
  });
});

describe('your logged time', () => {
  /** A dashboard whose one widget counts your logged time. */
  const LOGGED_VIEW = { ...VIEW, widgets: [{ id: 'w', how: { reads: ['worklogs'] } }] } as unknown as DashboardView;
  /** An answer whose one widget counts your logged time, its days not
   * read `unread`. */
  function loggedAnswer(unread: string[] = [], at = 1, more: Record<string, unknown> = {}): Answer {
    return { ...answerFor('sprints_5', at), widgets: [{ id: 'w', unread, ...more }] } as unknown as Answer;
  }
  const NOT_READ = ['2026-11-24', '2026-11-25'];

  /** Opens the store on a dashboard of your logged time and lands `first`. */
  async function openedLogged(first: Answer, view = LOGGED_VIEW) {
    const list = held<TrackerDashboardsView | null>('TrackerDashboards');
    const evaluate = held<Answer>('EvaluateTrackerDashboard');
    const reads = held<WorklogRead>('ReadTrackerWorklogDays');
    const s = open();
    list.answer(listed([view]));
    await flush();
    evaluate.answer(first);
    await flush();
    return { s, evaluate, reads };
  }
  const why = (reads: { calls: unknown[][] }) => reads.calls.map((c) => c[2]);

  it('reads the days not read on the board\'s computer while the computer says there is more, says it is reading, then counts again', async () => {
    const { s, evaluate, reads } = await openedLogged(loggedAnswer(NOT_READ));
    expect(reads.calls[0]).toEqual(['conn-1', '2026-11-25', 'changed']);
    expect(s.logged.reading).toBe(true);
    for (let i = 0; i < 5; i += 1) {
      reads.answer({ read: true, more: true });
      await flush();
    }
    reads.answer({ read: true });
    await flush();
    expect(why(reads)).toEqual(['changed', 'shown', 'shown', 'shown', 'shown', 'shown']);
    expect(reads.pins.every((p) => p === 'peer-1')).toBe(true);
    expect(s.logged.reading).toBe(true);
    expect(evaluate.calls).toHaveLength(2);
    evaluate.answer(loggedAnswer([], 2));
    await flush();
    expect(s.logged.reading).toBe(false);
    expect(reads.calls).toHaveLength(6);
  });

  it('settles a read that leaves days unread with no failure, so the widget offers Read again', async () => {
    const { s, evaluate, reads } = await openedLogged(loggedAnswer(NOT_READ));
    reads.answer({ read: true });
    await flush();
    evaluate.answer(loggedAnswer(NOT_READ, 2));
    await flush();
    expect(why(reads)).toEqual(['changed', 'shown']);
    reads.answer({});
    await flush();
    expect(s.logged.reading).toBe(false);
    expect(s.logged.failure).toBe('');
    expect(evaluate.calls).toHaveLength(2);
  });

  it('sends nothing further once the store is destroyed mid-read', async () => {
    const { s, evaluate, reads } = await openedLogged(loggedAnswer(NOT_READ));
    s.destroy();
    store = null;
    reads.answer({ read: true, more: true });
    await flush();
    vi.advanceTimersByTime(DASHBOARD_REFRESH_MS);
    await flush();
    expect(reads.calls).toHaveLength(1);
    expect(evaluate.calls).toHaveLength(1);
  });

  it('sends nothing further while the screen is hidden, and reads on from its next answer when it shows', async () => {
    const { s, evaluate, reads } = await openedLogged(loggedAnswer(NOT_READ));
    shown = false;
    await flush();
    expect(s.logged.reading).toBe(false);
    reads.answer({ read: true, more: true });
    await flush();
    vi.advanceTimersByTime(DASHBOARD_REFRESH_MS);
    await flush();
    expect(reads.calls).toHaveLength(1);
    expect(evaluate.calls).toHaveLength(1);
    shown = true;
    await flush();
    evaluate.answer(loggedAnswer(['2026-11-24'], 2));
    await flush();
    expect(why(reads)).toEqual(['changed', 'shown']);
  });

  it('says why a read failed, reads no day again on its own, and Read again asks as the person', async () => {
    const { s, evaluate, reads } = await openedLogged(loggedAnswer(NOT_READ));
    reads.answer({ read: true, failure: { kind: 'rate_limited', message: 'Jira asked to wait', at: 1 } as WorklogRead['failure'] });
    await flush();
    expect(s.logged.failure).toBe('Jira asked to wait');
    evaluate.answer(loggedAnswer(NOT_READ, 2));
    await flush();
    reads.answer({});
    await flush();
    expect(s.logged.failure).toBe('Jira asked to wait');
    s.logged.retry();
    await flush();
    expect(why(reads)).toEqual(['changed', 'shown', 'person']);
    expect(s.logged.failure).toBe('');
    reads.fail(new Error('the computer is not connected'));
    await flush();
    expect(s.logged.failure).toBe('the computer is not connected');
    expect(s.logged.reading).toBe(false);
  });

  it('reads the newest days again when the board changes, when the computer says it may, and a known number never reads as reading', async () => {
    const { s, evaluate, reads } = await openedLogged(loggedAnswer());
    expect(why(reads)).toEqual(['changed']);
    expect(s.logged.reading).toBe(false);
    reads.answer({ read: true });
    await flush();
    expect(s.logged.reading).toBe(false);
    evaluate.answer(loggedAnswer([], 2));
    await flush();
    expect(reads.calls).toHaveLength(1);
    source = { ...source!, changes: 1 };
    await flush();
    vi.advanceTimersByTime(DASHBOARD_CHANGE_INTERVAL_MS);
    await flush();
    evaluate.answer(loggedAnswer([], 3));
    await flush();
    expect(why(reads)).toEqual(['changed', 'changed']);
    reads.answer({ rereadIn: 60_000 });
    await flush();
    expect(s.logged.reading).toBe(false);
    vi.advanceTimersByTime(59_999);
    await flush();
    expect(reads.calls).toHaveLength(2);
    vi.advanceTimersByTime(1);
    await flush();
    expect(why(reads)).toEqual(['changed', 'changed', 'changed']);
    reads.answer({ read: true });
    await flush();
    expect(s.logged.reading).toBe(false);
    expect(evaluate.calls).toHaveLength(4);
  });

  it('reads nothing when the wait to read the newest days again ends while the screen is hidden', async () => {
    const { evaluate, reads } = await openedLogged(loggedAnswer());
    reads.answer({ rereadIn: 60_000 });
    await flush();
    shown = false;
    await flush();
    vi.advanceTimersByTime(60_000);
    await flush();
    expect(reads.calls).toHaveLength(1);
    expect(evaluate.calls).toHaveLength(1);
  });

  it('reads again as the person on Refresh', async () => {
    const { s, reads } = await openedLogged(loggedAnswer());
    reads.answer({ read: true });
    await flush();
    s.refresh();
    await flush();
    expect(why(reads)).toEqual(['changed', 'person']);
  });

  it('runs one read at a time: what an answer asks while one is out is asked after it', async () => {
    const { s, evaluate, reads } = await openedLogged(loggedAnswer(NOT_READ));
    s.refresh();
    await flush();
    evaluate.answer(loggedAnswer(NOT_READ, 2));
    await flush();
    expect(reads.calls).toHaveLength(1);
    reads.answer({ read: true });
    await flush();
    expect(why(reads)).toEqual(['changed', 'person']);
  });

  it('reads nothing for a dashboard with no widget of your logged time, or one whose widget says why it cannot count', async () => {
    const { reads } = await openedLogged(loggedAnswer(NOT_READ), VIEW);
    expect(reads.calls).toHaveLength(0);
    store?.destroy();
    const other = await openedLogged(loggedAnswer(NOT_READ, 1, { note: 'Only your own logged time is read.' }));
    expect(other.reads.calls).toHaveLength(0);
  });

  it('reads another connection\'s days while the last connection\'s read is still out, and that read changes nothing after', async () => {
    const { s, evaluate, reads } = await openedLogged(loggedAnswer(['2026-11-25']));
    const list = held<TrackerDashboardsView | null>('TrackerDashboards');
    expect(reads.calls).toHaveLength(1);
    source = { backend: 'peer-1', board: { ...testBoard, connection: 'conn-2', board: '2' }, subscription: 'sub-2', changes: 0 };
    await flush();
    list.answer(listed([LOGGED_VIEW]));
    await flush();
    evaluate.answer(loggedAnswer(['2026-11-25'], 3));
    await flush();
    expect(reads.calls.map((c) => c[0])).toEqual(['conn-1', 'conn-2']);
    expect(s.logged.reading).toBe(true);
    reads.answer({ failure: { kind: 'rate_limited', message: 'Jira asked to wait', at: 1 } as WorklogRead['failure'] });
    await flush();
    expect(s.logged.reading).toBe(true);
    expect(s.logged.failure).toBe('');
    reads.answer({});
    await flush();
    expect(s.logged.reading).toBe(false);
  });
});
