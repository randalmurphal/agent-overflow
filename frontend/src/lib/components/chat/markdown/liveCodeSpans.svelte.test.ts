import { beforeEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock } from '../../../../test/mocks/bindings-app';
import { contentKey } from '../../../utils/fnv1a';
import { getCachedBlockSpans, resetCodeSpanCacheForTest } from './codeSpanCache';
import {
  LIVE_CODE_FINISHED_ROWS_MAX,
  TextLineChain,
  __liveCodeSpanStatsForTest,
  applyLiveCode,
  coverColorsWhole,
  coverLiveCode,
  coverPaints,
  liveCodeRow,
  liveCoverSpans,
  resetLiveCodeSpansForTest,
  resyncLiveCodeRows,
  retainLiveCodeThreads,
  stopLiveCodeFence,
  type HighlightLiveCodeEvent,
} from './liveCodeSpans.svelte';

// The chain a backend push carries for `text` (highlight.FrontendLineHashes).
function chainOf(text: string): number[] {
  const chain = new TextLineChain(text);
  return Array.from({ length: chain.lineCount }, (_, i) => chain.at(i));
}

// One distinct run per line, so a test can tell which line painted.
function linesOf(text: string, from = 0) {
  return text.split('\n').slice(from).map((_, i) => ({ r: [1, from + i + 1] }));
}

// A push with spans: seq 1 is the fence's first push, which has none.
function push(text: string, overrides: Partial<HighlightLiveCodeEvent> = {}): void {
  const from = overrides.from ?? 0;
  applyLiveCode(
    {
      threadId: 't1',
      itemId: 'i1',
      fence: 0,
      lang: 'python',
      seq: 2,
      from,
      lineHashes: chainOf(text).slice(from),
      lines: linesOf(text, from),
      final: false,
      head: from === 0 ? text.split('\n')[0] : undefined,
      ...overrides,
    },
    '',
  );
}

function cover(text: string, lang = 'python', streaming = true) {
  return coverLiveCode(liveCodeRow('t1', 'i1'), undefined, lang, new TextLineChain(text), streaming);
}

beforeEach(() => {
  resetLiveCodeSpansForTest();
  resetCodeSpanCacheForTest();
});

describe('TextLineChain', () => {
  it('builds the chain the backend sends, the same whether appended or reset', () => {
    // The last entry is the whole-text fnv1a contentKey is built from, the
    // same hash internal/highlight's FrontendLineHashes ends with.
    for (const text of ['abc', 'def route():\n    pass', '🎉 emoji\ncafé ☕', '\n\n', '']) {
      const chain = chainOf(text);
      expect(chain.length).toBe(text.split('\n').length);
      expect(contentKey(text)).toBe(`${text.length}:${chain[chain.length - 1]!.toString(36)}`);
      const grown = new TextLineChain('');
      for (const piece of text.match(/[\s\S]{1,3}/g) ?? []) grown.append(piece);
      expect(Array.from({ length: grown.lineCount }, (_, i) => grown.at(i))).toEqual(chain);
      expect(grown.firstLine).toBe(text.split('\n')[0]);
    }
  });

  it('keeps the first line as it arrives and forgets it on reset', () => {
    const chain = new TextLineChain('val');
    chain.append('ue = 1');
    expect(chain.firstLine).toBe('value = 1');
    chain.append('\nnext');
    expect(chain.firstLine).toBe('value = 1');
    chain.reset('other\nx');
    expect(chain.firstLine).toBe('other');
  });
});

