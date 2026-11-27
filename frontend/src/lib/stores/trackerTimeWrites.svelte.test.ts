// The worklog writes: one send per target, what each outcome does to the
// weeks, Save anyway, and the toast for an outcome nobody shows.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { PostTimeView, TimeEntryView, WorklogView, WriteView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { attachTrackerWeek, type TrackerWeekAttachment } from './trackerTime.svelte';
import {
  addTarget,
  addWorklog,
  changeDraftEntry,
  draftTarget,
  entryTarget,
  postDraftEntries,
  removeDraftEntries,
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
    expect(outcome?.kind).toBe('refused');
    await vi.waitFor(() => expect(timeWrite(CTX, worklogTarget('501'))?.reread).toEqual({ kind: 'read' }));
    expect(timeWriteText(timeWrite(CTX, worklogTarget('501'))!)).toBe('This entry changed on the tracker since it was read. The week was read again; look at it before you change it.');
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
      'The delete of the time on WEB-1: This entry changed on the tracker since it was read. Look at the week before you change it.',
    ]);
    setBindingMock('DeleteTrackerWorklog', async () => APPLIED);
    await deleteWorklog(CTX, FROM);
    expect(getToasts()).toHaveLength(1);
  });
});

function entry(id: number, extra: Partial<TimeEntryView> = {}): TimeEntryView {
  return { id, revision: 3, connection: 'c1', day: '2026-11-10', issue: '10001', key: 'WEB-1', minutes: 60, reason: 'Commits', by: 'agent', thread: 't1', call: 'call-1', state: 'drafted', ...extra };
}

describe('a change of an agent\'s draft', () => {
  const DAY = draftTarget('day:2026-11-10');

  it('approves the entries as read once while it is pending, and holds the computer\'s sentence under its target', async () => {
    const answer = deferred<PostTimeView>();
    const post = setBindingMock('PostTrackerTimeEntries', () => answer.promise);
    const release = holdTimeWriteTarget(CTX, DAY);
    const first = postDraftEntries(CTX, DAY, [entry(1), entry(2, { revision: 5 })]);
    expect(await postDraftEntries(CTX, DAY, [entry(1)])).toBeNull();
    expect(post).toHaveBeenCalledTimes(1);
    expect(post).toHaveBeenCalledWith('c1', { entries: [{ id: 1, revision: 3 }, { id: 2, revision: 5 }], alone: undefined });
    expect(timeWrite(CTX, DAY)?.pending).toBe(true);
    const sentence = 'Posted 1 of 2 entries for Tue Nov 10. WEB-2, 1h, was refused: Worklog is closed.';
    answer.resolve({ entries: [], posted: 1, of: 2, sentence });
    expect((await first)?.outcome).toEqual({ kind: 'applied', note: sentence });
    expect(timeWriteText(timeWrite(CTX, DAY)!)).toBe(sentence);
    release();
    expect(timeWrite(CTX, DAY)).toBeNull();
  });

  it('logs one entry alone under the entry\'s target', async () => {
    const post = setBindingMock('PostTrackerTimeEntries', async () => ({ entries: [], posted: 1, of: 1, sentence: 'Posted 1 of 1 entries for Tue Nov 10.' }));
    await postDraftEntries(CTX, entryTarget(1), [entry(1)], true);
    expect(post).toHaveBeenCalledWith('c1', { entries: [{ id: 1, revision: 3 }], alone: true });
  });

  it('puts a receipt on the weeks showing a day the post could not read back, and says so', async () => {
    setBindingMock('PostTrackerTimeEntries', async () => ({
      entries: [], posted: 1, of: 1, sentence: 'Posted 1 of 1 entries for Tue Nov 10. The posted time shows on Tue Nov 10 after the next read: Jira did not answer.',
      stale: [{ resources: [{ kind: 'worklogs', author: 'acct-1', day: '2026-11-10' }], failure: { message: 'Jira did not answer', kind: 'unreachable' } }],
    } as unknown as PostTimeView));
    const release = holdTimeWriteTarget(CTX, DAY);
    await postDraftEntries(CTX, DAY, [entry(1)]);
    // The computer's sentence already says which days and why.
    const said = 'Posted 1 of 1 entries for Tue Nov 10. The posted time shows on Tue Nov 10 after the next read: Jira did not answer.';
    expect(timeWriteText(timeWrite(CTX, DAY)!)).toBe(said);
    expect(week.current.receipts.map((r) => r.sentence)).toEqual([said]);
    release();
  });

  it('answers the entry as the computer stored it, a merged one included, and holds nothing for a plain change', async () => {
    const merged = entry(9, { minutes: 85, revision: 1 });
    const edit = setBindingMock('EditTrackerTimeEntry', async () => merged);
    const release = holdTimeWriteTarget(CTX, entryTarget(4));
    const done = await changeDraftEntry(CTX, entry(4, { key: undefined, issue: undefined }), { ticket: { issue: '10001', key: 'WEB-1' } });
    expect(edit).toHaveBeenCalledWith('c1', { id: 4, revision: 3 }, { ticket: { issue: '10001', key: 'WEB-1' } });
    expect(done).toEqual({ outcome: { kind: 'applied', note: null }, value: merged });
    expect(timeWrite(CTX, entryTarget(4))).toBeNull();
    release();
  });

  it('says how many a discard removed when some changed since they were read', async () => {
    setBindingMock('RemoveTrackerTimeEntries', async () => [entry(1)]);
    const week = draftTarget('week:2026-11-09');
    const release = holdTimeWriteTarget(CTX, week);
    await removeDraftEntries(CTX, week, [entry(1), entry(2)]);
    expect(timeWriteText(timeWrite(CTX, week)!)).toBe('1 of 2 entries were removed; the others changed or were posted since they were read.');
    release();
  });

  it('says in a toast what became of a change nobody shows, its error as the computer gave it', async () => {
    setBindingMock('EditTrackerTimeEntry', async () => { throw new Error('WEB-1 already has an entry not yet posted that day'); });
    await changeDraftEntry(CTX, entry(4), { hours: 2 });
    expect(getToasts().map((t) => t.message)).toEqual(['The change of the entry on WEB-1 for Nov 10 was not confirmed']);
    setBindingMock('PostTrackerTimeEntries', async () => ({ entries: [], posted: 0, of: 1, sentence: 'Posted 0 of 1 entries for Tue Nov 10. WEB-1, 1h, was refused: Worklog is closed.' }));
    await postDraftEntries(CTX, DAY, [entry(1)]);
    expect(getToasts().map((t) => t.message).at(-1)).toBe('The approval of the draft: Posted 0 of 1 entries for Tue Nov 10. WEB-1, 1h, was refused: Worklog is closed.');
  });
});

