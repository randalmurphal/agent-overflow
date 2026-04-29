import { describe, expect, it } from 'vitest';
import {
  EMPTY_TURN_DIFF_SUMMARY,
  buildDiffViewForItems,
  buildTurnDiffView,
  changedFilesForItem,
  summarizeTurnDiffs,
  turnSummaryIsMeaningful,
} from './turnDiffSummary';
import { makeItem } from '../../test/helpers/chat';

describe('summarizeTurnDiffs', () => {
  it('returns zeros for empty turns', () => {
    expect(summarizeTurnDiffs([], 0)).toEqual(EMPTY_TURN_DIFF_SUMMARY);
  });

  it('aggregates diff payload rows and tool-result inline diffs in the same turn', () => {
    const items = [
      makeItem({
        id: 'diff-1',
        turnIndex: 0,
        payloadId: 'p1',
        payloadKind: 'diff',
        payloadMeta: JSON.stringify({
          filePath: 'a.ts',
          changeKind: 'modified',
          insertions: 3,
          deletions: 1,
          preview: '',
        }),
      }),
      makeItem({
        id: 'tool-1',
        turnIndex: 0,
        payloadId: 'p2',
        payloadKind: 'tool_result',
        payloadMeta: JSON.stringify({
          inlineDiff: {
            files: [
              { path: 'b.ts', insertions: 5, deletions: 2 },
              { path: 'c.ts', insertions: 1, deletions: 0 },
            ],
          },
        }),
      }),
      makeItem({
        id: 'other-turn',
        turnIndex: 1,
        payloadId: 'p3',
        payloadKind: 'diff',
        payloadMeta: JSON.stringify({
          filePath: 'ignored.ts',
          changeKind: 'modified',
          insertions: 99,
          deletions: 99,
          preview: '',
        }),
      }),
    ];

    expect(summarizeTurnDiffs(items, 0)).toEqual({
      insertions: 9,
      deletions: 3,
      fileCount: 3,
    });
  });

  it('dedupes file count when the same path is touched twice', () => {
    const items = [
      makeItem({
        id: 'tool-1',
        payloadId: 'p1',
        payloadKind: 'tool_result',
        payloadMeta: JSON.stringify({
          inlineDiff: {
            files: [
              { path: 'a.ts', insertions: 2, deletions: 0 },
              { path: 'a.ts', insertions: 1, deletions: 1 },
            ],
          },
        }),
      }),
    ];

    expect(summarizeTurnDiffs(items, 0)).toEqual({
      insertions: 3,
      deletions: 1,
      fileCount: 1,
    });
  });
});

describe('turnSummaryIsMeaningful', () => {
  it('requires at least one changed line', () => {
    expect(turnSummaryIsMeaningful(EMPTY_TURN_DIFF_SUMMARY)).toBe(false);
    expect(turnSummaryIsMeaningful({ insertions: 0, deletions: 0, fileCount: 1 })).toBe(false);
    expect(turnSummaryIsMeaningful({ insertions: 1, deletions: 0, fileCount: 1 })).toBe(true);
    expect(turnSummaryIsMeaningful({ insertions: 0, deletions: 1, fileCount: 1 })).toBe(true);
  });
});

describe('changedFilesForItem', () => {
  it('extracts one ChangedFile from a diff payload', () => {
    const item = makeItem({
      id: 'diff-1',
      payloadId: 'p1',
      payloadKind: 'diff',
      payloadMeta: JSON.stringify({
        filePath: 'src/a.ts',
        changeKind: 'modified',
        insertions: 4,
        deletions: 2,
        preview: '',
      }),
    });
    expect(changedFilesForItem(item)).toEqual([{
      path: 'src/a.ts',
      insertions: 4,
      deletions: 2,
      kind: 'modified',
      payloadId: 'p1',
    }]);
  });

  it('fans out tool_result inlineDiff.files into ChangedFile entries', () => {
    const item = makeItem({
      id: 'tool-1',
      payloadId: 'p2',
      payloadKind: 'tool_result',
      payloadMeta: JSON.stringify({
        inlineDiff: {
          files: [
            { path: 'x.ts', insertions: 1, deletions: 0, kind: 'added' },
            { path: 'y.ts', previousPath: 'old-y.ts', insertions: 2, deletions: 1, kind: 'renamed' },
          ],
        },
      }),
    });
    expect(changedFilesForItem(item)).toEqual([
      { path: 'x.ts', insertions: 1, deletions: 0, kind: 'added', payloadId: 'p2' },
      { path: 'y.ts', previousPath: 'old-y.ts', insertions: 2, deletions: 1, kind: 'renamed', payloadId: 'p2' },
    ]);
  });

  it('returns an empty list for non-diff items', () => {
    expect(changedFilesForItem(makeItem({ id: 'assistant:0' }))).toEqual([]);
  });

  it('silently returns an empty list on malformed meta', () => {
    const item = makeItem({
      id: 'bad',
      payloadId: 'p',
      payloadKind: 'diff',
      payloadMeta: '{not json}',
    });
    expect(changedFilesForItem(item)).toEqual([]);
  });

  it('drops malformed diff metadata instead of producing NaN rows', () => {
    expect(changedFilesForItem(makeItem({
      id: 'bad-diff',
      payloadId: 'p',
      payloadKind: 'diff',
      payloadMeta: JSON.stringify({
        filePath: '',
        changeKind: 'modified',
        insertions: Number.NaN,
        deletions: 0,
        preview: '',
      }),
    }))).toEqual([]);

    expect(changedFilesForItem(makeItem({
      id: 'bad-tool',
      payloadId: 'p2',
      payloadKind: 'tool_result',
      payloadMeta: JSON.stringify({
        inlineDiff: {
          files: [
            { path: 'valid.ts', insertions: 1, deletions: 0 },
            { path: '', insertions: 2, deletions: 1 },
            { path: 'nan.ts', insertions: 'many', deletions: 0 },
          ],
        },
      }),
    }))).toEqual([
      { path: 'valid.ts', insertions: 1, deletions: 0, kind: 'modified', payloadId: 'p2' },
    ]);
  });
});

