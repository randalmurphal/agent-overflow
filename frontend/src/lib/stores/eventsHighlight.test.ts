import { beforeEach, describe, expect, it } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { contentKey } from '../utils/fnv1a';
import {
  getCachedBlockSpans,
  resetCodeSpanCacheForTest,
} from '../components/chat/markdown/codeSpanCache';
import {
  lineHashChain,
  matchLiveCodeSeed,
  resetLiveCodeSeedsForTest,
  __liveCodeSeedStatsForTest,
  type HighlightSeedEvent,
} from '../components/chat/markdown/liveCodeSeeds.svelte';
import {
  getSpansForLine,
  resetDiffSpanCacheForTest,
} from '../utils/diffSpanCache.svelte';
import { resetSyntaxClassNamesForTest } from '../utils/syntaxSpans';
import { parsePatchFiles } from '../utils/patchFiles';
import { makeThread } from '../../test/helpers/chat';
import { getThreads, prependThread, removeThread } from './threads.svelte';
import {
  applyHighlightDiffSeed,
  applyHighlightSeed,
  type HighlightDiffSeedEvent,
} from './eventsHighlight';

const SOURCE = 'def f():\n    pass';

function seedEvent(overrides: Partial<HighlightSeedEvent> = {}): HighlightSeedEvent {
  return {
    threadId: 't1',
    itemId: 'i1',
    lang: 'python',
    lineHashes: lineHashChain(SOURCE),
    lines: [{ r: [3, 1] }, {}],
    final: false,
    ...overrides,
  };
}

// The ingest awaits the class-name table before exposing spans.
function drain(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 0));
}

beforeEach(() => {
  resetCodeSpanCacheForTest();
  resetLiveCodeSeedsForTest();
  resetDiffSpanCacheForTest();
  resetSyntaxClassNamesForTest();
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => ['none', 'keyword']);
  // Diff seeds only ingest for threads the client knows (deletion-race
  // guard); the module-level store persists across tests, so sweep it.
  for (const thread of getThreads()) removeThread(thread.id);
  prependThread(makeThread({ id: 't1' }));
});

describe('applyHighlightSeed', () => {
  it('feeds the live-seed table for non-final seeds without touching the block cache', async () => {
    applyHighlightSeed(seedEvent());
    await drain();
    expect(matchLiveCodeSeed('python', SOURCE)?.exact).toBe(true);
    expect(getCachedBlockSpans('python', SOURCE)).toBeNull();
  });

  it('warms the block cache under the backend contentKey for final seeds', async () => {
    applyHighlightSeed(seedEvent({ final: true, contentKey: contentKey(SOURCE) }));
    await drain();
    expect(getCachedBlockSpans('python', SOURCE)?.[0]?.r).toEqual([3, 1]);
    expect(matchLiveCodeSeed('python', SOURCE)?.exact).toBe(true);
  });

  it('drops malformed events', async () => {
    applyHighlightSeed(null as unknown as HighlightSeedEvent);
    applyHighlightSeed(seedEvent({ lang: 7 as unknown as string }));
    applyHighlightSeed(seedEvent({ lineHashes: null }));
    await drain();
    expect(__liveCodeSeedStatsForTest().entries).toBe(0);
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