describe('applyLiveCode', () => {
  it('splices deltas onto the fence and bumps the row', () => {
    push('a = 1\nb = 2\nc');
    const row = liveCodeRow('t1', 'i1')!;
    const before = row.version;
    push('a = 1\nb = 2\nc = 3\nd', { seq: 3, from: 2 });
    const fence = row.fences.get(0)!;
    expect(fence.lineHashes).toEqual(chainOf('a = 1\nb = 2\nc = 3\nd'));
    expect(fence.lines.map((line) => line.r![1])).toEqual([1, 2, 3, 4]);
    expect(row.version).toBeGreaterThan(before);
  });

  it('ignores a duplicate and an older push', () => {
    push('a = 1\nb');
    push('x = 9', { seq: 2 });
    push('y = 8', { seq: 1 });
    expect(cover('a = 1\nb')?.verified).toBe(2);
  });

  it('asks for a keyframe once on a gap, and stops the fences that ended meanwhile', async () => {
    let answer!: (open: number) => void;
    const resync = setBindingMock('ResyncLiveCode', () => new Promise<number>((resolve) => { answer = resolve; }));
    push('a = 1\nb');
    push('z = 0', { fence: 1 });
    push('a = 1\nb = 2\nc', { seq: 4, from: 1 });
    push('a = 1\nb = 2\nc = 3', { seq: 5, from: 1 });
    expect(resync).toHaveBeenCalledTimes(1);
    expect(resync).toHaveBeenCalledWith('t1', 'i1');
    // The skipped deltas did not apply.
    expect(liveCodeRow('t1', 'i1')!.fences.get(0)!.lineHashes).toEqual(chainOf('a = 1\nb'));

    answer(1);
    await vi.waitFor(() => expect(liveCodeRow('t1', 'i1')!.fences.get(0)!.stopped).toBe(true));
    expect(liveCodeRow('t1', 'i1')!.fences.get(1)!.stopped).toBe(false);
    expect(cover('a = 1\nb')).toBeNull();

    // The keyframe for the open fence applies whatever came before.
    push('z = 0\ny = 1', { fence: 1, seq: 7 });
    expect(cover('z = 0\ny = 1')?.verified).toBe(2);
  });

  it('asks for a keyframe for a row it has never seen', () => {
    const resync = setBindingMock('ResyncLiveCode', async () => 0);
    push('a = 1\nb', { seq: 4, from: 1 });
    expect(resync).toHaveBeenCalledWith('t1', 'i1');
  });

  it('stops every open fence when the resync fails', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    try {
      setBindingMock('ResyncLiveCode', async () => {
        throw new Error('gone');
      });
      push('a = 1\nb');
      push('a = 1\nb = 2', { seq: 4, from: 1 });
      await vi.waitFor(() => expect(liveCodeRow('t1', 'i1')!.fences.get(0)!.stopped).toBe(true));
      expect(warn).toHaveBeenCalled();
    } finally {
      warn.mockRestore();
    }
  });

  it('keeps a stopped fence out of every cover', () => {
    push('a = 1\nb');
    push('', { seq: 3, from: 2, lineHashes: null, lines: null, final: true });
    const fence = liveCodeRow('t1', 'i1')!.fences.get(0)!;
    expect(fence.stopped).toBe(true);
    expect(fence.lineHashes).toEqual(chainOf('a = 1\nb'));
    expect(cover('a = 1\nb')).toBeNull();
  });

  it('seeds the block cache with a final push and bounds the finished rows', () => {
    const text = 'def f():\n    pass';
    push(text, { final: true, contentKey: contentKey(text) });
    expect(getCachedBlockSpans('python', text)).toEqual(linesOf(text));
    expect(cover(text)?.exact).toBe(true);

    for (let i = 0; i < LIVE_CODE_FINISHED_ROWS_MAX + 3; i++) {
      applyLiveCode(
        { threadId: 't1', itemId: `done-${i}`, fence: 0, lang: 'python', seq: 1, from: 0, lineHashes: chainOf('x'), lines: linesOf('x'), final: true, contentKey: contentKey('x') },
        '',
      );
    }
    expect(__liveCodeSpanStatsForTest()).toEqual({ rows: LIVE_CODE_FINISHED_ROWS_MAX, finished: LIVE_CODE_FINISHED_ROWS_MAX });
    expect(liveCodeRow('t1', 'i1')).toBeUndefined();
  });

  it('never evicts a row with an open fence', () => {
    push('a = 1\nb');
    for (let i = 0; i < LIVE_CODE_FINISHED_ROWS_MAX + 3; i++) {
      applyLiveCode(
        { threadId: 't1', itemId: `done-${i}`, fence: 0, lang: 'python', seq: 1, from: 0, lineHashes: chainOf('x'), lines: linesOf('x'), final: true, contentKey: contentKey('x') },
        '',
      );
    }
    expect(liveCodeRow('t1', 'i1')).toBeDefined();
  });
});

