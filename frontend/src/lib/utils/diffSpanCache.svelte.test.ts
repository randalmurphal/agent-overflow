import { beforeEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { getToasts } from '../stores/toast.svelte';
import {
  DIFF_SPAN_CACHE_MAX_BYTES,
  INCOMPLETE_RETRY_MS,
  diffSpanCacheGeneration,
  evictDiffSpansForThread,
  getSpansForLine,
  requestFileSpans,
  resetDiffSpanCacheForTest,
  seedPayloadPatchSpans,
  type PatchSpanSeedWire,
  __diffSpanCacheStatsForTest,
} from './diffSpanCache.svelte';
import { applyContextExpansion, nextExpansionVersion } from './diffContextExpansion';
import { contentKey } from './fnv1a';
import { parsePatchFiles, type PatchFile, type PatchLine } from './patchFiles';
import { resetSyntaxClassNamesForTest } from './syntaxSpans';

function makeFile(path: string, bodies: string[]): PatchFile {
  const lines: PatchLine[] = [
    { type: 'meta', content: `diff --git a/${path} b/${path}` },
    { type: 'meta', content: `@@ -1,1 +1,${bodies.length} @@` },
    ...bodies.map((body): PatchLine => ({ type: 'add', content: `+${body}` })),
  ];
  return { path, kind: 'modified', additions: bodies.length, deletions: 0, lines };
}

/** Plain-span result matching a file's line count. */
function plainResult(file: PatchFile) {
  return { lang: 'plaintext', lines: file.lines.map(() => ({})), truncated: false };
}

/** Result with one keyword run on every add line. */
function keywordResult(file: PatchFile) {
  return {
    lang: 'typescript',
    lines: file.lines.map((line) =>
      line.type === 'add' ? { r: [line.content.length - 1, 1] } : {},
    ),
    truncated: false,
  };
}

const workspaceContext = { scope: 'workspace', commitSHA: '', headSHA: '' };

beforeEach(() => {
  resetDiffSpanCacheForTest();
  setBindingMock('HighlightClassNames', async () => ['none', 'keyword']);
});

describe('requestFileSpans', () => {
  it('requests once per file content and aligns spans to line index', async () => {
    const file = makeFile('src/a.ts', ['const x = 1;', 'const y = 2;']);
    const rpc = setBindingMock('HighlightPatch', async () => keywordResult(file));

    await requestFileSpans(file, 'thread-1');

    expect(rpc).toHaveBeenCalledTimes(1);
    expect(rpc.mock.calls[0][0]).toEqual({
      path: 'src/a.ts',
      patch: file.lines.map((l) => l.content).join('\n'),
    });
    // Patch-aligned: meta lines plain, add lines carry their runs.
    expect(getSpansForLine(file, file.lines[0])?.r).toBeUndefined();
    expect(getSpansForLine(file, file.lines[2])?.r).toEqual(['const x = 1;'.length, 1]);
    expect(getSpansForLine(file, file.lines[3])?.r).toEqual(['const y = 2;'.length, 1]);

    // Second request (windowing remount) is a cache hit — no RPC.
    await requestFileSpans(file, 'thread-1');
    expect(rpc).toHaveBeenCalledTimes(1);
  });

  it('dedupes concurrent requests for the same content', async () => {
    const file = makeFile('src/b.ts', ['let a;']);
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const rpc = setBindingMock('HighlightPatch', async () => {
      await gate;
      return keywordResult(file);
    });

    const first = requestFileSpans(file, 'thread-1');
    const second = requestFileSpans(file, 'thread-1');
    release();
    await Promise.all([first, second]);

    expect(rpc).toHaveBeenCalledTimes(1);
    expect(getSpansForLine(file, file.lines[2])).not.toBeNull();
  });

  it('caches empty-success (all-plain) results', async () => {
    const file = makeFile('src/plain.xyz', ['whatever']);
    const rpc = setBindingMock('HighlightPatch', async () => plainResult(file));

    await requestFileSpans(file, 'thread-1');
    await requestFileSpans(file, 'thread-1');

    // Unknown language → plain spans is the backend's authoritative
    // answer; it must not be re-requested on every effect run.
    expect(rpc).toHaveBeenCalledTimes(1);
    expect(getSpansForLine(file, file.lines[2])?.r).toBeUndefined();
  });

  it('never caches a rejection and retries on the next request', async () => {
    const file = makeFile('src/flaky.ts', ['const x = 1;']);
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const toastsBefore = getToasts().length;
    let calls = 0;
    const rpc = setBindingMock('HighlightPatch', async () => {
      calls += 1;
      if (calls === 1) throw new Error('transient');
      return keywordResult(file);
    });

    await requestFileSpans(file, 'thread-1');
    expect(getSpansForLine(file, file.lines[2])).toBeNull();
    // Degraded signal: one toast per extension, plus a console warning.
    expect(getToasts().length).toBe(toastsBefore + 1);
    expect(warnSpy).toHaveBeenCalled();

    await requestFileSpans(file, 'thread-1');
    expect(rpc).toHaveBeenCalledTimes(2);
    expect(getSpansForLine(file, file.lines[2])).not.toBeNull();

    // A second failure on the same extension does not toast again.
    const other = makeFile('src/other.ts', ['let z;']);
    setBindingMock('HighlightPatch', async () => { throw new Error('again'); });
    await requestFileSpans(other, 'thread-1');
    expect(getToasts().length).toBe(toastsBefore + 1);
    warnSpy.mockRestore();
  });

  it('uses HighlightPatchWithContext when scope context is passed', async () => {
    const file = makeFile('src/scoped.ts', ['const x = 1;']);
    const primed = setBindingMock('HighlightPatchWithContext', async () => keywordResult(file));
    const unprimed = setBindingMock('HighlightPatch', async () => plainResult(file));

    await requestFileSpans(file, 'thread-1', workspaceContext);

    expect(primed).toHaveBeenCalledTimes(1);
    expect(primed.mock.calls[0]).toEqual([
      'thread-1',
      {
        scope: 'workspace',
        commitSHA: '',
        headSHA: '',
        path: 'src/scoped.ts',
        patch: file.lines.map((l) => l.content).join('\n'),
      },
    ]);
    expect(unprimed).not.toHaveBeenCalled();
  });

  it('falls back to HighlightPatch when the primed RPC rejects and does not retry the primed path', async () => {
    const file = makeFile('src/remote.ts', ['const x = 1;']);
    const primed = setBindingMock('HighlightPatchWithContext', async () => {
      throw new Error('method HighlightPatchWithContext not available remotely');
    });
    const unprimed = setBindingMock('HighlightPatch', async () => keywordResult(file));

    await requestFileSpans(file, 'thread-1', workspaceContext);
    expect(unprimed).toHaveBeenCalledTimes(1);
    expect(getSpansForLine(file, file.lines[2], 'thread-1', workspaceContext)).not.toBeNull();

    // The primed attempt is recorded under the scoped key:
    // re-requesting with context is a cache hit, not another doomed
    // LocalOnly round trip.
    await requestFileSpans(file, 'thread-1', workspaceContext);
    expect(primed).toHaveBeenCalledTimes(1);
    expect(unprimed).toHaveBeenCalledTimes(1);
  });

  it('keys primed results per (thread, scope) — identical patch bytes do not alias across contexts', async () => {
    // Two threads can hold the same path + patch text over DIFFERENT
    // underlying file content (the priming input), so a primed result
    // must never be served across contexts.
    const file = makeFile('src/ctx.ts', ['const x = 1;']);
    const primed = setBindingMock('HighlightPatchWithContext', async (threadId: string) =>
      threadId === 'thread-a' ? keywordResult(file) : plainResult(file),
    );

    await requestFileSpans(file, 'thread-a', workspaceContext);
    await requestFileSpans(file, 'thread-b', workspaceContext);
    expect(primed).toHaveBeenCalledTimes(2);

    expect(getSpansForLine(file, file.lines[2], 'thread-a', workspaceContext)?.r).toEqual([
      'const x = 1;'.length,
      1,
    ]);
    expect(
      getSpansForLine(file, file.lines[2], 'thread-b', workspaceContext)?.r,
    ).toBeUndefined();

    // Distinct scopes within one thread key apart too.
    await requestFileSpans(file, 'thread-a', { scope: 'commit', commitSHA: 'a1b2c3d', headSHA: '' });
    expect(primed).toHaveBeenCalledTimes(3);
  });

  it('scoped reads fall back to the unprimed entry until the primed result lands', async () => {
    const file = makeFile('src/fallback.ts', ['const x = 1;']);
    setBindingMock('HighlightPatch', async () => keywordResult(file));
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    setBindingMock('HighlightPatchWithContext', async () => {
      await gate;
      return plainResult(file);
    });

    await requestFileSpans(file, 'thread-1');
    const pending = requestFileSpans(file, 'thread-1', workspaceContext);

    // Primed in flight: the scoped read serves the shared unprimed
    // entry rather than nothing.
    expect(getSpansForLine(file, file.lines[2], 'thread-1', workspaceContext)?.r).toEqual([
      'const x = 1;'.length,
      1,
    ]);

    release();
    await pending;
    // Primed result landed: the scoped read now prefers it.
    expect(
      getSpansForLine(file, file.lines[2], 'thread-1', workspaceContext)?.r,
    ).toBeUndefined();
    // The unprimed entry is untouched for unscoped consumers.
    expect(getSpansForLine(file, file.lines[2])?.r).toEqual(['const x = 1;'.length, 1]);
  });

  it('serves predecessor spans for shared lines while an expanded file is in flight', async () => {
    // Context expansion rebuilds the lines array (new content key), so
    // the expanded file's own spans need a round trip. Already-visible
    // lines must keep their colors from the superseded array's entry —
    // expanding must not flash the whole file plain — while freshly
    // fetched lines render plain until the expanded result lands.
    const patchText = [
      'diff --git a/exp.ts b/exp.ts',
      '@@ -5,2 +5,2 @@',
      ' const kept = 1;',
      '-const removed = 2;',
      '+const added = 2;',
    ].join('\n');
    const file = parsePatchFiles(patchText)[0];
    setBindingMock('HighlightPatch', async () => keywordResult(file));
    await requestFileSpans(file, 'thread-1');
    const addLine = file.lines.find((line) => line.type === 'add')!;
    const addSpans = getSpansForLine(file, addLine);
    expect(addSpans).not.toBeNull();

    const expanded = applyContextExpansion(file, {
      lines: new Map([[4, 'above()']]),
      eofLine: null,
      version: nextExpansionVersion(),
    });
    expect(expanded).not.toBe(file);
    const fetched = expanded.lines.find((line) => line.content === ' above()')!;

    // The expanded file's request hasn't landed (nor even started):
    // shared lines resolve through the predecessor, new lines plain.
    expect(getSpansForLine(expanded, addLine)).toEqual(addSpans);
    expect(getSpansForLine(expanded, fetched)).toBeNull();

    // Once the expanded result lands it takes over, fetched line too.
    setBindingMock('HighlightPatch', async () => ({
      lang: 'typescript',
      lines: expanded.lines.map(() => ({ r: [1, 1] })),
      truncated: false,
    }));
    await requestFileSpans(expanded, 'thread-1');
    expect(getSpansForLine(expanded, fetched)?.r).toEqual([1, 1]);
    expect(getSpansForLine(expanded, addLine)?.r).toEqual([1, 1]);
  });

  it('keeps base spans reachable through the truncated chain during rapid expansions', async () => {
    // 5 expansion clicks before ANY expanded highlight request runs:
    // the chain is truncated, but it stays terminated at the base
    // array, whose spans are the only landed entry — shared lines must
    // not flash plain mid-burst.
    const patchText = [
      'diff --git a/burst.ts b/burst.ts',
      '@@ -7,2 +7,2 @@',
      ' const kept = 1;',
      '-const removed = 2;',
      '+const added = 2;',
    ].join('\n');
    const file = parsePatchFiles(patchText)[0];
    setBindingMock('HighlightPatch', async () => keywordResult(file));
    await requestFileSpans(file, 'thread-1');
    const addLine = file.lines.find((line) => line.type === 'add')!;
    const baseSpans = getSpansForLine(file, addLine);
    expect(baseSpans).not.toBeNull();

    const expansion = {
      lines: new Map<number, string>(),
      eofLine: null as number | null,
      version: 0,
    };
    let latest = file;
    for (const lineNo of [6, 5, 4, 3, 2]) {
      expansion.lines.set(lineNo, `line ${lineNo}`);
      expansion.version = nextExpansionVersion();
      latest = applyContextExpansion(file, expansion);
    }
    expect(getSpansForLine(latest, addLine)).toEqual(baseSpans);
  });

  it('paints a new array identity with an existing content-key entry on its first read', async () => {
    // Reload + repeating the same expansion rebuilds the array (new
    // identity, fresh context-line objects) with patch text whose
    // content key already has a landed entry. The FIRST render read
    // must resolve it — the request effect runs after render and its
    // cache hit bumps nothing, so a miss here would stay plain forever.
    const patchText = [
      'diff --git a/rekey.ts b/rekey.ts',
      '@@ -5,2 +5,2 @@',
      ' const kept = 1;',
      '-const removed = 2;',
      '+const added = 2;',
    ].join('\n');
    const file = parsePatchFiles(patchText)[0];
    const stateA = { lines: new Map([[4, 'above()']]), eofLine: null, version: nextExpansionVersion() };
    const expandedA = applyContextExpansion(file, stateA);
    setBindingMock('HighlightPatch', async () => ({
      lang: 'typescript',
      lines: expandedA.lines.map(() => ({ r: [1, 1] })),
      truncated: false,
    }));
    await requestFileSpans(expandedA, 'thread-1');

    // Same expansion content under a NEW state (post-reload): new
    // array identity, identical patch text.
    const stateB = { lines: new Map([[4, 'above()']]), eofLine: null, version: nextExpansionVersion() };
    const expandedB = applyContextExpansion(file, stateB);
    expect(expandedB).not.toBe(expandedA);
    expect(expandedB.lines).not.toBe(expandedA.lines);

    const fetched = expandedB.lines.find((line) => line.content === ' above()')!;
    expect(getSpansForLine(expandedB, fetched)?.r).toEqual([1, 1]);
  });

  it('a new lines-array identity (gap expansion) is a new content key', async () => {
    const file = makeFile('src/expand.ts', ['const x = 1;']);
    const rpc = setBindingMock('HighlightPatch', async () => keywordResult(file));
    await requestFileSpans(file, 'thread-1');

    const expanded = makeFile('src/expand.ts', ['const x = 1;', 'const y = 2;']);
    setBindingMock('HighlightPatch', async () => keywordResult(expanded));
    await requestFileSpans(expanded, 'thread-1');

    expect(rpc).toHaveBeenCalledTimes(1); // first fixture's RPC
    expect(getSpansForLine(expanded, expanded.lines[3])).not.toBeNull();
    // The old identity still resolves until evicted.
    expect(getSpansForLine(file, file.lines[2])).not.toBeNull();
  });

  it('resolves shared PatchLine objects through each array identity independently', async () => {
    // Gap expansion reuses the base array's PatchLine objects in a new
    // array where they sit at different indices. Each view must
    // resolve through its OWN array's entry — the expanded
    // registration must not hijack the base view's lookups.
    const base = makeFile('src/shared-lines.ts', ['const x = 1;']);
    setBindingMock('HighlightPatch', async () => keywordResult(base));
    await requestFileSpans(base, 'thread-1');

    const sharedLine = base.lines[2];
    const expandedLines: PatchLine[] = [
      base.lines[0],
      base.lines[1],
      { type: 'context', content: ' // context above' },
      sharedLine,
    ];
    const expanded: PatchFile = { ...base, lines: expandedLines };

    // Expanded file registered but its spans NOT yet landed.
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    setBindingMock('HighlightPatch', async () => {
      await gate;
      return keywordResult(expanded);
    });
    const pending = requestFileSpans(expanded, 'thread-1');

    // Base view still resolves its own entry; expanded view is plain
    // until its result lands.
    expect(getSpansForLine(base, sharedLine)?.r).toEqual(['const x = 1;'.length, 1]);
    expect(getSpansForLine(expanded, sharedLine)).toBeNull();

    release();
    await pending;
    // Both resolve now, each at its own index.
    expect(getSpansForLine(base, sharedLine)).not.toBeNull();
    expect(getSpansForLine(expanded, sharedLine)?.r).toEqual(['const x = 1;'.length, 1]);
  });

  it('sends conflict marker and fold rows as non-content marker lines', async () => {
    const rpc = setBindingMock('HighlightPatch', async () => ({
      lang: 'typescript',
      lines: [],
      truncated: false,
    }));
    const lines: PatchLine[] = [
      { type: 'meta', content: '@@ -1,2 +1,2 @@' },
      { type: 'marker', content: '<<<<<<< base `unterminated' },
      { type: 'add', content: '+const x = 1;' },
      { type: 'context', content: ' tail', fold: { id: 1, lines: 4 } },
    ];
    const file: PatchFile = { path: 'src/conflict.ts', kind: 'modified', additions: 1, deletions: 0, lines };

    await requestFileSpans(file, 'thread-1');

    // Marker/fold content never reaches the parser as context lines —
    // an unlucky marker label (backtick, quote) would poison the
    // reconstructed documents.
    expect((rpc.mock.calls[0][0] as { patch: string }).patch).toBe(
      '@@ -1,2 +1,2 @@\n\\ marker\n+const x = 1;\n\\ marker',
    );
  });

  it('discards a result whose last owner was evicted mid-flight', async () => {
    const file = makeFile('src/departed.ts', ['const x = 1;']);
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    setBindingMock('HighlightPatch', async () => {
      await gate;
      return keywordResult(file);
    });

    const pending = requestFileSpans(file, 'thread-1');
    const before = diffSpanCacheGeneration();
    evictDiffSpansForThread('thread-1');
    // The discarded result is invisible; the bump is what tells a
    // mounted consumer (same-thread reload) to re-request.
    expect(diffSpanCacheGeneration()).toBeGreaterThan(before);
    release();
    await pending;

    // Inserting would resurrect an entry no thread can evict.
    expect(__diffSpanCacheStatsForTest().entries).toBe(0);
    expect(getSpansForLine(file, file.lines[2])).toBeNull();
  });

  it('a same-thread reload mid-flight starts a fresh request and discards the stale result', async () => {
    // Reload (same-thread re-switch) while a request is pending: the
    // mounted consumer re-registers on the generation bump BEFORE the
    // old RPC settles. The old flight was computed against pre-reload
    // state (scoped requests prime from the workspace), so it must be
    // invalidated — not deduped into — and its late result discarded.
    const file = makeFile('src/reloaded.ts', ['const x = 1;']);
    let releaseStale!: () => void;
    const staleGate = new Promise<void>((resolve) => { releaseStale = resolve; });
    setBindingMock('HighlightPatch', async () => {
      await staleGate;
      return plainResult(file);
    });
    const stale = requestFileSpans(file, 'thread-1');

    evictDiffSpansForThread('thread-1');

    const rpc = setBindingMock('HighlightPatch', async () => keywordResult(file));
    await requestFileSpans(file, 'thread-1');
    expect(rpc).toHaveBeenCalledTimes(1);
    expect(getSpansForLine(file, file.lines[2])?.r).toEqual(['const x = 1;'.length, 1]);

    releaseStale();
    await stale;
    // The pre-reload result arrived late: still the fresh spans.
    expect(getSpansForLine(file, file.lines[2])?.r).toEqual(['const x = 1;'.length, 1]);
    expect(__diffSpanCacheStatsForTest().entries).toBe(1);
  });

  it('retries an incomplete result after the damping window instead of memoizing it for the session', async () => {
    vi.useFakeTimers();
    try {
      const file = makeFile('src/slow.ts', ['const x = 1;']);
      let calls = 0;
      const rpc = setBindingMock('HighlightPatch', async () => {
        calls += 1;
        return calls === 1 ? { ...plainResult(file), incomplete: true } : keywordResult(file);
      });

      await requestFileSpans(file, 'thread-1');
      // The partial result displays, and within the damping window a
      // re-run (generation bump) just touches — no re-parse storm.
      await requestFileSpans(file, 'thread-1');
      expect(rpc).toHaveBeenCalledTimes(1);

      vi.advanceTimersByTime(INCOMPLETE_RETRY_MS);
      await requestFileSpans(file, 'thread-1');
      expect(rpc).toHaveBeenCalledTimes(2);
      // The complete retry replaces the partial entry permanently.
      expect(getSpansForLine(file, file.lines[2])?.r).toEqual(['const x = 1;'.length, 1]);
      vi.advanceTimersByTime(INCOMPLETE_RETRY_MS);
      await requestFileSpans(file, 'thread-1');
      expect(rpc).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it('a failed incomplete refresh consumes the damping window', async () => {
    vi.useFakeTimers();
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    try {
      const file = makeFile('src/slow-flaky.ts', ['const x = 1;']);
      let calls = 0;
      const rpc = setBindingMock('HighlightPatch', async () => {
        calls += 1;
        if (calls === 1) return { ...plainResult(file), incomplete: true };
        if (calls === 2) throw new Error('transient');
        return keywordResult(file);
      });

      await requestFileSpans(file, 'thread-1');
      vi.advanceTimersByTime(INCOMPLETE_RETRY_MS);
      await requestFileSpans(file, 'thread-1'); // aged retry → rejects
      expect(rpc).toHaveBeenCalledTimes(2);

      // The failure stamped a fresh window: the next bump must NOT
      // retry immediately, and the partial entry keeps displaying.
      await requestFileSpans(file, 'thread-1');
      expect(rpc).toHaveBeenCalledTimes(2);
      expect(getSpansForLine(file, file.lines[2])).not.toBeNull();

      vi.advanceTimersByTime(INCOMPLETE_RETRY_MS);
      await requestFileSpans(file, 'thread-1');
      expect(rpc).toHaveBeenCalledTimes(3);
      expect(getSpansForLine(file, file.lines[2])?.r).toEqual(['const x = 1;'.length, 1]);
    } finally {
      warnSpy.mockRestore();
      vi.useRealTimers();
    }
  });

  it('drops the ownership record when a request fails', async () => {
    const file = makeFile('src/failing.ts', ['const f = 1;']);
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    setBindingMock('HighlightPatch', async () => { throw new Error('down'); });

    await requestFileSpans(file, 'thread-1');
    // No entry landed: keeping the record would accumulate one orphan
    // owner set per failed content key for the thread's lifetime.
    expect(__diffSpanCacheStatsForTest().ownerKeys).toBe(0);

    // A retry re-registers and succeeds normally.
    setBindingMock('HighlightPatch', async () => keywordResult(file));
    await requestFileSpans(file, 'thread-1');
    expect(__diffSpanCacheStatsForTest().ownerKeys).toBe(1);
    expect(getSpansForLine(file, file.lines[2])).not.toBeNull();
    warnSpy.mockRestore();
  });
});

describe('byte-budget eviction', () => {
  function hugeResult(file: PatchFile, runPairs: number) {
    return {
      lang: 'typescript',
      lines: [{ r: new Array(runPairs).fill(1) }, ...file.lines.slice(1).map(() => ({}))],
      truncated: false,
    };
  }

  it('evicts the oldest entry and its ownership record under byte pressure', async () => {
    // Two entries at ~5/8 of the budget each: inserting the second
    // evicts the first, including its keyThreads record — otherwise a
    // long-lived thread accumulates one orphaned owner set per evicted
    // patch version.
    const runPairs = Math.ceil((DIFF_SPAN_CACHE_MAX_BYTES * 0.625) / 8);
    const first = makeFile('src/big-a.ts', ['const a = 1;']);
    const second = makeFile('src/big-b.ts', ['const b = 2;']);
    setBindingMock('HighlightPatch', async (req: { path: string }) =>
      hugeResult(req.path === 'src/big-a.ts' ? first : second, runPairs),
    );

    await requestFileSpans(first, 'thread-1');
    const generationBefore = diffSpanCacheGeneration();
    await requestFileSpans(second, 'thread-1');

    const stats = __diffSpanCacheStatsForTest();
    expect(stats.entries).toBe(1);
    expect(stats.ownerKeys).toBe(1);
    expect(getSpansForLine(first, first.lines[0])).toBeNull();
    expect(getSpansForLine(second, second.lines[0])).not.toBeNull();
    // The eviction is visible to request effects: generation moved.
    expect(diffSpanCacheGeneration()).toBeGreaterThan(generationBefore);

    // A re-request after eviction works (ownership re-registers).
    await requestFileSpans(first, 'thread-1');
    expect(getSpansForLine(first, first.lines[0])).not.toBeNull();
  });

  it('never evicts hot entries — a mounted working set over budget stays resident instead of looping', async () => {
    // The request effects re-run on every generation bump and touch
    // their entries. If eviction could remove a touched (hot) entry,
    // two mounted files over the combined budget would evict each
    // other forever, one RPC per cycle.
    const runPairs = Math.ceil((DIFF_SPAN_CACHE_MAX_BYTES * 0.625) / 8);
    const first = makeFile('src/hot-a.ts', ['const a = 1;']);
    const second = makeFile('src/hot-b.ts', ['const b = 2;']);
    const rpc = setBindingMock('HighlightPatch', async (req: { path: string }) =>
      hugeResult(req.path === 'src/hot-a.ts' ? first : second, runPairs),
    );

    await requestFileSpans(first, 'thread-1');
    await requestFileSpans(second, 'thread-1'); // evicts first (cold)
    // Both consumers re-request on the bump: second is a hit (touch →
    // hot), first refetches.
    await requestFileSpans(second, 'thread-1');
    await requestFileSpans(first, 'thread-1');

    // The refetched insert must NOT evict the hot second entry: both
    // stay resident, over budget, bounded by the mounted working set.
    const stats = __diffSpanCacheStatsForTest();
    expect(stats.entries).toBe(2);
    expect(stats.bytes).toBeGreaterThan(DIFF_SPAN_CACHE_MAX_BYTES);
    expect(getSpansForLine(first, first.lines[0])).not.toBeNull();
    expect(getSpansForLine(second, second.lines[0])).not.toBeNull();

    // And the system is quiescent: further requests are cache hits.
    const calls = rpc.mock.calls.length;
    await requestFileSpans(first, 'thread-1');
    await requestFileSpans(second, 'thread-1');
    expect(rpc).toHaveBeenCalledTimes(calls);
  });

  it('keeps a single over-budget entry resident instead of self-evicting', async () => {
    const runPairs = Math.ceil((DIFF_SPAN_CACHE_MAX_BYTES * 1.5) / 8);
    const file = makeFile('src/huge.ts', ['const h = 1;']);
    setBindingMock('HighlightPatch', async () => hugeResult(file, runPairs));

    await requestFileSpans(file, 'thread-1');

    // Self-evicting the fresh insert would loop forever between insert
    // and the eviction-triggered re-request.
    expect(__diffSpanCacheStatsForTest().entries).toBe(1);
    expect(getSpansForLine(file, file.lines[0])).not.toBeNull();
  });
});

describe('evictDiffSpansForThread', () => {
  it('drops entries owned solely by the thread and keeps shared ones until the last owner leaves', async () => {
    const solo = makeFile('src/solo.ts', ['const s = 1;']);
    const shared = makeFile('src/shared.ts', ['const sh = 2;']);
    setBindingMock('HighlightPatch', async (req: { path: string }) =>
      keywordResult(req.path === 'src/solo.ts' ? solo : shared),
    );

    await requestFileSpans(solo, 'thread-a');
    await requestFileSpans(shared, 'thread-a');
    await requestFileSpans(shared, 'thread-b');
    expect(__diffSpanCacheStatsForTest().entries).toBe(2);

    evictDiffSpansForThread('thread-a');
    expect(getSpansForLine(solo, solo.lines[2])).toBeNull();
    expect(getSpansForLine(shared, shared.lines[2])).not.toBeNull();

    evictDiffSpansForThread('thread-b');
    expect(getSpansForLine(shared, shared.lines[2])).toBeNull();
    expect(__diffSpanCacheStatsForTest().entries).toBe(0);
    expect(__diffSpanCacheStatsForTest().bytes).toBe(0);
    expect(__diffSpanCacheStatsForTest().ownerKeys).toBe(0);
  });

  it('bumps the generation when entries are removed so mounted consumers re-request', async () => {
    const file = makeFile('src/reload.ts', ['const r = 1;']);
    setBindingMock('HighlightPatch', async () => keywordResult(file));
    await requestFileSpans(file, 'thread-a');

    // Same-thread reload evicts while the
    // review companion stays mounted; the bump re-runs its request
    // effect.
    const before = diffSpanCacheGeneration();
    evictDiffSpansForThread('thread-a');
    expect(diffSpanCacheGeneration()).toBeGreaterThan(before);

    // No-op eviction does not churn the generation.
    const after = diffSpanCacheGeneration();
    evictDiffSpansForThread('thread-a');
    expect(diffSpanCacheGeneration()).toBe(after);
  });
});

describe('seedPayloadPatchSpans', () => {
  function seedFor(file: PatchFile, overrides: Partial<PatchSpanSeedWire> = {}): PatchSpanSeedWire {
    return {
      path: file.path,
      contentKey: contentKey(file.lines.map((l) => l.content).join('\n')),
      lines: file.lines.map((line) =>
        line.type === 'add' ? { r: [line.content.length - 1, 1] } : {},
      ),
      ...overrides,
    };
  }

  it('serves seeded spans synchronously with no RPC', async () => {
    const file = makeFile('src/seeded.ts', ['const s = 1;']);
    const rpc = setBindingMock('HighlightPatch', async () => keywordResult(file));

    await seedPayloadPatchSpans('thread-1', [seedFor(file)]);

    expect(getSpansForLine(file, file.lines[2])?.r).toEqual(['const s = 1;'.length, 1]);
    await requestFileSpans(file, 'thread-1');
    expect(rpc).not.toHaveBeenCalled();
  });

  it('bumps the generation so a consumer that rendered plain repaints', async () => {
    const file = makeFile('src/race.ts', ['const r = 1;']);
    const before = diffSpanCacheGeneration();
    await seedPayloadPatchSpans('thread-1', [seedFor(file)]);
    expect(diffSpanCacheGeneration()).toBeGreaterThan(before);
  });

  it('keeps an existing complete entry instead of churning identical spans', async () => {
    const file = makeFile('src/keep.ts', ['const k = 1;']);
    setBindingMock('HighlightPatch', async () => keywordResult(file));
    await requestFileSpans(file, 'thread-1');
    const landed = diffSpanCacheGeneration();

    await seedPayloadPatchSpans('thread-1', [seedFor(file)]);
    expect(diffSpanCacheGeneration()).toBe(landed);
    expect(getSpansForLine(file, file.lines[2])).not.toBeNull();
  });

  it('replaces an incomplete RPC entry like any retry result', async () => {
    const file = makeFile('src/retry.ts', ['const t = 1;']);
    setBindingMock('HighlightPatch', async () => ({ ...plainResult(file), incomplete: true }));
    await requestFileSpans(file, 'thread-1');
    expect(getSpansForLine(file, file.lines[2])?.r).toBeUndefined();

    await seedPayloadPatchSpans('thread-1', [seedFor(file)]);
    expect(getSpansForLine(file, file.lines[2])?.r).toEqual(['const t = 1;'.length, 1]);
  });

  it('a late incomplete flight never downgrades a complete seeded entry', async () => {
    const file = makeFile('src/late.ts', ['const l = 1;']);
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    setBindingMock('HighlightPatch', async () => {
      await gate;
      return { ...plainResult(file), incomplete: true };
    });

    // The request goes out; while it is in flight the seed lands
    // complete spans for the same content key.
    const flight = requestFileSpans(file, 'thread-1');
    await seedPayloadPatchSpans('thread-1', [seedFor(file)]);
    expect(getSpansForLine(file, file.lines[2])?.r).toEqual(['const l = 1;'.length, 1]);

    release();
    await flight;
    // Complete answer for a content-addressed key is final.
    expect(getSpansForLine(file, file.lines[2])?.r).toEqual(['const l = 1;'.length, 1]);
  });

  it('aborts ingest when the thread is evicted during the class-table await', async () => {
    resetSyntaxClassNamesForTest();
    const file = makeFile('src/evicted.ts', ['const e = 1;']);
    let releaseTable!: () => void;
    const tableGate = new Promise<void>((resolve) => { releaseTable = resolve; });
    setBindingMock('HighlightClassNames', async () => {
      await tableGate;
      return ['none', 'keyword'];
    });

    const ingest = seedPayloadPatchSpans('thread-1', [seedFor(file)]);
    evictDiffSpansForThread('thread-1');
    releaseTable();
    await ingest;

    // The eviction already swept this thread; a post-await insert
    // would strand entries cleanup can no longer remove.
    expect(__diffSpanCacheStatsForTest().entries).toBe(0);
    expect(__diffSpanCacheStatsForTest().ownerKeys).toBe(0);
  });

  it('retains eviction epochs only for threads with a pending ingest', async () => {
    // No ingest awaiting the class table: eviction records nothing —
    // there is no in-flight registration to invalidate, and recording
    // would grow the map per evicted thread for the page's lifetime.
    evictDiffSpansForThread('thread-cold');
    expect(__diffSpanCacheStatsForTest().evictEpochs).toBe(0);

    resetSyntaxClassNamesForTest();
    const file = makeFile('src/epoch.ts', ['const p = 1;']);
    let releaseTable!: () => void;
    const tableGate = new Promise<void>((resolve) => { releaseTable = resolve; });
    setBindingMock('HighlightClassNames', async () => {
      await tableGate;
      return ['none', 'keyword'];
    });

    const ingest = seedPayloadPatchSpans('thread-1', [seedFor(file)]);
    // Churn on UNRELATED threads while thread-1's ingest awaits the
    // table must not accumulate: an epoch only invalidates ingests for
    // its own thread.
    evictDiffSpansForThread('thread-other-a');
    evictDiffSpansForThread('thread-other-b');
    expect(__diffSpanCacheStatsForTest().evictEpochs).toBe(0);
    evictDiffSpansForThread('thread-1');
    expect(__diffSpanCacheStatsForTest().evictEpochs).toBe(1);

    releaseTable();
    await ingest;
    // The thread's last pending ingest deletes its epoch on the way
    // out; the eviction still aborted the ingest's registration.
    expect(__diffSpanCacheStatsForTest().evictEpochs).toBe(0);
    expect(__diffSpanCacheStatsForTest().pendingIngestThreads).toBe(0);
    expect(__diffSpanCacheStatsForTest().entries).toBe(0);
  });

  it('a test reset mid-await invalidates the pre-reset ingest', async () => {
    resetSyntaxClassNamesForTest();
    const fileA = makeFile('src/reset-stale.ts', ['const a = 1;']);
    const fileB = makeFile('src/reset-fresh.ts', ['const b = 1;']);
    let releaseTable!: () => void;
    const tableGate = new Promise<void>((resolve) => { releaseTable = resolve; });
    setBindingMock('HighlightClassNames', async () => {
      await tableGate;
      return ['none', 'keyword'];
    });

    const stale = seedPayloadPatchSpans('thread-1', [seedFor(fileA)]);
    resetDiffSpanCacheForTest();
    // A post-reset ingest for the same thread: the stale continuation
    // must neither repopulate the reset cache nor consume this
    // ingest's pending count when it unwinds.
    const fresh = seedPayloadPatchSpans('thread-1', [seedFor(fileB)]);
    // The reset cleared the stale ingest's count; only fresh pends.
    expect(__diffSpanCacheStatsForTest().pendingIngestThreads).toBe(1);
    releaseTable();
    await Promise.all([stale, fresh]);
    expect(__diffSpanCacheStatsForTest().pendingIngestThreads).toBe(0);
    // Only the post-reset ingest landed entries.
    expect(__diffSpanCacheStatsForTest().entries).toBe(1);
    expect(getSpansForLine(fileB, fileB.lines[2])).not.toBeNull();
    expect(getSpansForLine(fileA, fileA.lines[2])).toBeNull();
  });

  it('never rejects when the class-table fetch fails', async () => {
    resetSyntaxClassNamesForTest();
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const file = makeFile('src/failed.ts', ['const f = 1;']);
    setBindingMock('HighlightClassNames', async () => {
      throw new Error('backend gone');
    });

    await expect(seedPayloadPatchSpans('thread-1', [seedFor(file)])).resolves.toBeUndefined();
    expect(__diffSpanCacheStatsForTest().entries).toBe(0);
    expect(warnSpy).toHaveBeenCalled();
    warnSpy.mockRestore();
  });

  it('registers thread ownership so eviction drops seeded entries', async () => {
    const file = makeFile('src/owned.ts', ['const o = 1;']);
    await seedPayloadPatchSpans('thread-1', [seedFor(file)]);
    expect(getSpansForLine(file, file.lines[2])).not.toBeNull();

    evictDiffSpansForThread('thread-1');
    expect(getSpansForLine(file, file.lines[2])).toBeNull();
    expect(__diffSpanCacheStatsForTest().entries).toBe(0);
  });

  it('skips malformed files and empty batches', async () => {
    const file = makeFile('src/bad.ts', ['const b = 1;']);
    await seedPayloadPatchSpans('thread-1', null);
    await seedPayloadPatchSpans('thread-1', []);
    await seedPayloadPatchSpans('thread-1', [
      seedFor(file, { path: undefined }),
      seedFor(file, { contentKey: undefined }),
      null as unknown as PatchSpanSeedWire,
    ]);
    expect(__diffSpanCacheStatsForTest().entries).toBe(0);
  });
});
