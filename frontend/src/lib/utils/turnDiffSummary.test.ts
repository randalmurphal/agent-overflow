import { describe, expect, it } from 'vitest';
import {
  EMPTY_TURN_DIFF_SUMMARY,
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
