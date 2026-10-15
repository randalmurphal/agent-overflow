import { afterEach, describe, expect, it, vi } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { __resetAgentRequestAnswersForTest, requestAnswered, watchRequestAnswer } from './agentRequestAnswers.svelte';

afterEach(() => {
  __resetAgentRequestAnswersForTest();
  resetBindingMocks();
});

const settle = async () => {
  for (let i = 0; i < 5; i++) await Promise.resolve();
  await new Promise((resolve) => setTimeout(resolve, 0));
};

/** A ledger that answered every token it is asked about. Returns the mock. */
function answeredLedger() {
  const read = vi.fn(async (_thread: string, asked: string[]) => asked.map((token) => ({ token, state: 'finished', wakeItemId: `w-${token}` })));
  setBindingMock('GetThreadRequestStates', read);
  return read;
}

describe('agent request answers', () => {
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
