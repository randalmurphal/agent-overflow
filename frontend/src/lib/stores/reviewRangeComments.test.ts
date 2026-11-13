import { describe, expect, it } from 'vitest';
import { flushSync } from 'svelte';
import { createReviewPRLineIndex } from './reviewPRLineIndex.svelte';
import {
  lineCommentTarget,
  rangePROrphans,
  rangeSaveAnchor,
  rangeShownDrafts,
  RANGE_LINE_NOT_IN_PR,
  RANGE_LINE_PR_LOADING,
  RANGE_OLD_LINE,
  lineCommentReason,
} from './reviewRangeComments';
import { buildPRLineIndex, resolvePRNewLine } from '../utils/prLineIndex';
import { parseReviewFiles } from '../utils/patchStore';
import type { DiffReviewComment } from '../types/models';

// The whole PR's diff of src/app.ts: hunk one is new 1-4 (1 unchanged,
// 2-3 added, 4 unchanged) with old 2 removed; hunk two is new 20-22.
const WHOLE = [
  'diff --git a/src/app.ts b/src/app.ts',
  'index 1111111..2222222 100644',
  '--- a/src/app.ts',
  '+++ b/src/app.ts',
  '@@ -1,3 +1,4 @@',
  ' one',
  '-two',
  '+TWO',
  '+TWO b',
  ' three',
  '@@ -18,2 +20,3 @@',
  ' x',
  '+y',
  ' z',
  '',
].join('\n');

function readyIndex() {
  const index = createReviewPRLineIndex();
  index.seed('k', parseReviewFiles(WHOLE));
  return index;
}

function draft(over: Partial<DiffReviewComment>): DiffReviewComment {
  return {
    id: 'd', threadId: 't', scope: 'pr', sourceKey: 'pr', filePath: 'src/app.ts', status: 'draft', side: 'new',
    selectedText: '', body: 'b', createdAt: 1, updatedAt: 1, ...over,
  };
}

describe('the whole PR diff index', () => {
  it('resolves a new line inside a hunk to its whole-diff numbers and nothing outside one', () => {
    const index = buildPRLineIndex(parseReviewFiles(WHOLE));
    expect(resolvePRNewLine(index, 'src/app.ts', 1)).toEqual({ newLine: 1, oldLine: 1 });
    expect(resolvePRNewLine(index, 'src/app.ts', 2)).toEqual({ newLine: 2 });
    expect(resolvePRNewLine(index, 'src/app.ts', 4)).toEqual({ newLine: 4, oldLine: 3 });
    expect(resolvePRNewLine(index, 'src/app.ts', 5)).toBeNull();
    expect(resolvePRNewLine(index, 'src/app.ts', 19)).toBeNull();
    expect(resolvePRNewLine(index, 'src/app.ts', 21)).toEqual({ newLine: 21 });
    expect(resolvePRNewLine(index, 'src/app.ts', 22)).toEqual({ newLine: 22, oldLine: 19 });
    expect(resolvePRNewLine(index, 'src/app.ts', 23)).toBeNull();
    expect(resolvePRNewLine(index, 'other.ts', 1)).toBeNull();
  });
});