describe('row lifetime', () => {
  it('drops the rows of threads no longer watched', () => {
    push('a = 1');
    push('a = 1', { threadId: 't2' });
    retainLiveCodeThreads(new Set(['t2']));
    expect(liveCodeRow('t1', 'i1')).toBeUndefined();
    expect(liveCodeRow('t2', 'i1')).toBeDefined();
  });

  it('resyncs only the rows of a backend that still have an open fence', () => {
    const resync = setBindingMock('ResyncLiveCode', async () => -1);
    push('a = 1');
    push('a = 1', { itemId: 'done', final: true, contentKey: contentKey('a = 1') });
    applyLiveCode({ threadId: 't9', itemId: 'far', fence: 0, lang: 'python', seq: 1, from: 0, lineHashes: chainOf('a'), lines: linesOf('a'), final: false }, 'mac');
    resyncLiveCodeRows('');
    expect(resync.mock.calls).toEqual([['t1', 'i1']]);
    resyncLiveCodeRows();
    expect(resync.mock.calls).toEqual([['t1', 'i1'], ['t9', 'far']]);
  });
});

describe('coverLiveCode', () => {
  it('verifies whole lines and clips the host partial last line', () => {
    push('a = 1\nb = 2\nc = 3');
    const partial = cover('a = 1\nb =')!;
    expect(partial).toMatchObject({ verified: 1, clip: true, exact: false });
    expect(liveCoverSpans(partial, 0)?.r).toEqual([1, 1]);
    expect(liveCoverSpans(partial, 1)?.r).toEqual([1, 2]);
    expect(liveCoverSpans(partial, 2)).toBeNull();
  });

  it('clips an open fence partial line onto a longer host line and leaves later lines plain', () => {
    push('a = 1\nb =');
    const ahead = cover('a = 1\nb = 2\nc = 3')!;
    expect(ahead).toMatchObject({ verified: 1, clip: true });
    expect(liveCoverSpans(ahead, 1)?.r).toEqual([1, 2]);
    expect(liveCoverSpans(ahead, 2)).toBeNull();
  });

  it('covers a host that is ahead of an open fence and refuses one past a final fence', () => {
    push('a = 1\nb = 2\n');
    expect(cover('a = 1\nb = 2\nc = 3')).toMatchObject({ verified: 2, clip: true });
    push('a = 1\nb = 2', { seq: 3, final: true, contentKey: contentKey('a = 1\nb = 2') });
    expect(cover('a = 1\nb = 2\nc = 3')).toBeNull();
    expect(cover('a = 1\nb')).toMatchObject({ verified: 1, clip: true, exact: false });
  });

  it('refuses a fence whose complete line differs from the host', () => {
    push('a = 1\nb = 2\nc = 3');
    expect(cover('a = 1\nX = 9\nc = 3')).toBeNull();
    expect(cover('completely different\nx')).toBeNull();
    expect(cover('a = 1\nb = 2', 'go')).toBeNull();
  });

  it('picks the fence that verifies the most lines', () => {
    push('a = 1\nb = 2');
    push('a = 1\nb = 2\nc = 3\nd', { fence: 1 });
    expect(cover('a = 1\nb = 2\nc = 3\nd')).toMatchObject({ verified: 4, exact: false });
    expect(cover('a = 1\nb = 2\nc = 3\nd')!.fence.index).toBe(1);
  });

  it('keeps painting from a held fence after its row is evicted', () => {
    const text = 'a = 1\nb = 2';
    push(text, { final: true, contentKey: contentKey(text) });
    const held = liveCodeRow('t1', 'i1')!.fences.get(0)!;
    retainLiveCodeThreads(new Set());
    expect(coverLiveCode(undefined, held, 'python', new TextLineChain('a = 1\nb'), true)).toMatchObject({ verified: 1, clip: true });
  });
});

