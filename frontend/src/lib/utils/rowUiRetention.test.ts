import { describe, expect, it } from 'vitest';
import { makeItem } from '../../test/helpers/chat';
import { isRowUiRetentionActive, rowUiRetentionChanged } from './rowUiRetention';

describe('isRowUiRetentionActive', () => {
  it('is exactly the running/streaming pair', () => {
    for (const status of ['running', 'streaming'] as const) {
      expect(isRowUiRetentionActive(makeItem({ status }))).toBe(true);
    }
    for (const status of ['completed', 'errored', 'declined', 'killed'] as const) {
      expect(isRowUiRetentionActive(makeItem({ status }))).toBe(false);
    }
  });
});

describe('rowUiRetentionChanged', () => {
  // The case the revision exists for: a streamed text delta replaces the
  // row object and changes nothing the prune retains.
  it('ignores a summary-only delta on an active row', () => {
    const streaming = makeItem({ id: 'row', status: 'streaming', summary: 'par' });
    expect(rowUiRetentionChanged(streaming, {
      ...streaming,
      summary: 'partial plus more streamed text',
      updatedAt: 12,
    })).toBe(false);
  });

  it('ignores every field change on a row that stays inactive', () => {
    const settled = makeItem({ id: 'row', status: 'completed' });
    expect(rowUiRetentionChanged(settled, {
      ...settled,
      summary: 'rewritten',
      payloadId: 'payload-late',
      meta: '{"pathRefs":[]}',
    })).toBe(false);
  });

  it('sees an active row settling', () => {
    const streaming = makeItem({ id: 'row', status: 'streaming' });
    expect(rowUiRetentionChanged(streaming, { ...streaming, status: 'completed' })).toBe(true);
    expect(rowUiRetentionChanged(streaming, { ...streaming, status: 'errored' })).toBe(true);
    expect(rowUiRetentionChanged(streaming, { ...streaming, status: 'killed' })).toBe(true);
  });

  it('sees a settled row going active', () => {
    const settled = makeItem({ id: 'row', status: 'completed' });
    expect(rowUiRetentionChanged(settled, { ...settled, status: 'running' })).toBe(true);
  });

  it('sees a move between the two active statuses', () => {
    const running = makeItem({ id: 'row', status: 'running' });
    expect(rowUiRetentionChanged(running, { ...running, status: 'streaming' })).toBe(true);
  });

  it('sees a payload attaching to an active row mid-stream', () => {
    const streaming = makeItem({ id: 'row', status: 'streaming' });
    expect(rowUiRetentionChanged(streaming, { ...streaming, payloadId: 'payload-1' })).toBe(true);
    // …and detaching, which retires the retained payload handle.
    expect(rowUiRetentionChanged(
      { ...streaming, payloadId: 'payload-1' },
      streaming,
    )).toBe(true);
  });

  it('sees the retained keys move under an active row', () => {
    const streaming = makeItem({ id: 'row', status: 'streaming' });
    expect(rowUiRetentionChanged(streaming, { ...streaming, id: 'other' })).toBe(true);
    expect(rowUiRetentionChanged(streaming, { ...streaming, threadId: 'thread-2' })).toBe(true);
  });

  it('treats an absent side as an append or a removal', () => {
    const streaming = makeItem({ id: 'row', status: 'streaming' });
    const settled = makeItem({ id: 'row', status: 'completed' });
    expect(rowUiRetentionChanged(undefined, streaming)).toBe(true);
    expect(rowUiRetentionChanged(undefined, settled)).toBe(false);
    expect(rowUiRetentionChanged(streaming, undefined)).toBe(true);
    expect(rowUiRetentionChanged(settled, undefined)).toBe(false);
    expect(rowUiRetentionChanged(undefined, undefined)).toBe(false);
  });
});
