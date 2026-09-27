import { beforeEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock } from '../../../../test/mocks/bindings-app';
import { getToasts } from '../../../stores/toast.svelte';
import { contentKey } from '../../../utils/fnv1a';
import { createProvenAppend } from '../../../markdown';
import {
  appendCodeSourceIdentity,
  CODE_SPAN_CACHE_MAX_ENTRIES,
  createCodeSourceIdentity,
  getCachedBlockSpans,
  getCachedBlockSpansByIdentity,
  requestBlockSpans,
  requestBlockSpansByIdentity,
  resetCodeSpanCacheForTest,
  seedFinalBlockSpans,
  __codeSpanCacheStatsForTest,
} from './codeSpanCache';

function result(runs: number[]) {
  return { lang: 'python', lines: [{ r: runs }], truncated: false };
}

beforeEach(() => {
  resetCodeSpanCacheForTest();
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => ['none', 'keyword']);
});

describe('codeSpanCache', () => {
  it('extends a source identity from only an opaque proven suffix', async () => {
    const rpc = setBindingMock('HighlightCode', async () => result([3, 1]));
    const initial = 'const greeting = "caf';
    const append = createProvenAppend(initial, 'é 🎉";');
    const identity = appendCodeSourceIdentity(createCodeSourceIdentity(initial), append);

    expect(identity.source).toBe(append.next);
    expect(identity.contentKey).toBe(contentKey(append.next));
    expect(getCachedBlockSpansByIdentity('typescript', identity)).toBeNull();
    await requestBlockSpansByIdentity('typescript', identity);
    expect(getCachedBlockSpansByIdentity('typescript', identity)).not.toBeNull();
    expect(rpc).toHaveBeenCalledWith({ lang: 'typescript', source: append.next });
  });

  it('rejects stale and fabricated source identities', () => {
    const identity = createCodeSourceIdentity('old');
    expect(() => appendCodeSourceIdentity(identity, createProvenAppend('other', ' value')))
      .toThrow('does not match');
    expect(() => getCachedBlockSpansByIdentity('typescript', {
      source: 'old',
      contentKey: contentKey('old'),
      hash: 0,
    })).toThrow('requires an identity minted');
  });

  it('caches success and serves it synchronously afterwards', async () => {
    const rpc = setBindingMock('HighlightCode', async () => result([3, 1]));

    expect(getCachedBlockSpans('python', 'def')).toBeNull();
    const spans = await requestBlockSpans('python', 'def');
    expect(spans?.[0]?.r).toEqual([3, 1]);
    expect(getCachedBlockSpans('python', 'def')?.[0]?.r).toEqual([3, 1]);

    await requestBlockSpans('python', 'def');
    expect(rpc).toHaveBeenCalledTimes(1);
  });

  it('dedupes concurrent requests for identical content', async () => {
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const rpc = setBindingMock('HighlightCode', async () => {
      await gate;
      return result([3, 1]);
    });

    const a = requestBlockSpans('python', 'def');
    const b = requestBlockSpans('python', 'def');
    release();
    const [ra, rb] = await Promise.all([a, b]);

    expect(rpc).toHaveBeenCalledTimes(1);
    expect(ra).toBe(rb);
  });

  it('keys by language as well as content', async () => {
    const rpc = setBindingMock('HighlightCode', async () => result([3, 1]));
    await requestBlockSpans('python', 'def');
    await requestBlockSpans('go', 'def');
    expect(rpc).toHaveBeenCalledTimes(2);
  });

  it('never caches a rejection, toasts once per language, and retries', async () => {
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const toastsBefore = getToasts().length;
    let calls = 0;
    const rpc = setBindingMock('HighlightCode', async () => {
      calls += 1;
      if (calls <= 2) throw new Error('transient');
      return result([3, 1]);
    });

    expect(await requestBlockSpans('python', 'def')).toBeNull();
    expect(getCachedBlockSpans('python', 'def')).toBeNull();
    expect(await requestBlockSpans('python', 'other')).toBeNull();
    expect(getToasts().length).toBe(toastsBefore + 1);

    expect((await requestBlockSpans('python', 'def'))?.[0]?.r).toEqual([3, 1]);
    expect(rpc).toHaveBeenCalledTimes(3);
    warnSpy.mockRestore();
  });

  it('returns but never caches an incomplete result, so the next mount retries', async () => {
    // incomplete = transient backend degradation (parse timeout under
    // load). The caller gets the partial spans for display, but
    // memoizing them would pin the block partially-plain for the page
    // lifetime even after the backend recovers.
    let calls = 0;
    const rpc = setBindingMock('HighlightCode', async () => {
      calls += 1;
      return calls === 1 ? { ...result([3, 1]), incomplete: true } : result([3, 1]);
    });

    expect((await requestBlockSpans('python', 'def'))?.[0]?.r).toEqual([3, 1]);
    expect(getCachedBlockSpans('python', 'def')).toBeNull();

    expect((await requestBlockSpans('python', 'def'))?.[0]?.r).toEqual([3, 1]);
    expect(rpc).toHaveBeenCalledTimes(2);
    expect(getCachedBlockSpans('python', 'def')).not.toBeNull();
  });

  it('serves a backend-pushed final seed synchronously without any RPC', async () => {
    // The remote seed path: the backend computed contentKey(source)
    // with frontend hash parity, so a seeded entry must be a sync hit
    // for the exact source — and count against the same LRU cap.
    const rpc = setBindingMock('HighlightCode', async () => result([3, 1]));
    const source = 'def f():\n    pass';
    seedFinalBlockSpans('python', contentKey(source), [{ r: [3, 1] }, {}]);

    expect(getCachedBlockSpans('python', source)?.[0]?.r).toEqual([3, 1]);
    expect((await requestBlockSpans('python', source))?.[0]?.r).toEqual([3, 1]);
    expect(rpc).not.toHaveBeenCalled();

    // Different content or language misses; empty inputs are dropped.
    expect(getCachedBlockSpans('python', source + '\n')).toBeNull();
    expect(getCachedBlockSpans('go', source)).toBeNull();
    seedFinalBlockSpans('', contentKey(source), [{}]);
    seedFinalBlockSpans('python', '', [{}]);
    expect(__codeSpanCacheStatsForTest().entries).toBe(1);
  });

  it('evicts least-recently-used entries past the cap', async () => {
    setBindingMock('HighlightCode', async () => result([1, 1]));
    for (let i = 0; i < CODE_SPAN_CACHE_MAX_ENTRIES + 1; i += 1) {
      await requestBlockSpans('python', `source-${i}`);
    }
    expect(__codeSpanCacheStatsForTest().entries).toBe(CODE_SPAN_CACHE_MAX_ENTRIES);
    // source-0 was the oldest untouched entry.
    expect(getCachedBlockSpans('python', 'source-0')).toBeNull();
    expect(getCachedBlockSpans('python', 'source-1')).not.toBeNull();
  });

  describe('a streaming block', () => {
    // Identities for successive prefixes of one growing fence.
    function prefixes(count: number) {
      let identity = createCodeSourceIdentity('let x');
      const all = [identity];
      for (let i = 1; i < count; i += 1) {
        identity = appendCodeSourceIdentity(identity, createProvenAppend(identity.source, ` + ${i}`));
        all.push(identity);
      }
      return all;
    }

    it('keeps only its latest result', async () => {
      setBindingMock('HighlightCode', async () => result([3, 1]));
      const owner = {};
      const otherPane = {};
      const all = prefixes(40);
      for (const identity of all) {
        await requestBlockSpansByIdentity('python', identity, owner);
        // The same block streaming in another pane reads it.
        expect(getCachedBlockSpansByIdentity('python', identity, otherPane)).not.toBeNull();
      }

      expect(__codeSpanCacheStatsForTest().entries).toBe(1);
      expect(getCachedBlockSpansByIdentity('python', all[39])).not.toBeNull();
    });

    it('keeps a result a complete block read', async () => {
      setBindingMock('HighlightCode', async () => result([3, 1]));
      const owner = {};
      const [first, second] = prefixes(2);
      await requestBlockSpansByIdentity('python', first, owner);
      expect(getCachedBlockSpans('python', first.source)).not.toBeNull();

      await requestBlockSpansByIdentity('python', second, owner);
      expect(getCachedBlockSpans('python', first.source)).not.toBeNull();
      expect(__codeSpanCacheStatsForTest().entries).toBe(2);
    });

    it('keeps a result a complete block also requested', async () => {
      let release!: () => void;
      const gate = new Promise<void>((resolve) => { release = resolve; });
      setBindingMock('HighlightCode', async () => {
        await gate;
        return result([3, 1]);
      });
      const owner = {};
      const [first, second] = prefixes(2);
      const streaming = requestBlockSpansByIdentity('python', first, owner);
      const complete = requestBlockSpansByIdentity('python', first);
      release();
      expect(await complete).toBe(await streaming);

      await requestBlockSpansByIdentity('python', second, owner);
      expect(getCachedBlockSpans('python', first.source)).not.toBeNull();
    });

    it('keeps a final seed that lands while its request is pending', async () => {
      let release!: () => void;
      const gate = new Promise<void>((resolve) => { release = resolve; });
      setBindingMock('HighlightCode', async () => {
        await gate;
        return result([3, 1]);
      });
      const owner = {};
      const [first, second] = prefixes(2);
      const streaming = requestBlockSpansByIdentity('python', first, owner);
      seedFinalBlockSpans('python', first.contentKey, [{ r: [3, 1] }]);
      release();
      await streaming;

      setBindingMock('HighlightCode', async () => result([3, 1]));
      await requestBlockSpansByIdentity('python', second, owner);
      expect(getCachedBlockSpans('python', first.source)).not.toBeNull();
    });
  });
});
