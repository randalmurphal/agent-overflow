// The week store: which read runs when, what is shown when answers and
// failures cross, nothing read while hidden, and the weeks it holds.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import type { WorklogDays, WorklogView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import {
  attachTrackerWeek,
  refreshTrackerWeek,
  refreshTrackerWorklogDays,
  rereadTrackerWorklogDays,
  setTrackerWeekShown,
  WEEK_REFRESH_AFTER_MS,
  WEEKS_HELD,
  type TrackerWeekRef,
} from './trackerTime.svelte';
import { __setBackendStatusForTest } from './transportStatus.svelte';
import { applyTrackerConnectionsChanged } from './trackerConnections.svelte';
import { addWorklog } from './trackerTimeWrites.svelte';
import { getPinnedBackend } from '../transport/backends';
import { DisconnectedError, TransportError } from '../transport/wsClient';
import { setBindingMock } from '../../test/mocks/bindings-app';

const REF: TrackerWeekRef = { backend: 'peer-1', connection: 'c1', author: 'acct-1', monday: '2026-11-09' };

function log(id: string, day = '2026-11-09'): WorklogView {
  return { id, issue: '10001', key: 'WEB-1', title: 'First story', author: 'acct-1', day, spent_seconds: 3600, updated: 1 };
}

/** A binding mock whose answers the test releases one at a time. */
function held(name: string): { calls: unknown[][]; pins: (string | null)[]; answer: (value: WorklogDays) => void; fail: (err: unknown) => void } {
  const waiting: Array<{ resolve: (v: WorklogDays) => void; reject: (e: unknown) => void }> = [];
  const calls: unknown[][] = [];
  const pins: (string | null)[] = [];
  setBindingMock(name, (...args: unknown[]) => {
    calls.push(args);
    pins.push(getPinnedBackend());
    return new Promise<WorklogDays>((resolve, reject) => waiting.push({ resolve, reject }));
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
}

function setDocumentVisibility(value: DocumentVisibilityState): void {
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => value });
  document.dispatchEvent(new Event('visibilitychange'));
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(new Date(2026, 10, 12, 10, 0));
});

afterEach(() => {
  vi.useRealTimers();
  delete (document as unknown as Record<string, unknown>).visibilityState;
});

describe('when a week is read', () => {
  it('reads the stored week and refreshes it once it shows, on its computer, for its seven days', async () => {
    const stored = held('TrackerWorklogs');
    const refresh = held('RefreshTrackerWorklogs');
    const week = attachTrackerWeek(REF, true);
    expect(stored.calls).toEqual([['c1', 'acct-1', '2026-11-09', '2026-11-15']]);
    expect(refresh.calls).toEqual([['c1', 'acct-1', '2026-11-09', '2026-11-15']]);
    expect([...stored.pins, ...refresh.pins]).toEqual(['peer-1', 'peer-1']);
    expect(week.current.refreshing).toBe(true);
    stored.answer({ worklogs: [log('1')], unread: ['2026-11-10'] });
    await flush();
    expect(week.current.days?.worklogs.map((w) => w.id)).toEqual(['1']);
    refresh.answer({ worklogs: [log('1'), log('2', '2026-11-10')] });
    await flush();
    expect(week.current.days?.worklogs.map((w) => w.id)).toEqual(['1', '2']);
    expect(week.current.refreshing).toBe(false);
    week.release();
  });

  it('reads nothing while the pane hides it or the window is hidden', async () => {
    const stored = held('TrackerWorklogs');
    const refresh = held('RefreshTrackerWorklogs');
    const week = attachTrackerWeek(REF, false);
    setDocumentVisibility('hidden');
    setTrackerWeekShown(week.key, true);
    expect(stored.calls).toHaveLength(0);
    expect(refresh.calls).toHaveLength(0);
    setDocumentVisibility('visible');
    expect(stored.calls).toHaveLength(1);
    expect(refresh.calls).toHaveLength(1);
    week.release();
  });

  it('on showing again, refreshes only a week this page has not refreshed for five minutes', async () => {
    const stored = held('TrackerWorklogs');
    const refresh = held('RefreshTrackerWorklogs');
    const week = attachTrackerWeek(REF, true);
    refresh.answer({ worklogs: [] });
    stored.answer({ worklogs: [] });
    await flush();
    setTrackerWeekShown(week.key, false);
    vi.setSystemTime(Date.now() + WEEK_REFRESH_AFTER_MS - 1);
    setTrackerWeekShown(week.key, true);
    expect(stored.calls).toHaveLength(2);
    expect(refresh.calls).toHaveLength(1);
    setTrackerWeekShown(week.key, false);
    vi.setSystemTime(Date.now() + 1);
    setTrackerWeekShown(week.key, true);
    expect(stored.calls).toHaveLength(3);
    expect(refresh.calls).toHaveLength(2);
    week.release();
  });

  it('joins a Refresh pressed during a refresh', async () => {
    held('TrackerWorklogs');
    const refresh = held('RefreshTrackerWorklogs');
    const week = attachTrackerWeek(REF, true);
    const pressed = refreshTrackerWeek(week.key);
    expect(refresh.calls).toHaveLength(1);
    refresh.answer({ worklogs: [] });
    await pressed;
    void refreshTrackerWeek(week.key);
    expect(refresh.calls).toHaveLength(2);
    week.release();
  });

  it('refreshes a shown week when its computer connects again', async () => {
    held('TrackerWorklogs');
    const refresh = held('RefreshTrackerWorklogs');
    const week = attachTrackerWeek(REF, true);
    refresh.answer({ worklogs: [] });
    await flush();
    __setBackendStatusForTest('peer-1', { status: 'reconnecting', nextAttemptAt: null });
    __setBackendStatusForTest('peer-1', { status: 'connected', nextAttemptAt: null });
    expect(refresh.calls).toHaveLength(2);
    week.release();
  });
});

