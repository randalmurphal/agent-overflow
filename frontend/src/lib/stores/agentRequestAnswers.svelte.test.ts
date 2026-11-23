import { flushSync } from 'svelte';
import { describe, expect, it, vi } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { __resetAgentRequestAnswersForTest, requestAnswered, watchRequestAnswer } from './agentRequestAnswers.svelte';
import { applyThreadAgentRowsChanged } from './eventsThreadAgentRows';
import { __setBackendStatusForTest } from './transportStatus.svelte';
import { HOME_BACKEND } from '../transport/backendKey';

const settle = async () => {
  for (let i = 0; i < 5; i++) await Promise.resolve();
  await new Promise((resolve) => setTimeout(resolve, 0));
};

/** A ledger that answered every token it is asked about. Returns the mock. */
function answeredLedger() {
  const read = vi.fn(async (_thread: string, asked: string[]) => asked.map((token) => ({ token, state: 'finished', wakeItemId: `w-${token}`, answered: true })));
  setBindingMock('GetThreadRequestStates', read);
  return read;
}

/**
 * Mounts one row per token as GenericToolCallRow does: an effect that
 * watches its token and one that reads its answer. Counts each effect's runs
 * by token.
 */
function mountRows(threadId: string, tokens: readonly string[]) {
  const watches = new Map<string, number>();
  const reads = new Map<string, number>();
  const unmount = $effect.root(() => {
    for (const token of tokens) {
      $effect(() => {
        watches.set(token, (watches.get(token) ?? 0) + 1);
        return watchRequestAnswer(threadId, token);
      });
      $effect(() => {
        reads.set(token, (reads.get(token) ?? 0) + 1);
        requestAnswered(threadId, undefined, token);
      });
    }
  });
  flushSync();
  const total = (runs: Map<string, number>) => [...runs.values()].reduce((sum, n) => sum + n, 0);
  return { unmount, watches, reads, total };
}

