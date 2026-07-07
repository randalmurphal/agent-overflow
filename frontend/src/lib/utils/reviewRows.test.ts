import { describe, expect, it } from 'vitest';
import type { DiffReviewComment, ReviewThread } from '../types/models';
import { parsePatchFiles } from './patchFiles';
import {
  REVIEW_FILE_HEADER_PX,
  REVIEW_LINE_BLOCK_MAX_LINES,
  REVIEW_LINE_HEIGHT_PX,
  buildReviewRows,
  reviewRowEstimate,
  type CommentAnchor,
  type ReviewRow,
} from './reviewRows';

function addedPatch(path: string, count: number): string {
  const lines = Array.from({ length: count }, (_, index) => `+line ${index + 1}`);
  return [
    `diff --git a/${path} b/${path}`,
    'new file mode 100644',
    'index 0000000..1111111',
    '--- /dev/null',
    `+++ b/${path}`,
    `@@ -0,0 +1,${count} @@`,
    ...lines,
  ].join('\n');
}

function twoFilePatch(): string {
  return [
    addedPatch('src/one.ts', 2),
    addedPatch('src/two.ts', 3),
  ].join('\n');
}

function draft(overrides: Partial<DiffReviewComment>): DiffReviewComment {
  return {
    id: overrides.id ?? 'comment-1',
    threadId: 'thread-1',
    scope: 'workspace',
    sourceKey: 'workspace',
    filePath: overrides.filePath ?? 'src/file.ts',
    status: overrides.status ?? 'draft',
    oldLine: overrides.oldLine,
    newLine: overrides.newLine,
    side: overrides.side ?? 'new',
    selectedText: overrides.selectedText ?? '',
    body: overrides.body ?? 'comment',
    createdAt: overrides.createdAt ?? 1,
    updatedAt: overrides.updatedAt ?? 1,
  };
}

function lineBlocks(rows: ReviewRow[]): Extract<ReviewRow, { kind: 'line-block' }>[] {
  return rows.filter((row): row is Extract<ReviewRow, { kind: 'line-block' }> => row.kind === 'line-block');
}

function prThread(overrides: Partial<ReviewThread> = {}): ReviewThread {
  return {
    id: overrides.id ?? 'prt-1',
    path: overrides.path ?? 'src/file.ts',
    line: overrides.line ?? 2,
    startLine: overrides.startLine,
    side: overrides.side ?? 'right',
    isResolvable: overrides.isResolvable ?? true,
    isResolved: overrides.isResolved ?? false,
    isOutdated: overrides.isOutdated ?? false,
    comments: overrides.comments ?? [{ authorLogin: 'alice', body: 'Please fix', createdAt: 'now', databaseID: 1 }],
  };
}

