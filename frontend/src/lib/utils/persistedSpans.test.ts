import { beforeEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { getCachedBlockSpans, resetCodeSpanCacheForTest } from '../components/chat/markdown/codeSpanCache';
import {
  __diffSpanCacheStatsForTest,
  evictDiffSpansForThread,
  getSpansForLine,
  resetDiffSpanCacheForTest,
} from './diffSpanCache.svelte';
import { contentKey } from './fnv1a';
import { parsePatchFiles } from './patchFiles';
import { ingestPersistedCodeSpans, ingestPersistedPatchSpans } from './persistedSpans';
import {
  ensureHighlightSchemaVersion,
  ensureSyntaxClassNames,
  resetSyntaxClassNamesForTest,
} from './syntaxSpans';

const HV = 'hv-test';

const PATCH_TEXT = ['diff --git a/src/a.py b/src/a.py', '@@ -1,1 +1,2 @@', '+def f():'].join('\n');

function codeSpansMeta(hv: string): string {
  return JSON.stringify({
    pathRefs: [{ path: 'a.py' }],
    codeSpans: {
      hv,
      blocks: [{ lang: 'python', contentKey: contentKey('def f():\n    pass'), lines: [{ r: [3, 1] }] }],
    },
  });
}

function patchSpansBlob(hv: string): string {
  return JSON.stringify({
    hv,
    files: [{ path: 'src/a.py', contentKey: contentKey(PATCH_TEXT), lines: [{}, {}, { r: [8, 1] }] }],
  });
}

/** Loads the version + class-name tables so ingest takes the sync path
 * (the steady-state cold-mount scenario the feature exists for). */
async function warmTables(): Promise<void> {
  await ensureHighlightSchemaVersion();
  await ensureSyntaxClassNames();
}

beforeEach(() => {
  resetCodeSpanCacheForTest();
  resetDiffSpanCacheForTest();
  resetSyntaxClassNamesForTest();
  setBindingMock('HighlightSchemaVersion', async () => HV);
  setBindingMock('HighlightClassNames', async () => ['none', 'keyword']);
});

describe('ingestPersistedCodeSpans', () => {
  it('seeds the code span cache synchronously once tables are loaded', async () => {
    await warmTables();

    ingestPersistedCodeSpans(codeSpansMeta(HV));

    // Synchronous — no await between ingest and the read, which is the
    // property that lets a row init seed before its children mount.
    expect(getCachedBlockSpans('python', 'def f():\n    pass')).toEqual([{ r: [3, 1] }]);
  });

  it('resolves the tables on first ingest and seeds when they land', async () => {
    ingestPersistedCodeSpans(codeSpansMeta(HV));
    expect(getCachedBlockSpans('python', 'def f():\n    pass')).toBeNull();

    await vi.waitFor(() => {
      expect(getCachedBlockSpans('python', 'def f():\n    pass')).toEqual([{ r: [3, 1] }]);
    });
  });

  it('drops blobs stamped with another schema version', async () => {
    await warmTables();

    ingestPersistedCodeSpans(codeSpansMeta('stale-schema'));
    await Promise.resolve();

    // The stale blob never seeds — the block's mount falls back to the
    // RPC path, which recomputes under the connected backend's grammar.
    expect(getCachedBlockSpans('python', 'def f():\n    pass')).toBeNull();
  });

  it('ignores metas without a well-formed codeSpans value', async () => {
    await warmTables();
    for (const meta of [
      undefined,
      '',
      'not-json',
      '{}',
      JSON.stringify({ codeSpans: { hv: HV, blocks: [] } }),
      JSON.stringify({ codeSpans: { blocks: [{ lang: 'python', contentKey: 'k' }] } }),
    ]) {
      expect(() => ingestPersistedCodeSpans(meta)).not.toThrow();
    }
    expect(getCachedBlockSpans('python', 'def f():\n    pass')).toBeNull();
  });

  it('skips malformed blocks while seeding valid siblings', async () => {
    await warmTables();
    ingestPersistedCodeSpans(
      JSON.stringify({
        codeSpans: {
          hv: HV,
          blocks: [
            null,
            { contentKey: 'orphan' },
            { lang: 'go', contentKey: contentKey('ok := true'), lines: [{ r: [2, 1] }] },
          ],
        },
      }),
    );
    expect(getCachedBlockSpans('go', 'ok := true')).toEqual([{ r: [2, 1] }]);
  });
});

describe('ingestPersistedPatchSpans', () => {
  function patchFile() {
    const files = parsePatchFiles(PATCH_TEXT);
    expect(files).toHaveLength(1);
    return files[0];
  }

  it('seeds the diff span cache synchronously once tables are loaded', async () => {
    await warmTables();

    ingestPersistedPatchSpans('thread-1', patchSpansBlob(HV));

    const file = patchFile();
    expect(getSpansForLine(file, file.lines[2])).toEqual({ r: [8, 1] });
  });

  it('registers thread ownership so eviction covers ingested entries', async () => {
    await warmTables();
    ingestPersistedPatchSpans('thread-1', patchSpansBlob(HV));
    const file = patchFile();
    expect(getSpansForLine(file, file.lines[2])).not.toBeNull();

    evictDiffSpansForThread('thread-1');
    expect(getSpansForLine(file, file.lines[2])).toBeNull();

    // The evict → remount transition re-seeds (ingest is deliberately
    // unmemoized): reopening the thread paints from the blob again.
    ingestPersistedPatchSpans('thread-1', patchSpansBlob(HV));
    expect(getSpansForLine(file, file.lines[2])).toEqual({ r: [8, 1] });
  });

  it('drops blobs stamped with another schema version', async () => {
    await warmTables();
    ingestPersistedPatchSpans('thread-1', patchSpansBlob('stale-schema'));
    await Promise.resolve();
    const file = patchFile();
    expect(getSpansForLine(file, file.lines[2])).toBeNull();
  });

  it('resolves the tables on first ingest and seeds when they land', async () => {
    ingestPersistedPatchSpans('thread-1', patchSpansBlob(HV));
    const file = patchFile();
    expect(getSpansForLine(file, file.lines[2])).toBeNull();

    await vi.waitFor(() => {
      expect(getSpansForLine(file, file.lines[2])).toEqual({ r: [8, 1] });
    });
    // The pending-ingest registration unwound cleanly.
    expect(__diffSpanCacheStatsForTest().pendingIngestThreads).toBe(0);
  });

  it('drops a cold-table ingest whose thread is evicted mid-fetch', async () => {
    // Hold the version fetch so the ingest is parked in its await
    // window when the eviction sweep runs (thread switch / deletion
    // before the first-load tables land).
    let releaseVersion!: (v: string) => void;
    setBindingMock(
      'HighlightSchemaVersion',
      () => new Promise<string>((resolve) => (releaseVersion = resolve)),
    );

    ingestPersistedPatchSpans('thread-1', patchSpansBlob(HV));
    expect(__diffSpanCacheStatsForTest().pendingIngestThreads).toBe(1);
    evictDiffSpansForThread('thread-1');
    releaseVersion(HV);

    await vi.waitFor(() => {
      expect(__diffSpanCacheStatsForTest().pendingIngestThreads).toBe(0);
    });
    // The late continuation must not re-register entries cleanup
    // already swept.
    const file = patchFile();
    expect(getSpansForLine(file, file.lines[2])).toBeNull();
    expect(__diffSpanCacheStatsForTest().ownerKeys).toBe(0);
  });

  it('drops a cold-table ingest when the fetched version mismatches the blob', async () => {
    ingestPersistedPatchSpans('thread-1', patchSpansBlob('stale-schema'));

    await vi.waitFor(() => {
      expect(__diffSpanCacheStatsForTest().pendingIngestThreads).toBe(0);
    });
    const file = patchFile();
    expect(getSpansForLine(file, file.lines[2])).toBeNull();
  });

  it('ignores malformed blobs without throwing', async () => {
    await warmTables();
    for (const blob of [undefined, '', 'not-json', '{}', JSON.stringify({ hv: HV, files: [] })]) {
      expect(() => ingestPersistedPatchSpans('thread-1', blob)).not.toThrow();
    }
    const file = patchFile();
    expect(getSpansForLine(file, file.lines[2])).toBeNull();
  });
});
