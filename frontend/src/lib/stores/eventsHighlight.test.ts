import { beforeEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { contentKey } from '../utils/fnv1a';
import {
  getCachedBlockSpans,
  resetCodeSpanCacheForTest,
} from '../components/chat/markdown/codeSpanCache';
import {
  TextLineChain,
  __liveCodeSpanStatsForTest,
  applyLiveCode,
  liveCodeRow,
  resetLiveCodeSpansForTest,
  type HighlightLiveCodeEvent,
} from '../components/chat/markdown/liveCodeSpans.svelte';
import {
  getSpansForLine,
  resetDiffSpanCacheForTest,
} from '../utils/diffSpanCache.svelte';
import { resetSyntaxClassNamesForTest } from '../utils/syntaxSpans';
import { requireHighlightSchema } from '../utils/highlightService';
import { HOME_BACKEND } from '../transport/backends';
import { parsePatchFiles } from '../utils/patchFiles';
import { makeThread } from '../../test/helpers/chat';
import { getThreads, prependThread, removeThread } from './threads.svelte';
import {
  applyHighlightDiffSeed,
  applyHighlightLive,
  setupHighlightLiveEvents,
  type HighlightDiffSeedEvent,
} from './eventsHighlight';
import { refreshWatchedThreads, registerWatchedThreadSource, resetWatchedThreadSourcesForTest } from './watchedThreads';
import { __resetClientLeaseForTest, setClientLease } from '../transport/lease';

const SOURCE = 'def f():\n    pass';

function chainOf(text: string): number[] {
  const chain = new TextLineChain(text);
  return Array.from({ length: chain.lineCount }, (_, i) => chain.at(i));
}

function liveEvent(overrides: Partial<HighlightLiveCodeEvent> = {}): HighlightLiveCodeEvent {
  return {
    threadId: 't1',
    itemId: 'i1',
    fence: 0,
    lang: 'python',
    seq: 2,
    from: 0,
    lineHashes: chainOf(SOURCE),
    lines: [{ r: [3, 1] }, {}],
    final: false,
    head: 'def f():',
    ...overrides,
  };
}

// The ingest awaits the class-name table before exposing spans.
function drain(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 0));
}

beforeEach(() => {
  resetCodeSpanCacheForTest();
  resetLiveCodeSpansForTest();
  resetDiffSpanCacheForTest();
  resetSyntaxClassNamesForTest();
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => ['none', 'keyword']);
  // Diff seeds only ingest for threads the client knows (deletion-race
  // guard); the module-level store persists across tests, so sweep it.
  for (const thread of getThreads()) removeThread(thread.id);
  prependThread(makeThread({ id: 't1' }));
});

describe('applyHighlightLive', () => {
  it('applies a push once the origin speaks the page schema', async () => {
    applyHighlightLive(liveEvent());
    expect(liveCodeRow('t1', 'i1')).toBeUndefined();
    await drain();
    expect(liveCodeRow('t1', 'i1')!.fences.get(0)!.lineHashes).toEqual(chainOf(SOURCE));
    expect(getCachedBlockSpans('python', SOURCE)).toBeNull();
  });

  it('warms the block cache under the backend contentKey for a final push', async () => {
    applyHighlightLive(liveEvent({ final: true, contentKey: contentKey(SOURCE) }));
    await drain();
    expect(getCachedBlockSpans('python', SOURCE)?.[0]?.r).toEqual([3, 1]);
  });

  it('drops malformed pushes and pushes for threads this client does not know', async () => {
    applyHighlightLive(null as unknown as HighlightLiveCodeEvent);
    applyHighlightLive(liveEvent({ lang: 7 as unknown as string }));
    applyHighlightLive(liveEvent({ seq: 0 }));
    applyHighlightLive(liveEvent({ fence: -1 }));
    applyHighlightLive(liveEvent({ from: 1.5 }));
    applyHighlightLive(liveEvent({ itemId: '' }));
    applyHighlightLive(liveEvent({ lines: [] }));
    applyHighlightLive(liveEvent({ threadId: 'unknown' }));
    await drain();
    expect(__liveCodeSpanStatsForTest().rows).toBe(0);
  });

  it('applies a fence\'s first push at once, before any schema check', () => {
    applyHighlightLive(liveEvent({ seq: 1, lines: [{}, {}] }));
    expect(liveCodeRow('t1', 'i1')!.fences.get(0)).toMatchObject({ seq: 1, spanned: false, head: 'def f():' });
    // A first push that names span classes waits like any other.
    applyHighlightLive(liveEvent({ fence: 1, seq: 1 }));
    expect(liveCodeRow('t1', 'i1')!.fences.get(1)).toBeUndefined();
  });

  it('applies pushes at once after the origin is proven', async () => {
    const resync = setBindingMock('ResyncLiveCode', async () => 0);
    applyHighlightLive(liveEvent());
    expect(liveCodeRow('t1', 'i1')).toBeUndefined();
    await drain();
    const fence = liveCodeRow('t1', 'i1')!.fences.get(0)!;
    applyHighlightLive(liveEvent({ seq: 3, from: 1, lineHashes: chainOf(SOURCE).slice(1), lines: [{}] }));
    expect(fence.seq).toBe(3);
    expect(resync).not.toHaveBeenCalled();
  });

  it('waits on the schema for a first push that names span classes', async () => {
    applyHighlightLive(liveEvent({ seq: 1 }));
    expect(liveCodeRow('t1', 'i1')).toBeUndefined();
    await drain();
    expect(liveCodeRow('t1', 'i1')!.fences.get(0)!.seq).toBe(1);
  });

  it('waits on the class-name table when only the origin is proven', async () => {
    await requireHighlightSchema(HOME_BACKEND);
    applyHighlightLive(liveEvent());
    expect(liveCodeRow('t1', 'i1')).toBeUndefined();
    await drain();
    expect(liveCodeRow('t1', 'i1')!.fences.get(0)!.seq).toBe(2);
  });

  it('applies pushes that wait in arrival order', async () => {
    const resync = setBindingMock('ResyncLiveCode', async () => 0);
    applyHighlightLive(liveEvent());
    applyHighlightLive(liveEvent({ seq: 3, from: 1, lineHashes: chainOf(SOURCE).slice(1), lines: [{}] }));
    expect(liveCodeRow('t1', 'i1')).toBeUndefined();
    await drain();
    expect(liveCodeRow('t1', 'i1')!.fences.get(0)!.seq).toBe(3);
    expect(resync).not.toHaveBeenCalled();
  });

  it('stops the fence of a push it cannot take', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    try {
      applyHighlightLive(liveEvent({ seq: 1, lines: [{}, {}] }));
      setBindingMock('HighlightClassNames', async () => {
        throw new Error('backend gone');
      });
      applyHighlightLive(liveEvent());
      await drain();
      expect(liveCodeRow('t1', 'i1')!.fences.get(0)).toMatchObject({ seq: 1, final: true, stopped: true });
    } finally {
      warn.mockRestore();
    }
  });

  it('drops a push whose origin speaks another schema', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    try {
      setBindingMock('HighlightClassNames', async () => {
        throw new Error('backend gone');
      });
      applyHighlightLive(liveEvent());
      await drain();
      expect(liveCodeRow('t1', 'i1')).toBeUndefined();
      expect(warn).toHaveBeenCalled();
    } finally {
      warn.mockRestore();
    }
  });
});

