// The failed writes kept for a PR's next review state: one per editor of
// a PR, found by its editor (a line by either end of its range), freed by
// that editor, scoped by PR, and bounded with the oldest dropped first.

import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  FAILED_WRITES_MAX, failedLineWriteAt, failedWriteFor, keepFailedWrite, keptFailedWrites, releaseFailedWrite, resetFailedWrites,
} from './failedCommentWrites';
import type { CommentAnchor } from '../utils/reviewRows';

const end: CommentAnchor = { filePath: 'src/a.ts', side: 'new', newLine: 12 };
const start: CommentAnchor = { filePath: 'src/a.ts', side: 'new', newLine: 10 };
const line = { kind: 'line' as const, anchor: end, start };
const reply = (threadId: string) => ({ kind: 'reply' as const, threadId });

afterEach(() => {
  resetFailedWrites();
  vi.restoreAllMocks();
});

describe('failed comment writes', () => {
  it('finds one by its PR and editor, a line by either end of its range', () => {
    keepFailedWrite('pr-1', { target: line, body: 'Range.', error: 'Refused.', where: 'src/a.ts:10–12' });
    keepFailedWrite('pr-1', { target: reply('t1'), body: 'Reply.', error: 'Refused.', where: 'src/a.ts:3' });
    expect(failedLineWriteAt('pr-1', end)?.body).toBe('Range.');
    expect(failedLineWriteAt('pr-1', start)?.body).toBe('Range.');
    expect(failedLineWriteAt('pr-1', { ...end, newLine: 11 })).toBeNull();
    expect(failedWriteFor('pr-1', reply('t1'))?.body).toBe('Reply.');
    // Another PR, or none, has none.
    expect(failedLineWriteAt('pr-2', end)).toBeNull();
    expect(failedWriteFor('pr-2', reply('t1'))).toBeNull();
    expect(failedWriteFor(null, reply('t1'))).toBeNull();
  });

  it('keeps the newest for an editor, and frees it by that editor only', () => {
    keepFailedWrite('pr-1', { target: reply('t1'), body: 'First.', error: 'A.', where: 'x' });
    keepFailedWrite('pr-1', { target: reply('t1'), body: 'Second.', error: 'B.', where: 'x' });
    keepFailedWrite('pr-2', { target: reply('t1'), body: 'Other PR.', error: 'C.', where: 'x' });
    expect(failedWriteFor('pr-1', reply('t1'))).toMatchObject({ body: 'Second.', error: 'B.' });
    releaseFailedWrite('pr-1', reply('t2'));
    releaseFailedWrite('pr-1', reply('t1'));
    expect(failedWriteFor('pr-1', reply('t1'))).toBeNull();
    expect(failedWriteFor('pr-2', reply('t1'))?.body).toBe('Other PR.');
  });

  it(`keeps at most ${FAILED_WRITES_MAX}, dropping the oldest with a warning that names it`, () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    for (let i = 0; i <= FAILED_WRITES_MAX; i += 1) {
      keepFailedWrite('pr-1', { target: reply(`t${i}`), body: `Reply ${i}.`, error: 'Refused.', where: `src/a.ts:${i}` });
    }
    expect(keptFailedWrites()).toHaveLength(FAILED_WRITES_MAX);
    expect(failedWriteFor('pr-1', reply('t0'))).toBeNull();
    expect(failedWriteFor('pr-1', reply(`t${FAILED_WRITES_MAX}`))?.body).toBe(`Reply ${FAILED_WRITES_MAX}.`);
    expect(warn).toHaveBeenCalledTimes(1);
    expect(warn.mock.calls[0][0]).toContain('src/a.ts:0 (pr-1)');
  });
});