describe('buildReviewRows', () => {
  it('splits line blocks at REVIEW_LINE_BLOCK_MAX_LINES', () => {
    const files = parsePatchFiles(addedPatch('src/file.ts', REVIEW_LINE_BLOCK_MAX_LINES + 1));
    const result = buildReviewRows({
      files,
      viewMode: 'stacked',
      collapsedPaths: new Set(),
      drafts: [],
      openEditors: [],
    });

    const blocks = lineBlocks(result.rows);
    expect(blocks.map((block) => block.rows.length)).toEqual([32, 1]);
    expect(result.rowKeys[0]).toBe('h:src/file.ts');
    expect(result.rowKeys.slice(1).every((key) => key.startsWith('b:src/file.ts:'))).toBe(true);
    expect(new Set(result.rowKeys).size).toBe(result.rowKeys.length);
  });

  it('ends a line block at anchors and inserts comment/editor rows immediately after it', () => {
    const files = parsePatchFiles(addedPatch('src/file.ts', 12));
    const editor: CommentAnchor = { filePath: 'src/file.ts', side: 'new', newLine: 10 };
    const result = buildReviewRows({
      files,
      viewMode: 'stacked',
      collapsedPaths: new Set(),
      drafts: [draft({ id: 'draft-7', newLine: 10 })],
      openEditors: [editor],
    });

    expect(result.rows.map((row) => row.kind)).toEqual([
      'file-header',
      'line-block',
      'comment-thread',
      'draft-editor',
      'line-block',
    ]);
    expect(lineBlocks(result.rows).map((block) => [block.startLine, block.rows.length])).toEqual([
      [1, 10],
      [11, 2],
    ]);
    expect(result.rowKeys[2]).toBe('t:draft-7');
    expect(result.rowKeys[3]).toBe('d:src/file.ts:new:0:10');
    expect(new Set(result.rowKeys).size).toBe(result.rowKeys.length);
  });

  it('keeps later fixed chunk keys stable when an earlier block splits', () => {
    const files = parsePatchFiles(addedPatch('src/file.ts', 70));
    const plain = buildReviewRows({
      files,
      viewMode: 'stacked',
      collapsedPaths: new Set(),
      drafts: [],
      openEditors: [],
    });
    const anchored = buildReviewRows({
      files,
      viewMode: 'stacked',
      collapsedPaths: new Set(),
      drafts: [draft({ id: 'draft-early', newLine: 10 })],
      openEditors: [],
    });

    // Fixed 32-row chunk boundaries survive an earlier anchor split: every
    // key present before the split (except the split block's own) must
    // survive it, so the virtualizer's keyed remap moves sizes instead of
    // remeasuring everything below the split.
    const plainBlockKeys = plain.rowKeys.filter((key) => key.startsWith('b:'));
    const anchoredKeys = new Set(anchored.rowKeys);
    const survivors = plainBlockKeys.filter((key) => anchoredKeys.has(key));
    expect(survivors.length).toBe(plainBlockKeys.length - 1);
  });

  it('never emits colliding block keys when old and new line numbers overlap', () => {
    // One hunk deletes lines 1-2, the next adds different content at the
    // same numeric lines — line-number-based keys would collide across
    // sides. A draft forces block splits right at the overlap.
    const patch = [
      'diff --git a/src/file.ts b/src/file.ts',
      'index 1111111..2222222 100644',
      '--- a/src/file.ts',
      '+++ b/src/file.ts',
      '@@ -1,2 +1,2 @@',
      '-old line 1',
      '-old line 2',
      '+new line 1',
      '+new line 2',
    ].join('\n');
    const result = buildReviewRows({
      files: parsePatchFiles(patch),
      viewMode: 'stacked',
      collapsedPaths: new Set(),
      drafts: [draft({ id: 'split-1', side: 'old', oldLine: 1, newLine: undefined })],
      openEditors: [],
    });

    expect(new Set(result.rowKeys).size).toBe(result.rowKeys.length);
  });

  it('attaches an insert exactly once when old and new sides share a line number', () => {
    const patch = [
      'diff --git a/src/file.ts b/src/file.ts',
      'index 1111111..2222222 100644',
      '--- a/src/file.ts',
      '+++ b/src/file.ts',
      '@@ -1,1 +1,1 @@',
      '-old line 1',
      '+new line 1',
    ].join('\n');
    // The deleted row reports line 1 (oldLine) and the added row reports
    // line 1 (newLine); the draft must land after the first and not repeat.
    const result = buildReviewRows({
      files: parsePatchFiles(patch),
      viewMode: 'stacked',
      collapsedPaths: new Set(),
      drafts: [draft({ id: 'once-1', newLine: 1 })],
      openEditors: [],
    });

    expect(result.rowKeys.filter((key) => key === 't:once-1')).toHaveLength(1);
    expect(new Set(result.rowKeys).size).toBe(result.rowKeys.length);
  });

  it('renders an orphaned draft at the end of its file instead of dropping it', () => {
    const files = parsePatchFiles(addedPatch('src/file.ts', 3));
    const result = buildReviewRows({
      files,
      viewMode: 'stacked',
      collapsedPaths: new Set(),
      drafts: [draft({ id: 'orphan-1', newLine: 999 })],
      openEditors: [],
    });

    expect(result.rowKeys).toContain('t:orphan-1');
    expect(result.rowKeys.indexOf('t:orphan-1')).toBe(result.rowKeys.length - 1);
  });

  it('places anchored PR threads after the line block containing the anchor', () => {
    const files = parsePatchFiles(addedPatch('src/file.ts', 4));
    const result = buildReviewRows({
      files,
      viewMode: 'stacked',
      collapsedPaths: new Set(),
      drafts: [],
      openEditors: [],
      prThreads: [prThread({ id: 'thread-a', line: 2 })],
      expandedPRThreadIds: new Set(),
    });

    expect(result.rows.map((row) => row.kind)).toEqual(['file-header', 'line-block', 'pr-thread', 'line-block']);
    const row = result.rows[2];
    expect(row.kind).toBe('pr-thread');
    if (row.kind === 'pr-thread') expect(row.orphaned).toBe(false);
  });

  it('groups outdated and unanchored PR threads under the file header', () => {
    const files = parsePatchFiles(addedPatch('src/file.ts', 2));
    const result = buildReviewRows({
      files,
      viewMode: 'stacked',
      collapsedPaths: new Set(),
      drafts: [],
      openEditors: [],
      prThreads: [
        prThread({ id: 'outdated', line: 1, isOutdated: true }),
        prThread({ id: 'missing', line: 99 }),
      ],
      expandedPRThreadIds: new Set(),
    });

    expect(result.rows.map((row) => row.kind)).toEqual(['file-header', 'pr-thread', 'pr-thread', 'line-block']);
    const prRows = result.rows.filter((row): row is Extract<ReviewRow, { kind: 'pr-thread' }> => row.kind === 'pr-thread');
    expect(prRows.map((row) => row.orphaned)).toEqual([true, true]);
  });

  it('collapses resolved PR threads by default and expands from store state', () => {
    const files = parsePatchFiles(addedPatch('src/file.ts', 2));
    const collapsed = buildReviewRows({
      files,
      viewMode: 'stacked',
      collapsedPaths: new Set(),
      drafts: [],
      openEditors: [],
      prThreads: [prThread({ id: 'resolved', isResolved: true })],
      expandedPRThreadIds: new Set(),
    });
    const expanded = buildReviewRows({
      files,
      viewMode: 'stacked',
      collapsedPaths: new Set(),
      drafts: [],
      openEditors: [],
      prThreads: [prThread({ id: 'resolved', isResolved: true })],
      expandedPRThreadIds: new Set(['resolved']),
    });

    expect((collapsed.rows.find((row) => row.kind === 'pr-thread') as Extract<ReviewRow, { kind: 'pr-thread' }>).collapsed).toBe(true);
    expect((expanded.rows.find((row) => row.kind === 'pr-thread') as Extract<ReviewRow, { kind: 'pr-thread' }>).collapsed).toBe(false);
  });

  it('uses stable file keys across collapse and expand', () => {
    const files = parsePatchFiles(addedPatch('src/file.ts', 3));
    const expanded = buildReviewRows({
      files,
      viewMode: 'stacked',
      collapsedPaths: new Set(),
      drafts: [],
      openEditors: [],
    });
    const collapsed = buildReviewRows({
      files,
      viewMode: 'stacked',
      collapsedPaths: new Set(['src/file.ts']),
      drafts: [],
      openEditors: [],
    });
    const expandedAgain = buildReviewRows({
      files,
      viewMode: 'stacked',
      collapsedPaths: new Set(),
      drafts: [],
      openEditors: [],
    });

    expect(expanded.rowKeys[0]).toBe('h:src/file.ts');
    expect(collapsed.rowKeys).toEqual(['h:src/file.ts']);
    expect(expandedAgain.rowKeys).toEqual(expanded.rowKeys);
  });

  it('populates fileOfRow and firstRowOfFile for expanded and collapsed files', () => {
    const files = parsePatchFiles(twoFilePatch());
    const result = buildReviewRows({
      files,
      viewMode: 'stacked',
      collapsedPaths: new Set(['src/two.ts']),
      drafts: [],
      openEditors: [],
    });

    expect(result.rows.map((row) => row.kind)).toEqual([
      'file-header',
      'line-block',
      'file-header',
    ]);
    expect(result.fileOfRow).toEqual([0, 0, 1]);
    expect(result.firstRowOfFile).toEqual([0, 2]);
  });

  it('builds splitRows for split mode line blocks', () => {
    const files = parsePatchFiles(addedPatch('src/file.ts', 2));
    const result = buildReviewRows({
      files,
      viewMode: 'split',
      collapsedPaths: new Set(),
      drafts: [],
      openEditors: [],
    });

    expect(lineBlocks(result.rows)[0]?.splitRows).toHaveLength(2);
  });
});