describe('setupHighlightLiveEvents', () => {
  function openRow(threadId = 't1'): void {
    applyLiveCode({ ...liveEvent(), threadId }, '');
  }

  it('asks open rows for keyframes on returning from a background lease', () => {
    const resync = setBindingMock('ResyncLiveCode', async () => 0);
    const off = setupHighlightLiveEvents();
    try {
      openRow();
      setClientLease('active');
      expect(resync).not.toHaveBeenCalled();
      setClientLease('background');
      expect(resync).not.toHaveBeenCalled();
      setClientLease('active');
      expect(resync).toHaveBeenCalledWith('t1', 'i1');
    } finally {
      off();
      __resetClientLeaseForTest();
    }
  });

  it('drops the rows of threads that leave the watched set', () => {
    const off = setupHighlightLiveEvents();
    const unregister = registerWatchedThreadSource(() => ['t2']);
    try {
      openRow('t1');
      openRow('t2');
      refreshWatchedThreads();
      expect(liveCodeRow('t1', 'i1')).toBeUndefined();
      expect(liveCodeRow('t2', 'i1')).toBeDefined();
    } finally {
      unregister();
      off();
      resetWatchedThreadSourcesForTest();
    }
  });
});

describe('applyHighlightDiffSeed', () => {
  const PATCH =
    'diff --git a/src/app.py b/src/app.py\n' +
    '--- a/src/app.py\n' +
    '+++ b/src/app.py\n' +
    '@@ -0,0 +1 @@\n' +
    '+pass';

  it('warms the diff span cache under the backend-computed key', async () => {
    applyHighlightDiffSeed({
      threadId: 't1',
      files: [
        {
          path: 'src/app.py',
          contentKey: contentKey(PATCH),
          lines: [{}, {}, {}, {}, { r: [4, 1] }],
        },
      ],
    });
    await drain();

    const file = parsePatchFiles(PATCH)[0]!;
    expect(getSpansForLine(file, file.lines[4]!)?.r).toEqual([4, 1]);
  });

  it('never rejects, even when the class table fails to load', async () => {
    setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
    setBindingMock('HighlightClassNames', async () => {
      throw new Error('backend gone');
    });
    // A rejection here would surface as an unhandled rejection and
    // fail the test run — best-effort ingest must swallow it.
    applyHighlightDiffSeed({
      threadId: 't1',
      files: [{ path: 'src/app.py', contentKey: contentKey(PATCH), lines: [] }],
    });
    await drain();
    const file = parsePatchFiles(PATCH)[0]!;
    expect(getSpansForLine(file, file.lines[0]!)).toBeNull();
  });

  it('drops seeds for threads the client no longer knows (deletion race)', async () => {
    // Thread deletion removes the row and evicts the diff span cache in
    // one pass; a seed whose backend worker outraced the delete arrives
    // after that cleanup and must not re-register entries.
    removeThread('t1');
    applyHighlightDiffSeed({
      threadId: 't1',
      files: [
        {
          path: 'src/app.py',
          contentKey: contentKey(PATCH),
          lines: [{}, {}, {}, {}, { r: [4, 1] }],
        },
      ],
    });
    await drain();

    const file = parsePatchFiles(PATCH)[0]!;
    expect(getSpansForLine(file, file.lines[4]!)).toBeNull();
  });

  it('drops malformed events and files', async () => {
    applyHighlightDiffSeed(null as unknown as HighlightDiffSeedEvent);
    applyHighlightDiffSeed({ threadId: 't1', files: null });
    applyHighlightDiffSeed({
      threadId: 't1',
      files: [{ path: 'x', contentKey: 7 as unknown as string, lines: [] }],
    });
    await drain();

    const file = parsePatchFiles(PATCH)[0]!;
    expect(getSpansForLine(file, file.lines[0]!)).toBeNull();
  });
});
