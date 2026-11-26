// The Dashboards screen's state: what it evaluates when, on which
// computer, that an answer to something no longer asked is never shown,
// that a board's changes count again at most once every 5 s, nothing
// counted while hidden, and the drill's pages.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, tick } from 'svelte';
import type { Answer, DashboardView, DrillAnswer } from '../../../bindings/agent-overflow/internal/trackerdash/models';
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

function answerFor(range: string, at = 1): Answer {
  return { dashboard: VIEW, range: { id: range, label: range, periods: [], previous: '' }, estimates: 'points', at, widgets: [] } as unknown as Answer;
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

function open(): TrackerDashboard {
  store = createTrackerDashboard({ source: () => source, shown: () => shown });
  flushSync();
  return store;
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'Date'] });
  vi.setSystemTime(new Date(2026, 10, 25, 12, 0));
  source = { backend: 'peer-1', subscription: 'sub-1', changes: 0 };
  shown = true;
});

afterEach(() => {
  store?.destroy();
  store = null;
  vi.useRealTimers();
});

/** Opens the store and lands its dashboards and its first answer. */
async function opened(): Promise<{ s: TrackerDashboard; evaluate: ReturnType<typeof held<Answer>> }> {
  const list = held<DashboardView[] | null>('TrackerDashboards');
  const evaluate = held<Answer>('EvaluateTrackerDashboard');
  const s = open();
  list.answer([VIEW]);
  await flush();
  evaluate.answer(answerFor('sprints_5'));
  await flush();
  return { s, evaluate };
}

describe('when the screen opens', () => {
  it('lists the board\'s dashboards and counts the first at once, on the board\'s computer', async () => {
    const list = held<DashboardView[] | null>('TrackerDashboards');
    const evaluate = held<Answer>('EvaluateTrackerDashboard');
    const s = open();
    expect(list.calls).toEqual([['sub-1']]);
    expect(list.pins).toEqual(['peer-1']);
    list.answer([VIEW]);
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
    const list = held<DashboardView[] | null>('TrackerDashboards');
    const evaluate = held<Answer>('EvaluateTrackerDashboard');
    const s = open();
    list.answer(null);
    await flush();
    expect(s.dashboards).toBe('older');
    expect(evaluate.calls).toHaveLength(0);
  });

  it('counts nothing while hidden, and at once when it shows', async () => {
    shown = false;
    const list = held<DashboardView[] | null>('TrackerDashboards');
    const evaluate = held<Answer>('EvaluateTrackerDashboard');
    open();
    expect(list.calls).toHaveLength(0);
    shown = true;
    flushSync();
    list.answer([VIEW]);
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
    held<DashboardView[] | null>('TrackerDashboards');
    source = { backend: 'peer-1', subscription: 'sub-2', changes: 0 };
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
    return { title: 'Finished', value: { v: 3 }, unit: 'estimate', estimates: 'points', count, counted: count, page, pages: 3, restarted, rows: [], at: 1 } as unknown as DrillAnswer;
  }

  it('asks a later page with the count the first answered, and takes the page its answer starts again at', async () => {
    const { s } = await opened();
    const drill = held<DrillAnswer>('TrackerDashboardDrill');
    s.openDrill({ widget: 'finished', cell: { bucket: 's7', series: '' } });
    expect(drill.calls[0]).toEqual(['sub-1', expect.objectContaining({ widget: 'finished', cell: { bucket: 's7', series: '' }, page: 0, count: 0, range: 'sprints_5' })]);
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