describe('reviewRowEstimate', () => {
  it('reports exact fixed rows when word wrap is disabled and inexact rows when enabled', () => {
    const files = parsePatchFiles(addedPatch('src/file.ts', 2));
    const result = buildReviewRows({
      files,
      viewMode: 'stacked',
      collapsedPaths: new Set(),
      drafts: [draft({ id: 'draft-1', newLine: 1 })],
      openEditors: [{ filePath: 'src/file.ts', side: 'new', newLine: 2 }],
    });

    const plain = reviewRowEstimate(result, false);
    const wrapped = reviewRowEstimate(result, true);
    expect(result.rows.map((row) => row.kind)).toEqual([
      'file-header',
      'line-block',
      'comment-thread',
      'line-block',
      'draft-editor',
    ]);
    expect(plain.at(0)).toBe(REVIEW_FILE_HEADER_PX);
    expect(plain.at(1)).toBe(REVIEW_LINE_HEIGHT_PX);
    expect(plain.at(2)).toBe(120);
    expect(plain.isExact?.(0)).toBe(true);
    expect(plain.isExact?.(1)).toBe(true);
    expect(plain.isExact?.(2)).toBe(false);
    expect(wrapped.at(1)).toBe(REVIEW_LINE_HEIGHT_PX);
    expect(wrapped.isExact?.(0)).toBe(false);
    expect(wrapped.isExact?.(1)).toBe(false);
  });

  it('renders a collapsed file as its header row alone', () => {
    const files = parsePatchFiles(addedPatch('src/file.ts', 2));
    const result = buildReviewRows({
      files,
      viewMode: 'stacked',
      collapsedPaths: new Set(['src/file.ts']),
      drafts: [],
      openEditors: [],
    });

    const estimate = reviewRowEstimate(result, false);
    expect(result.rows.map((row) => row.kind)).toEqual(['file-header']);
    expect(estimate.at(0)).toBe(REVIEW_FILE_HEADER_PX);
    expect(estimate.isExact?.(0)).toBe(true);
  });
});
