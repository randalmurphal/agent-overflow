import { describe, expect, it } from 'vitest';
import { conflictPatchFile } from './conflictFile';
import { parseReviewFiles, reviewFileFromPatchFile, type ReviewFile } from './patchStore';
import { forgetReviewFindSpotsForTest, locateReviewFindHit, seedReviewFindSpot, type ReviewFindLocateContent, type ReviewFindLocation } from './reviewFindRows';
import { scanReviewFind, type ReviewFindContent, type ReviewFindHit } from './reviewFindScan';
import type { DiffReviewComment, ReviewThread } from '../types/models';

// The resolver against content that changed under published matches. The
// oracle is a fresh scan of the new content: a match resolves to where
// that scan finds the same identity, else to nothing.

const diffHeader = (path: string) => [`diff --git a/${path} b/${path}`, `--- a/${path}`, `+++ b/${path}`];

const PATCHES = {
  base: [
    ...diffHeader('src/a.test.ts'), '@@ -10,3 +10,3 @@', ' ctx needle', '-del needle', '+add needle', ' tail',
    ...diffHeader('src/b.ts'), '@@ -100,2 +100,2 @@', ' needle hundred', ' end', '',
  ],
  // A gap expanded above each match: every row moves down one.
  moved: [
    ...diffHeader('src/a.test.ts'), '@@ -9,4 +9,4 @@', ' nine', ' ctx needle', '-del needle', '+add needle', ' tail',
    ...diffHeader('src/b.ts'), '@@ -99,3 +99,3 @@', ' ninety-nine', ' needle hundred', ' end', '',
  ],
  // The removed line and the context line at 100 are gone.
  removed: [
    ...diffHeader('src/a.test.ts'), '@@ -10,2 +10,3 @@', ' ctx needle', '+add needle', ' tail',
    ...diffHeader('src/b.ts'), '@@ -101,1 +101,1 @@', ' end', '',
  ],
};

const draft: DiffReviewComment = {
  id: 'd1', threadId: 't', scope: 'workspace', sourceKey: 'k', filePath: 'src/b.ts', status: 'draft',
  newLine: 100, side: 'new', selectedText: '', body: 'needle draft', createdAt: 0, updatedAt: 0,
};
const thread: ReviewThread = {
  id: 'p1', path: 'src/a.test.ts', line: 10, side: 'right', isResolvable: true, isResolved: false, isOutdated: false,
  comments: [{ authorLogin: 'a', body: 'needle one', createdAt: '', databaseID: 1 }, { authorLogin: 'b', body: 'needle two', createdAt: '', databaseID: 2 }],
};

function diffContent(patch: string[], comments = true): ReviewFindContent {
  return { files: parseReviewFiles(patch.join('\n')), drafts: comments ? [draft] : [], prThreads: comments ? [thread] : [], commentText: (body) => [body] };
}

async function scan(content: ReviewFindContent): Promise<Map<string, { hit: ReviewFindHit; at: ReviewFindLocation }>> {
  const out = new Map<string, { hit: ReviewFindHit; at: ReviewFindLocation }>();
  await scanReviewFind(content, 'needle', {
    limit: 1000, live: () => true, pause: async () => {},
    // Seeded as the pane's source seeds what its scans read.
    emit: (hit, file, spot) => {
      seedReviewFindSpot(file, hit, spot);
      out.set(hit.identity, { hit, at: { fileIndex: content.files.indexOf(file), ...spot } });
    },
  });
  return out;
}

// Every match `before` published, resolved against `after`, with what the
// scans of both recorded and again with nothing recorded.
async function expectResolves(before: ReviewFindContent, after: ReviewFindContent): Promise<Map<string, ReviewFindLocation | null>> {
  const published = await scan(before);
  const fresh = await scan(after);
  const located = new Map<string, ReviewFindLocation | null>();
  for (const seeded of [true, false]) {
    if (!seeded) for (const file of after.files) forgetReviewFindSpotsForTest(file);
    for (const { hit } of published.values()) {
      const at = locateReviewFindHit(after, hit);
      expect(at, `${hit.identity} (${seeded ? 'seeded' : 'walked'})`).toEqual(fresh.get(hit.identity)?.at ?? null);
      located.set(hit.identity, at);
    }
  }
  return located;
}

const id = (path: string, element: string, occurrence = 0) => `${path}\u0000${element}\u0000${occurrence}`;

