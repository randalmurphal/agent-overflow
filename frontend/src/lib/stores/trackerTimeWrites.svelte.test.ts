// The worklog writes: one send per target, what each outcome does to the
// weeks, Save anyway, and the toast for an outcome nobody shows.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { WorklogView, WriteView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { attachTrackerWeek, type TrackerWeekAttachment } from './trackerTime.svelte';
import {
  addTarget,
  addWorklog,
  deleteWorklog,
  editWorklog,
  holdTimeWriteTarget,
  timeWrite,
  timeWriteText,
  worklogTarget,
  type TimeWriteContext,
} from './trackerTimeWrites.svelte';
import { getToasts } from './toast.svelte';
import { DisconnectedError } from '../transport/wsClient';
import { getBindingMock, setBindingMock } from '../../test/mocks/bindings-app';

const CTX: TimeWriteContext = { backend: 'peer-1', connection: 'c1', author: 'acct-1', computer: 'Laptop' };
const FROM: WorklogView = { id: '501', issue: '10001', key: 'WEB-1', title: 'First story', author: 'acct-1', day: '2026-11-10', spent_seconds: 3600, description: 'Review', updated: 7 };
const APPLIED = { parts: [{ step: 'worklog', state: 'applied' }] } as WriteView;
const STALE = { parts: [{ step: 'worklog', state: 'refused', refusal: { reason: 'stale', message: 'worklog 501 changed since it was read' } }] } as WriteView;
const LOSS = { parts: [{ step: 'worklog', state: 'refused', refusal: { reason: 'loss', message: 'This change would lose formatting.', lost: ['a table'] } }] } as WriteView;

function deferred<T>(): { promise: Promise<T>; resolve: (v: T) => void } {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => { resolve = r; });
  return { promise, resolve };
}

let week: TrackerWeekAttachment;

beforeEach(() => {
  setBindingMock('TrackerWorklogs', async () => ({ worklogs: [] }));
  setBindingMock('RefreshTrackerWorklogs', async () => ({ worklogs: [] }));
  week = attachTrackerWeek({ backend: 'peer-1', connection: 'c1', author: 'acct-1', monday: '2026-11-09' }, true);
});

afterEach(() => {
  week.release();
});

function reads(): { stored: number; refreshed: number } {
  return { stored: getBindingMock('TrackerWorklogs')!.mock.calls.length, refreshed: getBindingMock('RefreshTrackerWorklogs')!.mock.calls.length };
}

describe('a worklog write', () => {
  it('logs time once while it is pending, then reads the stored week again', async () => {
    const answer = deferred<WriteView>();
    const add = setBindingMock('AddTrackerWorklog', () => answer.promise);
    const release = holdTimeWriteTarget(CTX, addTarget('10001', '2026-11-11'));
    const w = { issue: '10001', key: 'WEB-1', day: '2026-11-11', spentSeconds: 5400, description: 'Pairing' };
    const first = addWorklog(CTX, w);
    expect(await addWorklog(CTX, w)).toBeNull();
    expect(add).toHaveBeenCalledTimes(1);
    expect(add).toHaveBeenCalledWith('c1', { issue: '10001', day: '2026-11-11', spent_seconds: 5400, description: 'Pairing' });
    expect(timeWrite(CTX, addTarget('10001', '2026-11-11'))?.pending).toBe(true);
    const before = reads();
    answer.resolve(APPLIED);
    expect(await first).toEqual({ kind: 'applied', note: null });
    expect(reads()).toEqual({ stored: before.stored + 1, refreshed: before.refreshed });
    expect(timeWrite(CTX, addTarget('10001', '2026-11-11'))).toBeNull();
    release();
  });

  it('changes only the members given, from the worklog as read, and rereads both days it touched', async () => {
    const edit = setBindingMock('EditTrackerWorklog', async () => APPLIED);
    setBindingMock('TrackerWorklogs', async () => ({ worklogs: [] }));
    await editWorklog(CTX, FROM, { spentSeconds: 1800 });
    expect(edit).toHaveBeenLastCalledWith('c1', { from: FROM, day: undefined, spent_seconds: 1800, description: undefined, accept_loss: undefined });
    const before = reads();
    // Moved to the next week: the shown week holds its old day.
    await editWorklog(CTX, FROM, { day: '2026-11-17' });
    expect(reads().stored).toBe(before.stored + 1);
  });

  it('refreshes the week when the entry changed on the tracker, and says so', async () => {
    setBindingMock('DeleteTrackerWorklog', async () => STALE);
    const release = holdTimeWriteTarget(CTX, worklogTarget('501'));
    const before = reads();
    const outcome = await deleteWorklog(CTX, FROM);
    expect(reads().refreshed).toBe(before.refreshed + 1);
    expect(timeWriteText(outcome!)).toBe('This entry changed on the tracker since it was read. The week was read again; look at it before you change it.');
    expect(timeWrite(CTX, worklogTarget('501'))?.outcome?.kind).toBe('refused');
    release();
    // The last surface showing it let go: the outcome goes with it.
    expect(timeWrite(CTX, worklogTarget('501'))).toBeNull();
  });

  it('refreshes the week when the answer was lost', async () => {
    setBindingMock('DeleteTrackerWorklog', async () => { throw new DisconnectedError('client closed'); });
    const release = holdTimeWriteTarget(CTX, worklogTarget('501'));
    const before = reads();
    const outcome = await deleteWorklog(CTX, FROM);
    expect(outcome?.kind).toBe('lost');
    expect(reads().refreshed).toBe(before.refreshed + 1);
    release();
  });

  it('offers Save anyway for a comment change that would lose formatting, sending it accepting the loss', async () => {
    const edit = setBindingMock('EditTrackerWorklog', vi.fn().mockResolvedValueOnce(LOSS).mockResolvedValueOnce(APPLIED));
    const release = holdTimeWriteTarget(CTX, worklogTarget('501'));
    await editWorklog(CTX, FROM, { description: 'Review, with a table' });
    const again = timeWrite(CTX, worklogTarget('501'))?.saveAnyway;
    expect(again).toBeTypeOf('function');
    await again!();
    expect(edit).toHaveBeenLastCalledWith('c1', expect.objectContaining({ description: 'Review, with a table', accept_loss: true }));
    expect(timeWrite(CTX, worklogTarget('501'))).toBeNull();
    release();
  });

  it('says in a toast what became of a write nobody shows, unless it was made', async () => {
    setBindingMock('DeleteTrackerWorklog', async () => STALE);
    await deleteWorklog(CTX, FROM);
    expect(getToasts().map((t) => t.message)).toEqual([
      'The delete of the time on WEB-1: This entry changed on the tracker since it was read. The week was read again; look at it before you change it.',
    ]);
    setBindingMock('DeleteTrackerWorklog', async () => APPLIED);
    await deleteWorklog(CTX, FROM);
    expect(getToasts()).toHaveLength(1);
  });
});
