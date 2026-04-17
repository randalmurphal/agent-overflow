import { describe, expect, it } from 'vitest';
import {
  EMPTY_TURN_DIFF_SUMMARY,
  summarizeTurnDiffs,
  turnSummaryIsMeaningful,
} from './turnDiffSummary';
import type { DiffMeta, Item } from '../types/models';

function item(overrides: Partial<Item>): Item {
  return {
    id: overrides.id ?? 'item',
    threadId: 'thread-1',
    turnIndex: overrides.turnIndex ?? 0,
    itemIndex: overrides.itemIndex ?? 0,
    kind: overrides.kind ?? 'message',
    role: overrides.role ?? 'assistant',
    summary: overrides.summary ?? '',
    payloadId: overrides.payloadId,
    parentToolUseId: overrides.parentToolUseId,
    createdAt: 0,
  };
}

function diff(overrides: Partial<DiffMeta> = {}): DiffMeta {
  return {
    filePath: overrides.filePath ?? 'a.ts',
    changeKind: overrides.changeKind ?? 'modified',
    insertions: overrides.insertions ?? 0,
    deletions: overrides.deletions ?? 0,
    preview: overrides.preview ?? '',
  };
}

function metaLookup(map: Record<string, DiffMeta>) {
  return (id: string) => map[id] ?? null;
}

describe('summarizeTurnDiffs()', () => {
  it('returns zero counts for an empty item list', () => {
    expect(summarizeTurnDiffs([], 0, () => null)).toEqual(EMPTY_TURN_DIFF_SUMMARY);
  });

  it('sums insertions/deletions and counts distinct files in one turn', () => {
    const items: Item[] = [
      item({ id: '1', kind: 'diff', payloadId: 'p1', itemIndex: 0 }),
      item({ id: '2', kind: 'diff', payloadId: 'p2', itemIndex: 1 }),
    ];
    const metas = metaLookup({
      p1: diff({ filePath: 'a.ts', insertions: 10, deletions: 2 }),
      p2: diff({ filePath: 'b.ts', insertions: 5, deletions: 1 }),
    });
    const result = summarizeTurnDiffs(items, 0, metas);
    expect(result).toEqual({ insertions: 15, deletions: 3, fileCount: 2 });
  });

  it('counts a single-file turn once', () => {
    const items: Item[] = [item({ id: '1', kind: 'diff', payloadId: 'p1' })];
    const metas = metaLookup({
      p1: diff({ filePath: 'solo.ts', insertions: 1, deletions: 0 }),
    });
    expect(summarizeTurnDiffs(items, 0, metas)).toEqual({
      insertions: 1,
      deletions: 0,
      fileCount: 1,
    });
  });

  it('ignores items from other turns', () => {
    const items: Item[] = [
      item({ id: '1', kind: 'diff', payloadId: 'p1', turnIndex: 0 }),
      item({ id: '2', kind: 'diff', payloadId: 'p2', turnIndex: 1 }),
    ];
    const metas = metaLookup({
      p1: diff({ filePath: 'a.ts', insertions: 10, deletions: 2 }),
      p2: diff({ filePath: 'b.ts', insertions: 99, deletions: 99 }),
    });
    const result = summarizeTurnDiffs(items, 0, metas);
    expect(result).toEqual({ insertions: 10, deletions: 2, fileCount: 1 });
  });

  it('skips non-diff items in the same turn', () => {
    const items: Item[] = [
      item({ id: '1', kind: 'message' }),
      item({ id: '2', kind: 'diff', payloadId: 'p1' }),
      item({ id: '3', kind: 'tool_result', payloadId: 'p2' }),
    ];
    const metas = metaLookup({
      p1: diff({ filePath: 'a.ts', insertions: 3, deletions: 4 }),
      p2: diff({ filePath: 'wrong.ts', insertions: 99, deletions: 99 }),
    });
    expect(summarizeTurnDiffs(items, 0, metas)).toEqual({
      insertions: 3,
      deletions: 4,
      fileCount: 1,
    });
  });

  it('skips diff items with missing payload meta without throwing', () => {
    const items: Item[] = [
      item({ id: '1', kind: 'diff', payloadId: 'present' }),
      item({ id: '2', kind: 'diff', payloadId: 'missing' }),
    ];
    const metas = metaLookup({
      present: diff({ filePath: 'present.ts', insertions: 2, deletions: 1 }),
    });
    expect(summarizeTurnDiffs(items, 0, metas)).toEqual({
      insertions: 2,
      deletions: 1,
      fileCount: 1,
    });
  });

  it('dedupes files when the same path is touched twice in one turn', () => {
    const items: Item[] = [
      item({ id: '1', kind: 'diff', payloadId: 'p1', itemIndex: 0 }),
      item({ id: '2', kind: 'diff', payloadId: 'p2', itemIndex: 1 }),
    ];
    const metas = metaLookup({
      p1: diff({ filePath: 'same.ts', insertions: 4, deletions: 1 }),
      p2: diff({ filePath: 'same.ts', insertions: 1, deletions: 0 }),
    });
    const result = summarizeTurnDiffs(items, 0, metas);
    // File count is 1 but insertions/deletions sum both edits.
    expect(result).toEqual({ insertions: 5, deletions: 1, fileCount: 1 });
  });

  it('returns all zeros when every diff meta is zero', () => {
    const items: Item[] = [
      item({ id: '1', kind: 'diff', payloadId: 'p1' }),
    ];
    const metas = metaLookup({
      p1: diff({ filePath: 'a.ts', insertions: 0, deletions: 0 }),
    });
    expect(summarizeTurnDiffs(items, 0, metas)).toEqual({
      insertions: 0,
      deletions: 0,
      fileCount: 1,
    });
  });
});

describe('turnSummaryIsMeaningful()', () => {
  it('rejects an empty summary', () => {
    expect(turnSummaryIsMeaningful(EMPTY_TURN_DIFF_SUMMARY)).toBe(false);
  });

  it('rejects summaries with files but no line changes', () => {
    expect(turnSummaryIsMeaningful({ insertions: 0, deletions: 0, fileCount: 3 })).toBe(false);
  });

  it('accepts any non-zero insertions', () => {
    expect(turnSummaryIsMeaningful({ insertions: 1, deletions: 0, fileCount: 1 })).toBe(true);
  });

  it('accepts any non-zero deletions', () => {
    expect(turnSummaryIsMeaningful({ insertions: 0, deletions: 1, fileCount: 1 })).toBe(true);
  });
});