describe('locateReviewFindHit', () => {
  it('every kind of diff match resolves where it is shown now', async () => {
    const base = diffContent(PATCHES.base);
    const located = await expectResolves(base, base);
    expect([...located.keys()]).toEqual([
      id('src/a.test.ts', 'pc:0:p1'), id('src/a.test.ts', 'pc:1:p1'), id('src/a.test.ts', 'c10:10'), id('src/a.test.ts', 'd11'), id('src/a.test.ts', 'a11'),
      id('src/b.ts', 'c100:100'), id('src/b.ts', 'dc:d1'),
    ]);
    expect(located.get(id('src/b.ts', 'c100:100'))).toMatchObject({ fileIndex: 1, rowKey: 'l:4:src/b.ts', place: { kind: 'line', lineIndex: 4 } });
    expect(located.get(id('src/b.ts', 'dc:d1'))).toMatchObject({ fileIndex: 1, rowKey: 'dc:d1', line: 4, place: { kind: 'draft', commentId: 'd1' } });
  });

  it('a match whose row moved resolves to its new row', async () => {
    const located = await expectResolves(diffContent(PATCHES.base), diffContent(PATCHES.moved));
    expect(located.get(id('src/b.ts', 'c100:100'))).toMatchObject({ fileIndex: 1, rowKey: 'l:5:src/b.ts', place: { kind: 'line', lineIndex: 5 } });
    expect(located.get(id('src/a.test.ts', 'a11'))).toMatchObject({ rowKey: 'l:7:src/a.test.ts' });
    expect(located.get(id('src/b.ts', 'dc:d1'))).toMatchObject({ line: 5 });
  });

  it('a match whose line or comment is gone is not present', async () => {
    const located = await expectResolves(diffContent(PATCHES.base), diffContent(PATCHES.removed, false));
    expect(located.get(id('src/b.ts', 'c100:100'))).toBeNull();
    expect(located.get(id('src/a.test.ts', 'd11'))).toBeNull();
    expect(located.get(id('src/b.ts', 'dc:d1'))).toBeNull();
    expect(located.get(id('src/a.test.ts', 'pc:1:p1'))).toBeNull();
    expect(located.get(id('src/a.test.ts', 'c10:10'))).not.toBeNull();
  });

  it('a comment deleted while its file stays the same is not present', async () => {
    const base = diffContent(PATCHES.base);
    const located = await expectResolves(base, { ...base, drafts: [], prThreads: [{ ...thread, comments: thread.comments.slice(0, 1) }] });
    expect(located.get(id('src/b.ts', 'dc:d1'))).toBeNull();
    expect(located.get(id('src/a.test.ts', 'pc:1:p1'))).toBeNull();
    expect(located.get(id('src/a.test.ts', 'pc:0:p1'))).not.toBeNull();
  });

  it('a file hidden by a filter takes its matches with it; the others resolve at their new index', async () => {
    const base = diffContent(PATCHES.base);
    const hidden = { ...base, files: base.files.filter((file) => !file.path.endsWith('.test.ts')) };
    const located = await expectResolves(base, hidden);
    expect(located.get(id('src/a.test.ts', 'c10:10'))).toBeNull();
    expect(located.get(id('src/b.ts', 'c100:100'))).toMatchObject({ fileIndex: 0, rowKey: 'l:4:src/b.ts' });
  });

  it('reordered files resolve by path', async () => {
    const base = diffContent(PATCHES.base);
    const located = await expectResolves(base, { ...base, files: [...base.files].reverse() });
    expect(located.get(id('src/b.ts', 'c100:100'))).toMatchObject({ fileIndex: 0 });
    expect(located.get(id('src/a.test.ts', 'pc:0:p1'))).toMatchObject({ fileIndex: 1, rowKey: 'pc:0:p1' });
  });

  it('a conflict fold\'s hidden match resolves to the fold, and to its line once the fold opens; markers move with it', async () => {
    const lead = Array.from({ length: 40 }, (_, i) => (i === 20 ? 'needle in the fold' : `lead ${i}`));
    // Their side is empty: the last marker is the second of a run.
    const source = [...lead, '<<<<<<< ours', 'needle ours', '=======', '>>>>>>> theirs needle'].join('\n');
    const build = (expanded: Set<number> | undefined): ReviewFindContent => {
      const folds = new Map<number, readonly string[]>();
      const file = reviewFileFromPatchFile(conflictPatchFile('c.go', source, { expandedFolds: expanded, onFold: (foldId, lines) => folds.set(foldId, lines) }));
      return { files: [file], drafts: [], prThreads: [], commentText: (body) => [body], foldLines: () => folds };
    };
    const closed = build(undefined);
    const atClosed = await expectResolves(closed, closed);
    const fold = atClosed.get(id('c.go', 'c21:21'));
    expect(fold).toMatchObject({ place: { kind: 'fold' } });
    const foldId = (fold!.place as { foldId: number }).foldId;
    const atOpen = await expectResolves(closed, build(new Set([foldId])));
    expect(atOpen.get(id('c.go', 'c21:21'))).toMatchObject({ place: { kind: 'line' } });
    expect([...atOpen.values()].every((at) => at !== null)).toBe(true);
  });

  it('a content listing no file of the match is not present', () => {
    const hit: ReviewFindHit = { identity: id('gone.ts', 'h'), path: 'gone.ts', at: { kind: 'header' }, occurrence: 0 };
    const content: ReviewFindLocateContent = { files: [] as ReviewFile[], drafts: [], prThreads: [] };
    expect(locateReviewFindHit(content, hit)).toBeNull();
  });
});
