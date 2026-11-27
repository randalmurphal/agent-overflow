// A time_draft call's card: one read per card on its thread's computer,
// a frame naming its thread read once per burst, a gap and a reconnect
// read every card of the computer, an older answer never over a newer
// one, and nothing held once the last card lets go.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import type { TimeEntryView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { attachDraftCall, DRAFT_CARD_REREAD_DELAY_MS, type DraftCallRef } from './trackerTimeDraft.svelte';
import { trackerTimeEntriesGap, trackerTimeEntriesUpdated } from './trackerBoardEvents';
import { __setBackendStatusForTest } from './transportStatus.svelte';
import { getPinnedBackend } from '../transport/backends';
import { DisconnectedError } from '../transport/wsClient';
import { setBindingMock } from '../../test/mocks/bindings-app';

const REF: DraftCallRef = { backend: 'peer-1', thread: 't1', call: 'call-1' };

function entry(id: number, extra: Partial<TimeEntryView> = {}): TimeEntryView {
  return { id, revision: 1, connection: 'c1', day: '2026-11-10', issue: '10001', key: 'WEB-1', minutes: 60, reason: 'Commits', by: 'agent', thread: 't1', call: 'call-1', state: 'drafted', ...extra };
}

/** ThreadTimeEntries, answered by the test one call at a time. */
function held(): { calls: unknown[][]; pins: (string | null)[]; answer: (v: TimeEntryView[]) => void; answerLast: (v: TimeEntryView[]) => void; fail: (e: unknown) => void; failLast: (e: unknown) => void } {
  const waiting: Array<{ resolve: (v: TimeEntryView[]) => void; reject: (e: unknown) => void }> = [];
  const calls: unknown[][] = [];
  const pins: (string | null)[] = [];
  setBindingMock('ThreadTimeEntries', (...args: unknown[]) => {
    calls.push(args);
    pins.push(getPinnedBackend());
    return new Promise<TimeEntryView[]>((resolve, reject) => waiting.push({ resolve, reject }));
  });
  return { calls, pins, answer: (v) => waiting.shift()!.resolve(v), answerLast: (v) => waiting.pop()!.resolve(v), fail: (e) => waiting.shift()!.reject(e), failLast: (e) => waiting.pop()!.reject(e) };
}

async function flush(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await tick();
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] });
});

afterEach(() => {
  vi.useRealTimers();
});