describe('what a week shows', () => {
  it('keeps a refresh\'s failure until the next refresh answers; a stored read leaves it', async () => {
    const stored = held('TrackerWorklogs');
    const refresh = held('RefreshTrackerWorklogs');
    const week = attachTrackerWeek(REF, true);
    stored.answer({ worklogs: [log('1')] });
    const failure = { kind: 'transient', message: 'Tempo could not be reached; try again', at: 1 };
    refresh.answer({ worklogs: [log('1')], failure });
    await flush();
    expect(week.current.failure).toEqual(failure);
    rereadTrackerWorklogDays('peer-1', 'c1', 'acct-1', ['2026-11-09']);
    stored.answer({ worklogs: [log('1'), log('2')] });
    await flush();
    expect(week.current.days?.worklogs).toHaveLength(2);
    expect(week.current.failure).toEqual(failure);
    void refreshTrackerWeek(week.key);
    refresh.answer({ worklogs: [log('1'), log('2')] });
    await flush();
    expect(week.current.failure).toBeNull();
    week.release();
  });

  it('shows a read the computer refused until a later read answers, and nothing for a dropped connection', async () => {
    const stored = held('TrackerWorklogs');
    const refresh = held('RefreshTrackerWorklogs');
    const week = attachTrackerWeek(REF, true);
    stored.fail(new DisconnectedError('client closed'));
    await flush();
    expect(week.current.readError).toBeNull();
    const refused = new TransportError('method_not_found', 'Laptop has no Time screen: it runs v0.0.19, this window v0.0.20. Update Laptop.');
    refresh.fail(refused);
    await flush();
    expect(week.current.readError).toBe(refused);
    void refreshTrackerWeek(week.key);
    refresh.answer({ worklogs: [] });
    await flush();
    expect(week.current.readError).toBeNull();
    week.release();
  });

  it('holds the weeks used last, and drops the one used least recently past them', async () => {
    const stored = held('TrackerWorklogs');
    held('RefreshTrackerWorklogs');
    const mondays = ['2026-10-05', '2026-10-12', '2026-10-19', '2026-10-26', '2026-11-02', '2026-11-09'];
    for (const monday of mondays) {
      const w = attachTrackerWeek({ ...REF, monday }, true);
      stored.answer({ worklogs: [log(monday, monday)] });
      await flush();
      w.release();
    }
    expect(mondays.length).toBe(WEEKS_HELD + 1);
    const kept = attachTrackerWeek({ ...REF, monday: mondays[1] }, true);
    expect(kept.current.days?.worklogs[0].id).toBe(mondays[1]);
    const dropped = attachTrackerWeek({ ...REF, monday: mondays[0] }, true);
    expect(dropped.current.days).toBeNull();
    kept.release();
    dropped.release();
  });

  it('never drops a week a surface holds, however many others it reads', async () => {
    const stored = held('TrackerWorklogs');
    const refresh = held('RefreshTrackerWorklogs');
    const shown = attachTrackerWeek(REF, true);
    stored.answer({ worklogs: [log('shown')] });
    refresh.answer({ worklogs: [log('shown')] });
    await flush();
    for (const monday of ['2026-10-05', '2026-10-12', '2026-10-19', '2026-10-26', '2026-11-02', '2026-11-16']) {
      attachTrackerWeek({ ...REF, monday }, false).release();
    }
    void refreshTrackerWeek(shown.key);
    expect(refresh.calls).toHaveLength(2);
    refresh.answer({ worklogs: [log('shown'), log('later')] });
    await flush();
    expect(shown.current.days?.worklogs.map((w) => w.id)).toEqual(['shown', 'later']);
    shown.release();
  });
});