// Connection IDs are each computer's own, so an entry of another
// connection is one another board's screen showed: it is never sent.
describe('an entry of another board\'s connection', () => {
  const OTHER: TimeWriteContext = { backend: 'peer-2', connection: 'c2', author: 'acct-1', computer: 'Desktop' };

  it('is refused before anything is sent, by every write that names entries, and says so', async () => {
    const edit = setBindingMock('EditTrackerTimeEntry', async () => entry(1));
    const remove = setBindingMock('RemoveTrackerTimeEntries', async () => [entry(1)]);
    const post = setBindingMock('PostTrackerTimeEntries', async () => ({ entries: [], posted: 1, of: 1, sentence: 'Posted 1 of 1 entries for Tue Nov 10.' }));
    const add = setBindingMock('AddTrackerWorklog', async () => APPLIED);
    const one = "The entry belongs to another board's connection, so nothing was sent.";
    const release = holdTimeWriteTarget(OTHER, entryTarget(1));
    expect((await changeDraftEntry(OTHER, entry(1), { hours: 2 }))?.outcome).toEqual({ kind: 'not_sent', message: one });
    expect(timeWriteText(timeWrite(OTHER, entryTarget(1))!)).toBe(one);
    release();
    await removeDraftEntries(OTHER, entryTarget(1), [entry(1)]);
    await postDraftEntries(OTHER, draftTarget('day:2026-11-10'), [entry(1, { connection: 'c2' }), entry(2)]);
    expect(getToasts().map((t) => t.message).slice(-2)).toEqual([
      `The removal of the entry on WEB-1 for Nov 10: ${one}`,
      "The approval of the draft: One of these entries belongs to another board's connection, so nothing was sent.",
    ]);
    const w = { issue: '10001', key: 'WEB-1', day: '2026-11-10', spentSeconds: 3600, description: 'Commits' };
    await addWorklog(OTHER, w, entry(1));
    expect(getToasts().map((t) => t.message).at(-1)).toBe(`The time logged on WEB-1: ${one}`);
    expect([edit, remove, post, add].map((m) => m.mock.calls.length)).toEqual([0, 0, 0, 0]);
  });
});