describe('a fence before its lines are proven', () => {
  it('takes a first push without spans, which covers without coloring', () => {
    push('value_0 = 0', { seq: 1, lines: [{}] });
    const fence = liveCodeRow('t1', 'i1')!.fences.get(0)!;
    expect(fence).toMatchObject({ spanned: false, head: 'value_0 = 0' });
    const first = cover('value_0 = 0 * 2\nx')!;
    expect(first).toMatchObject({ verified: 0, clip: true });
    expect(coverPaints(first)).toBe(0);
    push('value_0 = 0 * 2\nx', { seq: 2 });
    expect(fence.spanned).toBe(true);
    expect(coverPaints(cover('value_0 = 0 * 2\nx')!)).toBe(2);
  });

  it('keeps the head a keyframe brought through later deltas', () => {
    push('first line\nb');
    push('first line\nb = 2\nc', { seq: 3, from: 1 });
    expect(liveCodeRow('t1', 'i1')!.fences.get(0)!.head).toBe('first line');
  });

  it('proves a first line by its head in either direction', () => {
    // The fence's partial first line is a prefix of the host's.
    push('value_0 = 0');
    expect(cover('value_0 = 0 * 2\nvalue_1')).toMatchObject({ verified: 0, clip: true });
    // The streaming host's partial first line is a prefix of the fence's.
    push('value_0 = 0 * 2\nvalue_1 = 1', { seq: 3 });
    expect(cover('valu')).toMatchObject({ verified: 0, clip: true });
    expect(liveCoverSpans(cover('valu')!, 0)?.r).toEqual([1, 1]);
  });

  it('refuses a first line that is neither prefix of the other', () => {
    push('foo()');
    expect(cover('x = 1')).toBeNull();
    expect(cover('x = 1\ny = 2')).toBeNull();
    push('foo()\nbar()', { seq: 3 });
    expect(cover('x =')).toBeNull();
  });

  it('treats a completed host last line as complete', () => {
    push('value_0 = 0 * 2\nvalue_1 = 1', { final: true, contentKey: contentKey('value_0 = 0 * 2\nvalue_1 = 1') });
    expect(cover('valu', 'python', true)).toMatchObject({ verified: 0, clip: true });
    expect(cover('valu', 'python', false)).toBeNull();
    expect(cover('value_0 = 0 * 2\nvalue_1', 'python', true)).toMatchObject({ verified: 1, clip: true });
    expect(cover('value_0 = 0 * 2\nvalue_1', 'python', false)).toBeNull();
  });

  it('colors a clipped line in full only when it is a prefix of the fence line', () => {
    // The fence's line 2 is complete; the streaming host shows part of it.
    push('a = 1\nb = 2\nc');
    const behind = cover('a = 1\nb =')!;
    expect(behind).toMatchObject({ verified: 1, clip: true, whole: true });
    expect([coverPaints(behind), coverColorsWhole(behind)]).toEqual([2, 2]);
    // The open fence's last line is partial; the host shows more of it.
    const ahead = cover('a = 1\nb = 2\nc = 3\nd')!;
    expect(ahead).toMatchObject({ verified: 2, clip: true, whole: false });
    expect([coverPaints(ahead), coverColorsWhole(ahead)]).toEqual([3, 2]);
    // Both last lines are partial: neither is known to hold the other.
    expect(cover('a = 1\nb = 2\nc = 3')).toMatchObject({ verified: 2, clip: true, whole: false });
    // On the first line the head tells which is the prefix.
    push('value_0 = 0', { fence: 1 });
    expect(coverLiveCode(liveCodeRow('t1', 'i1'), undefined, 'python', new TextLineChain('valu'), true))
      .toMatchObject({ verified: 0, clip: true, whole: true });
    expect(coverLiveCode(liveCodeRow('t1', 'i1'), undefined, 'python', new TextLineChain('value_0 = 0 * 2\nx'), true))
      .toMatchObject({ verified: 0, clip: true, whole: false });
  });

  it('stops a fence whose push could not be taken', () => {
    push('a = 1\nb');
    stopLiveCodeFence('t1', 'i1', 0);
    expect(liveCodeRow('t1', 'i1')!.fences.get(0)).toMatchObject({ final: true, stopped: true });
    expect(cover('a = 1\nb')).toBeNull();
    stopLiveCodeFence('t1', 'missing', 0);
  });
});