describe('buildTurnDiffView', () => {
  it('returns null for turns with no diff activity', () => {
    const items = [
      makeItem({ id: 'user:0', turnIndex: 0, kind: 'user_text' }),
      makeItem({ id: 'text:0:0', turnIndex: 0, itemIndex: 1, kind: 'assistant_text' }),
    ];
    expect(buildTurnDiffView(items, 0)).toBeNull();
  });

  it('combines files and summary from all diff items in the turn', () => {
    const items = [
      makeItem({
        id: 'diff-1',
        turnIndex: 0,
        payloadId: 'p1',
        payloadKind: 'diff',
        payloadMeta: JSON.stringify({
          filePath: 'a.ts',
          changeKind: 'modified',
          insertions: 3,
          deletions: 1,
          preview: '',
        }),
      }),
      makeItem({
        id: 'tool-1',
        turnIndex: 0,
        itemIndex: 1,
        payloadId: 'p2',
        payloadKind: 'tool_result',
        payloadMeta: JSON.stringify({
          inlineDiff: {
            files: [
              { path: 'b.ts', insertions: 5, deletions: 2, kind: 'modified' },
              { path: 'c.ts', insertions: 1, deletions: 0, kind: 'added' },
            ],
          },
        }),
      }),
    ];

    const view = buildTurnDiffView(items, 0);
    expect(view).not.toBeNull();
    expect(view!.files.map((f) => f.path)).toEqual(['a.ts', 'b.ts', 'c.ts']);
    expect(view!.summary).toEqual({
      insertions: 9,
      deletions: 3,
      fileCount: 3,
    });
  });

  it('dedupes fileCount on repeated paths but sums every line contribution', () => {
    const items = [
      makeItem({
        id: 'tool-1',
        turnIndex: 0,
        payloadId: 'p1',
        payloadKind: 'tool_result',
        payloadMeta: JSON.stringify({
          inlineDiff: {
            files: [
              { path: 'a.ts', insertions: 2, deletions: 0 },
              { path: 'a.ts', insertions: 1, deletions: 1 },
            ],
          },
        }),
      }),
    ];
    expect(buildTurnDiffView(items, 0)?.summary).toEqual({
      insertions: 3,
      deletions: 1,
      fileCount: 1,
    });
  });

  it('ignores items belonging to other turns', () => {
    const items = [
      makeItem({
        id: 'diff-turn-0',
        turnIndex: 0,
        payloadId: 'p0',
        payloadKind: 'diff',
        payloadMeta: JSON.stringify({
          filePath: 'a.ts',
          changeKind: 'modified',
          insertions: 3,
          deletions: 1,
          preview: '',
        }),
      }),
      makeItem({
        id: 'diff-turn-1',
        turnIndex: 1,
        payloadId: 'p1',
        payloadKind: 'diff',
        payloadMeta: JSON.stringify({
          filePath: 'b.ts',
          changeKind: 'modified',
          insertions: 99,
          deletions: 99,
          preview: '',
        }),
      }),
    ];
    expect(buildTurnDiffView(items, 0)?.summary).toEqual({
      insertions: 3,
      deletions: 1,
      fileCount: 1,
    });
  });
});

describe('buildDiffViewForItems', () => {
  it('builds from pre-filtered turn items without requiring a turn scan', () => {
    const view = buildDiffViewForItems([
      makeItem({
        id: 'diff-1',
        turnIndex: 4,
        payloadId: 'p1',
        payloadKind: 'diff',
        payloadMeta: JSON.stringify({
          filePath: 'indexed.ts',
          changeKind: 'modified',
          insertions: 2,
          deletions: 1,
          preview: '',
        }),
      }),
    ]);

    expect(view?.files.map((file) => file.path)).toEqual(['indexed.ts']);
    expect(view?.summary).toEqual({ insertions: 2, deletions: 1, fileCount: 1 });
  });
});