describe('a call\'s card', () => {
  it('reads its call once on the thread\'s computer, shared by every card that shows it', async () => {
    const reads = held();
    const a = attachDraftCall(REF);
    const b = attachDraftCall(REF);
    expect(reads.calls).toEqual([['t1', 'call-1']]);
    expect(reads.pins).toEqual(['peer-1']);
    expect(a.current.entries).toBeNull();
    reads.answer([entry(1)]);
    await flush();
    expect(a.current.entries?.map((e) => e.id)).toEqual([1]);
    expect(b.current.entries?.map((e) => e.id)).toEqual([1]);
    a.release();
    b.release();
  });

  it('reads again once for a burst of frames naming its thread on its computer, and not for others', async () => {
    const reads = held();
    const card = attachDraftCall(REF);
    reads.answer([entry(1)]);
    await flush();
    trackerTimeEntriesUpdated({ connection: 'c1', author: 'acct-1', days: ['2026-11-10'], threads: ['t2'] }, 'peer-1');
    trackerTimeEntriesUpdated({ connection: 'c1', author: 'acct-1', days: ['2026-11-10'], threads: ['t1'] }, 'peer-2');
    await vi.advanceTimersByTimeAsync(DRAFT_CARD_REREAD_DELAY_MS * 6);
    expect(reads.calls).toHaveLength(1);
    // A question folded into another call's entry names both threads.
    for (let i = 0; i < 3; i += 1) trackerTimeEntriesUpdated({ connection: 'c1', author: 'acct-1', days: ['2026-11-10'], threads: ['t2', 't1'] }, 'peer-1');
    await vi.advanceTimersByTimeAsync(DRAFT_CARD_REREAD_DELAY_MS);
    expect(reads.calls).toHaveLength(2);
    reads.answer([]);
    await flush();
    expect(card.current.entries).toEqual([]);
    card.release();
  });

  it('keeps the newer answer when an older read answers after it', async () => {
    const reads = held();
    const card = attachDraftCall(REF);
    trackerTimeEntriesUpdated({ connection: 'c1', author: 'acct-1', threads: ['t1'] }, 'peer-1');
    await vi.advanceTimersByTimeAsync(DRAFT_CARD_REREAD_DELAY_MS);
    expect(reads.calls).toHaveLength(2);
    // The second read answers first; the first answers after it.
    reads.answerLast([entry(1, { revision: 2 })]);
    await flush();
    reads.answer([entry(1, { revision: 1 })]);
    await flush();
    expect(card.current.entries?.[0].revision).toBe(2);
    card.release();
  });

  it('keeps a newer read\'s refusal when an older read answers after it', async () => {
    const reads = held();
    const card = attachDraftCall(REF);
    trackerTimeEntriesUpdated({ connection: 'c1', author: 'acct-1', threads: ['t1'] }, 'peer-1');
    await vi.advanceTimersByTimeAsync(DRAFT_CARD_REREAD_DELAY_MS);
    expect(reads.calls).toHaveLength(2);
    const refused = new Error('the computer refused the read');
    // The second read is refused first; the first answers after it.
    reads.failLast(refused);
    await flush();
    reads.answer([entry(1)]);
    await flush();
    expect(card.current.readError).toBe(refused);
    card.release();
  });

  it('reads every card of the computer on a gap and settles the recovery with them', async () => {
    const reads = held();
    const a = attachDraftCall(REF);
    const b = attachDraftCall({ ...REF, call: 'call-2' });
    const other = attachDraftCall({ ...REF, backend: 'peer-2' });
    expect(reads.calls).toHaveLength(3);
    let settled = false;
    void trackerTimeEntriesGap('peer-1')!.then(() => { settled = true; });
    expect(reads.calls.slice(3)).toEqual([['t1', 'call-1'], ['t1', 'call-2']]);
    for (let i = 0; i < 4; i += 1) reads.answer([]);
    await flush();
    expect(settled).toBe(false);
    reads.answer([]);
    await flush();
    expect(settled).toBe(true);
    a.release();
    b.release();
    other.release();
  });

  it('reads again when its computer connects again, and says nothing of a dropped connection', async () => {
    const reads = held();
    const card = attachDraftCall(REF);
    reads.fail(new DisconnectedError());
    await flush();
    expect(card.current.readError).toBeNull();
    __setBackendStatusForTest('peer-1', { status: 'reconnecting', nextAttemptAt: null });
    __setBackendStatusForTest('peer-1', { status: 'connected', nextAttemptAt: null });
    expect(reads.calls).toHaveLength(2);
    const refused = new Error('the computer refused the read');
    reads.fail(refused);
    await flush();
    expect(card.current.readError).toBe(refused);
    card.release();
  });

  it('holds nothing once the last card lets go, and reads afresh when one shows again', async () => {
    const reads = held();
    const card = attachDraftCall(REF);
    reads.answer([entry(1)]);
    await flush();
    card.release();
    card.release();
    trackerTimeEntriesUpdated({ connection: 'c1', author: 'acct-1', threads: ['t1'] }, 'peer-1');
    await vi.advanceTimersByTimeAsync(DRAFT_CARD_REREAD_DELAY_MS * 6);
    expect(reads.calls).toHaveLength(1);
    const again = attachDraftCall(REF);
    expect(again.current.entries).toBeNull();
    expect(reads.calls).toHaveLength(2);
    again.release();
  });
});