describe('comments in a commit run ending at the head', () => {
  it('a new-side line inside a whole-PR hunk takes a PR comment; outside one, an agent-only draft (or none pinned)', () => {
    const index = readyIndex();
    expect(lineCommentTarget(index, { filePath: 'src/app.ts', side: 'new', newLine: 3 }, true)).toBe('pr');
    expect(lineCommentTarget(index, { filePath: 'src/app.ts', side: 'context', newLine: 21 }, true)).toBe('pr');
    expect(lineCommentTarget(index, { filePath: 'src/app.ts', side: 'new', newLine: 10 }, true)).toBe('agent');
    expect(lineCommentReason(index, 'agent', { newLine: 10 })).toBe(RANGE_LINE_NOT_IN_PR);
    expect(lineCommentTarget(index, { filePath: 'src/app.ts', side: 'new', newLine: 10 }, false)).toBe('none');
  });

  it('an old-side line takes no comment: its number names the run\'s base', () => {
    const index = readyIndex();
    expect(lineCommentTarget(index, { filePath: 'src/app.ts', side: 'old' }, true)).toBe('none');
    expect(lineCommentReason(index, 'none', {})).toBe(RANGE_OLD_LINE);
    // The old half of a split unchanged row is the unchanged line.
    expect(lineCommentTarget(index, { filePath: 'src/app.ts', side: 'old', newLine: 1 }, true)).toBe('pr');
  });

  it('while the whole diff is loading or failed, no line takes a PR comment', () => {
    const index = createReviewPRLineIndex();
    let reject!: (err: Error) => void;
    index.open('k', () => new Promise((_, no) => { reject = no; }));
    expect(index.state).toBe('loading');
    expect(lineCommentTarget(index, { filePath: 'src/app.ts', side: 'new', newLine: 3 }, true)).toBe('agent');
    expect(lineCommentReason(index, 'agent', { newLine: 3 })).toBe(RANGE_LINE_PR_LOADING);
    expect(rangeSaveAnchor(index, { filePath: 'src/app.ts', side: 'new', newLine: 3 }, false)).toBeNull();
    reject(new Error('boom'));
    return Promise.resolve().then(() => {
      flushSync();
      expect(index.state).toBe('failed');
      expect(lineCommentTarget(index, { filePath: 'src/app.ts', side: 'new', newLine: 3 }, true)).toBe('agent');
    });
  });

  it('saves a PR line in the whole diff\'s numbers and an agent line by its head line alone', () => {
    const index = readyIndex();
    expect(rangeSaveAnchor(index, { filePath: 'src/app.ts', side: 'context', oldLine: 7, newLine: 4, selectedText: 'three' }, true)).toEqual({
      target: 'pr', anchor: { filePath: 'src/app.ts', side: 'new', newLine: 4, oldLine: 3, selectedText: 'three' },
    });
    expect(rangeSaveAnchor(index, { filePath: 'src/app.ts', side: 'context', oldLine: 2, newLine: 3 }, true)?.anchor).toEqual({
      filePath: 'src/app.ts', side: 'new', newLine: 3, oldLine: undefined, selectedText: undefined,
    });
    expect(rangeSaveAnchor(index, { filePath: 'src/app.ts', side: 'context', oldLine: 8, newLine: 10 }, true)).toEqual({
      target: 'agent', anchor: { filePath: 'src/app.ts', side: 'new', newLine: 10, oldLine: undefined, selectedText: undefined },
    });
  });

  it('places the PR\'s drafts by head line, drops removed-line and file drafts from the run, keeps replies', () => {
    const shown = rangeShownDrafts([
      draft({ id: 'ctx', side: 'new', newLine: 4, oldLine: 3 }),
      draft({ id: 'del', side: 'old', oldLine: 2 }),
      draft({ id: 'file', side: 'file' }),
      draft({ id: 'reply', side: 'reply', replyThreadId: 'x' }),
    ]);
    expect(shown.map((each) => [each.id, each.side, each.newLine, each.oldLine])).toEqual([
      ['ctx', 'new', 4, undefined],
      ['reply', 'reply', undefined, undefined],
    ]);
  });

  it('leaves agent-only drafts out of a PR review sent from the run, and every draft while the index is not ready', () => {
    const index = readyIndex();
    const drafts = [
      draft({ id: 'pr', newLine: 4, oldLine: 3 }),
      draft({ id: 'agent', newLine: 10 }),
      draft({ id: 'del', side: 'old', oldLine: 2 }),
      draft({ id: 'reply', side: 'reply', replyThreadId: 'x' }),
    ];
    expect([...rangePROrphans(index, drafts)]).toEqual(['agent']);
    expect([...rangePROrphans(createReviewPRLineIndex(), drafts)]).toEqual(['pr', 'agent', 'del']);
  });

  it('drops a read answering for a key no longer open', async () => {
    const index = createReviewPRLineIndex();
    let resolveFirst!: (value: ReturnType<typeof buildPRLineIndex>) => void;
    index.open('one', () => new Promise((resolve) => { resolveFirst = resolve; }));
    index.open('two', () => new Promise(() => {}));
    resolveFirst(buildPRLineIndex(parseReviewFiles(WHOLE)));
    await Promise.resolve();
    expect(index.key).toBe('two');
    expect(index.state).toBe('loading');
    expect(index.resolveNewLine('src/app.ts', 3)).toBeNull();
    index.close();
    expect(index.state).toBe('closed');
  });
});