describe('agent request answers', () => {
  it('reruns only the changed row on a frame for one token, and no row\'s watch', async () => {
    const tokens = Array.from({ length: 300 }, (_, i) => `tok-${i}`);
    const answers = new Map(tokens.map((token) => [token, '']));
    setBindingMock('GetThreadRequestStates', vi.fn(async (_thread: string, asked: string[]) =>
      asked.map((token) => ({ token, state: 'finished', wakeItemId: answers.get(token) ?? '', answered: Boolean(answers.get(token)) }))));
    const rows = mountRows('lead', tokens);
    await settle();
    flushSync();
    const watches = rows.total(rows.watches);
    const reads = new Map(rows.reads);

    // Read again, unchanged: nothing reruns.
    applyThreadAgentRowsChanged({ threadId: 'lead', id: 'tok-7' }, 'home');
    await settle();
    flushSync();
    expect(rows.total(rows.watches)).toBe(watches);
    expect(rows.reads).toEqual(reads);

    // Answered: only its own row reads it again.
    answers.set('tok-7', 'wake-7');
    applyThreadAgentRowsChanged({ threadId: 'lead', id: 'tok-7' }, 'home');
    await settle();
    flushSync();
    expect(rows.total(rows.watches)).toBe(watches);
    expect([...rows.reads].filter(([token, n]) => n !== reads.get(token))).toEqual([['tok-7', reads.get('tok-7')! + 1]]);
    expect(requestAnswered('lead', undefined, 'tok-7')).toBe(true);
    rows.unmount();
  });

  it('keeps every token a mounted row watches, past one read, and reads none of them again', async () => {
    const tokens = Array.from({ length: 300 }, (_, i) => `tok-${i}`);
    const read = answeredLedger();
    const releases = tokens.map((token) => watchRequestAnswer('lead', token));
    const perRound: number[] = [];
    let shown: boolean[] = [];
    for (let round = 0; round < 6; round++) {
      const before = read.mock.calls.length;
      shown = tokens.map((token) => requestAnswered('lead', undefined, token));
      await settle();
      perRound.push(read.mock.calls.length - before);
    }
    // One task's asks, in reads of at most 256.
    expect(perRound).toEqual([2, 0, 0, 0, 0, 0]);
    expect(read.mock.calls.map(([, asked]) => asked.length)).toEqual([256, 44]);
    expect(shown.every(Boolean)).toBe(true);
    for (const release of releases) release();
  });

  it('drops unwatched tokens past its bound, least recently asked first', async () => {
    const read = answeredLedger();
    const watchedRelease = watchRequestAnswer('lead', 'kept');
    for (let i = 0; i < 300; i++) requestAnswered('lead', undefined, `tok-${i}`);
    await settle();
    const before = read.mock.calls.length;
    expect(requestAnswered('lead', undefined, 'kept')).toBe(true);
    expect(requestAnswered('lead', undefined, 'tok-299')).toBe(true);
    // The first 44 asked went to keep 256 unwatched, and are asked again.
    expect(requestAnswered('lead', undefined, 'tok-0')).toBe(false);
    await settle();
    expect(read.mock.calls.slice(before).map(([, asked]) => asked)).toEqual([['tok-0']]);
    watchedRelease();
  });

  it('reads an answer a call collected, which left no wake row, as arrived', async () => {
    setBindingMock('GetThreadRequestStates', vi.fn(async (_thread: string, asked: string[]) =>
      asked.map((token) => ({ token, state: 'finished', wakeItemId: '', answered: token === 'inline' }))));
    requestAnswered('lead', undefined, 'inline');
    requestAnswered('lead', undefined, 'owed');
    await settle();
    expect(requestAnswered('lead', undefined, 'inline')).toBe(true);
    expect(requestAnswered('lead', undefined, 'owed')).toBe(false);
  });

  it('reads again for a frame that lands during the first read, after it, and lets the newer answer stand', async () => {
    const replies: ((answered: boolean) => void)[] = [];
    const read = vi.fn((_thread: string, asked: string[]) => new Promise((resolve) => {
      replies.push((answered) => resolve(asked.map((token) => ({ token, state: 'finished', wakeItemId: '', answered }))));
    }));
    setBindingMock('GetThreadRequestStates', read);
    const release = watchRequestAnswer('lead', 'tok');
    await settle();
    expect(read).toHaveBeenCalledTimes(1);
    // The only frame: the answer landed while the first read was out.
    applyThreadAgentRowsChanged({ threadId: 'lead', id: 'tok' }, 'home');
    await settle();
    // One read per thread at a time: the second waits for the first.
    expect(read).toHaveBeenCalledTimes(1);
    replies[0](false);
    await settle();
    expect(read).toHaveBeenCalledTimes(2);
    replies[1](true);
    await settle();
    expect(requestAnswered('lead', undefined, 'tok')).toBe(true);
    release();
  });

  it('reads a computer\'s threads again when it reconnects, after a read that failed', async () => {
    let fail = true;
    const read = vi.fn(async (_thread: string, asked: string[]) => {
      if (fail) throw new Error('offline');
      return asked.map((token) => ({ token, state: 'finished', wakeItemId: '', answered: true }));
    });
    setBindingMock('GetThreadRequestStates', read);
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    const release = watchRequestAnswer('lead', 'tok');
    await settle();
    expect(read).toHaveBeenCalledTimes(1);
    fail = false;
    __setBackendStatusForTest(HOME_BACKEND, { status: 'reconnecting', nextAttemptAt: null });
    __setBackendStatusForTest(HOME_BACKEND, { status: 'connected', nextAttemptAt: null });
    await settle();
    expect(read).toHaveBeenCalledTimes(2);
    expect(requestAnswered('lead', undefined, 'tok')).toBe(true);
    release();
    __setBackendStatusForTest(HOME_BACKEND, { status: 'disconnected', nextAttemptAt: null });
  });

  it('applies nothing from a read that was out when the cache reset', async () => {
    let reply!: () => void;
    setBindingMock('GetThreadRequestStates', vi.fn((_thread: string, asked: string[]) => new Promise((resolve) => {
      reply = () => resolve(asked.map((token) => ({ token, state: 'finished', wakeItemId: '', answered: true })));
    })));
    requestAnswered('lead', undefined, 'tok');
    await settle();
    __resetAgentRequestAnswersForTest();
    setBindingMock('GetThreadRequestStates', vi.fn(() => new Promise(() => {})));
    reply();
    await settle();
    expect(requestAnswered('lead', undefined, 'tok')).toBe(false);
  });

  it('lets a released token go once the bound needs room', async () => {
    answeredLedger();
    const release = watchRequestAnswer('lead', 'gone');
    await settle();
    release();
    release();
    for (let i = 0; i < 256; i++) requestAnswered('lead', undefined, `tok-${i}`);
    await settle();
    expect(requestAnswered('lead', undefined, 'gone')).toBe(false);
  });
});