describe('the order of answers', () => {
  it('never shows a stored answer that arrives after a refresh answered', async () => {
    const stored = held('TrackerWorklogs');
    const refresh = held('RefreshTrackerWorklogs');
    const week = attachTrackerWeek(REF, true);
    refresh.answer({ worklogs: [log('1'), log('2', '2026-11-10')] });
    await flush();
    stored.answer({ worklogs: [log('1')] });
    await flush();
    expect(week.current.days?.worklogs.map((w) => w.id)).toEqual(['1', '2']);
    week.release();
  });

  it('runs one more refresh after the one out when a write asks for it, and answers how that read ended', async () => {
    const stored = held('TrackerWorklogs');
    const refresh = held('RefreshTrackerWorklogs');
    const week = attachTrackerWeek(REF, true);
    stored.answer({ worklogs: [] });
    const asked = refreshTrackerWorklogDays('peer-1', 'c1', 'acct-1', ['2026-11-10']);
    expect(refresh.calls).toHaveLength(1);
    refresh.answer({ worklogs: [] });
    await flush();
    expect(refresh.calls).toHaveLength(2);
    let ended: unknown = null;
    void asked.then((end) => { ended = end; });
    await flush();
    expect(ended).toBeNull();
    refresh.answer({ worklogs: [log('9', '2026-11-10')] });
    await flush();
    expect(ended).toEqual({ kind: 'read' });
    expect(week.current.days?.worklogs.map((w) => w.id)).toEqual(['9']);
    week.release();
  });

  it('rereads the stored copy after the refresh out when a write was made meanwhile', async () => {
    const stored = held('TrackerWorklogs');
    const refresh = held('RefreshTrackerWorklogs');
    const week = attachTrackerWeek(REF, true);
    stored.answer({ worklogs: [] });
    await flush();
    rereadTrackerWorklogDays('peer-1', 'c1', 'acct-1', ['2026-11-10']);
    expect(stored.calls).toHaveLength(1);
    refresh.answer({ worklogs: [] });
    await flush();
    expect(stored.calls).toHaveLength(2);
    stored.answer({ worklogs: [log('3', '2026-11-10')] });
    await flush();
    expect(week.current.days?.worklogs.map((w) => w.id)).toEqual(['3']);
    week.release();
  });
});

describe('a change of the computer\'s connections', () => {
  it('drops what its weeks hold, and reads and refreshes the shown week again', async () => {
    const stored = held('TrackerWorklogs');
    const refresh = held('RefreshTrackerWorklogs');
    const week = attachTrackerWeek(REF, true);
    stored.answer({ worklogs: [log('old')] });
    refresh.answer({ worklogs: [log('old')] });
    await flush();
    const other = attachTrackerWeek({ ...REF, backend: 'home' }, true);
    applyTrackerConnectionsChanged('peer-1');
    expect(week.current.days).toBeNull();
    expect(stored.calls.filter((_, i) => stored.pins[i] === 'peer-1')).toHaveLength(2);
    expect(refresh.calls.filter((_, i) => refresh.pins[i] === 'peer-1')).toHaveLength(2);
    other.release();
    week.release();
  });

  it('drops an answer to a read sent before the change', async () => {
    const stored = held('TrackerWorklogs');
    const refresh = held('RefreshTrackerWorklogs');
    const week = attachTrackerWeek(REF, true);
    applyTrackerConnectionsChanged('peer-1');
    stored.answer({ worklogs: [log('before')] });
    refresh.answer({ worklogs: [log('before')] });
    await flush();
    expect(week.current.days).toBeNull();
    expect(week.current.refreshing).toBe(true);
    stored.answer({ worklogs: [log('after')] });
    await flush();
    expect(week.current.days?.worklogs.map((w) => w.id)).toEqual(['after']);
    week.release();
  });
});

describe('a write the week has not caught up with', () => {
  const CTX = { backend: 'peer-1', connection: 'c1', author: 'acct-1', computer: 'Laptop' };
  const NEW = { issue: '10001', key: 'WEB-1', day: '2026-11-11', spentSeconds: 5400, description: '' };
  const failure = { kind: 'transient', message: 'Jira could not be reached; try again', at: 1 };
  const UNREAD = { parts: [{ step: 'worklog', state: 'applied' }], stale: [{ connection: 'c1', resources: [], failure }] };

  it('keeps its receipt through a refresh that started before it, and Refresh then reads after that one', async () => {
    const stored = held('TrackerWorklogs');
    const refresh = held('RefreshTrackerWorklogs');
    setBindingMock('AddTrackerWorklog', async () => UNREAD);
    const week = attachTrackerWeek(REF, true);
    stored.answer({ worklogs: [] });
    await addWorklog(CTX, NEW);
    expect(week.current.receipts.map((r) => r.sentence)).toEqual([
      '1h 30m logged on WEB-1 for Nov 11: Saved. The week shows it after its next read: Jira could not be reached; try again',
    ]);
    void refreshTrackerWeek(week.key);
    expect(refresh.calls).toHaveLength(1);
    refresh.answer({ worklogs: [] });
    await flush();
    expect(week.current.receipts).toHaveLength(1);
    expect(refresh.calls).toHaveLength(2);
    refresh.answer({ worklogs: [], failure });
    await flush();
    expect(week.current.receipts).toHaveLength(1);
    void refreshTrackerWeek(week.key);
    refresh.answer({ worklogs: [log('new', '2026-11-11')] });
    await flush();
    expect(week.current.receipts).toEqual([]);
    week.release();
  });
});

describe('what a write asks of the weeks', () => {
  it('rereads the stored copy of a shown week holding its day, and of no other', async () => {
    const stored = held('TrackerWorklogs');
    const refresh = held('RefreshTrackerWorklogs');
    const week = attachTrackerWeek(REF, true);
    refresh.answer({ worklogs: [] });
    await flush();
    rereadTrackerWorklogDays('peer-1', 'c1', 'acct-1', ['2026-11-20']);
    rereadTrackerWorklogDays('peer-1', 'c2', 'acct-1', ['2026-11-10']);
    expect(stored.calls).toHaveLength(1);
    rereadTrackerWorklogDays('peer-1', 'c1', 'acct-1', ['2026-11-20', '2026-11-15']);
    expect(stored.calls).toHaveLength(2);
    expect(refresh.calls).toHaveLength(1);
    week.release();
  });

  it('refreshes a shown week holding its day now, and a hidden one when it shows', async () => {
    held('TrackerWorklogs');
    const refresh = held('RefreshTrackerWorklogs');
    const week = attachTrackerWeek(REF, true);
    refresh.answer({ worklogs: [] });
    await flush();
    refreshTrackerWorklogDays('peer-1', 'c1', 'acct-1', ['2026-11-10']);
    expect(refresh.calls).toHaveLength(2);
    refresh.answer({ worklogs: [] });
    await flush();
    setTrackerWeekShown(week.key, false);
    refreshTrackerWorklogDays('peer-1', 'c1', 'acct-1', ['2026-11-10']);
    expect(refresh.calls).toHaveLength(2);
    setTrackerWeekShown(week.key, true);
    expect(refresh.calls).toHaveLength(3);
    week.release();
  });
});
