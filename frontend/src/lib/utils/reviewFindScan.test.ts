import { afterEach, describe, expect, it, vi } from 'vitest';
import { conflictPatchFile } from './conflictFile';
import { applyContextExpansion } from './diffContextExpansion';
import { setPatchTextBudgetForTest } from './patchMemory.svelte';
import { PatchBody, PatchParser, PatchStore, parseReviewFiles, reviewFileFromPatchFile, type ReviewFile } from './patchStore';
import { unpackText } from './packedText';
import { createMarkdownFindTextCache } from '../components/chat/markdown/markdownFindText';
import { streamPatch, textKey, wideMarks, type PatchForm } from '../../test/helpers/streamedPatch';
import {
  findDraftKey, findHeaderKey, findLineKey, findMarkerLines, findMatchChunk, findRowTag, findThreadCommentKey, LONG_TEXT,
  makeReviewFindHit, reviewCommentKey, scanReviewFind,
  type ReviewFindAt, type ReviewFindContent, type ReviewFindFileSpot, type ReviewFindHit, type ReviewFindPlace, type ReviewFindScanOptions,
} from './reviewFindScan';
import { forgetReviewFindSpotsForTest, placeReviewFindHit, resolveReviewFindHit } from './reviewFindRows';
import { FIND_FILTER_CODE_POINTS, findFilterPattern, findNeedle, foldFindText } from './findMatch';
import { shownOf } from '../../test/helpers/reviewFindOracle';
import { RowWalker } from './patchRows';
import { reviewInsertsByFile, type InsertRow } from './reviewRows';
import { findableBody } from './suggestionMarks';
import type { FindRun } from './findHighlights';
import type { DiffReviewComment, ReviewThread } from '../types/models';

const PATCH = [
  'diff --git a/src/needle.ts b/src/needle.ts',
  'index 1..2 100644',
  '--- a/src/needle.ts',
  '+++ b/src/needle.ts',
  '@@ -1,3 +1,3 @@',
  ' const keep = 1; // needle',
  '-const old = "needle";',
  '+const fresh = "Needle needle";',
  ' end',
  'diff --git a/b.ts b/b.ts',
  'index 1..2 100644',
  '--- a/b.ts',
  '+++ b/b.ts',
  '@@ -10,2 +10,2 @@ function needleInHunkHeader()',
  ' x',
  '+y needle',
  '',
].join('\n');

function files(patch = PATCH): ReviewFile[] {
  return parseReviewFiles(patch);
}

function content(over: Partial<ReviewFindContent> = {}): ReviewFindContent {
  return { files: files(), drafts: [], prThreads: [], commentText: (body) => [{ text: body, shown: true }], ...over };
}

type Scanned = ReviewFindHit & ReviewFindFileSpot & { fileIndex: number };

// The scan's hits with where it read each, which the resolver, asked
// with nothing recorded, must find the same: placed at the same line, and
// present at the same element and painter's index (its index among the
// element's matches in scan order).
async function scan(source: ReviewFindContent, query: string, limit = 1000): Promise<{ hits: Scanned[]; capped: boolean; pauses: number }> {
  const hits: Scanned[] = [];
  let pauses = 0;
  const capped = await scanReviewFind(source, query, {
    limit,
    live: () => true,
    emit: (hit, file, spot) => hits.push({ ...hit, ...spot, fileIndex: source.files.indexOf(file) }),
    pause: async () => { pauses += 1; },
  });
  const perRow = new Map<string, number>();
  for (const hit of hits) {
    forgetReviewFindSpotsForTest(source.files[hit.fileIndex]);
    expect(placeReviewFindHit(source, hit), hit.identity).toEqual({ fileIndex: hit.fileIndex, line: hit.line });
    const ordinal = perRow.get(hit.rowKey) ?? 0;
    perRow.set(hit.rowKey, ordinal + 1);
    let resolved = resolveReviewFindHit(shownOf(source), hit, findNeedle(query));
    // Text the patch budget evicted is judged once it is read back.
    if (resolved.kind === 'evicted') {
      const { file, lineIndex } = resolved;
      resolved = await file.body.whenResident(() => resolveReviewFindHit(shownOf(source), hit, findNeedle(query)), lineIndex, lineIndex + 1);
    }
    expect(resolved, hit.identity).toEqual({ kind: 'present', at: { fileIndex: hit.fileIndex, rowKey: hit.rowKey, ordinal, place: hit.place } });
  }
  return { hits, capped, pauses };
}

function draft(id: string, newLine: number, body: string): DiffReviewComment {
  return {
    id, threadId: 't', scope: 'workspace', sourceKey: 'k', filePath: 'src/needle.ts', status: 'draft',
    newLine, side: 'new', selectedText: '', body, createdAt: 0, updatedAt: 0,
  };
}

function thread(id: string, line: number | null, bodies: string[], resolved = false): ReviewThread {
  return {
    id, path: 'src/needle.ts', line, side: 'right', isResolvable: true, isResolved: resolved, isOutdated: false,
    comments: bodies.map((body, index) => ({ authorLogin: 'a', body, createdAt: '', databaseID: index + 1, id: `n${index + 1}`, viewerCanEdit: false, viewerCanDelete: false })),
  };
}

describe('scanReviewFind', () => {
  it('finds paths and code text in document order, never the prefix, line numbers or hunk headers', async () => {
    const { hits, capped } = await scan(content(), 'NEEDLE');
    expect(capped).toBe(false);
    expect(hits.map((hit) => [hit.path, hit.place.kind, hit.line, hit.occurrence])).toEqual([
      ['src/needle.ts', 'header', -2, 0],
      ['src/needle.ts', 'line', 5, 0],
      ['src/needle.ts', 'line', 6, 0],
      ['src/needle.ts', 'line', 7, 0],
      ['src/needle.ts', 'line', 7, 1],
      ['b.ts', 'line', 6, 0],
    ]);
    expect(hits[1].rowKey).toBe('l:5:src/needle.ts');
    expect((await scan(content(), '+const')).hits).toHaveLength(0);
    expect((await scan(content(), '-const')).hits).toHaveLength(0);
    expect((await scan(content(), '@@')).hits).toHaveLength(0);
    expect((await scan(content(), 'needleInHunk')).hits).toHaveLength(0);
    expect((await scan(content(), '   ')).hits).toHaveLength(0);
  });

  it('gives each match an identity its line numbers name', async () => {
    const { hits } = await scan(content(), 'needle');
    expect(hits.map((hit) => hit.identity.split('\u0000').slice(1))).toEqual([
      ['h', '0'], ['c1:1', '0'], ['d2', '0'], ['a2', '0'], ['a2', '1'], ['a11', '0'],
    ]);
  });

  it('stops at its limit and says so', async () => {
    const { hits, capped } = await scan(content(), 'needle', 4);
    expect(hits).toHaveLength(4);
    expect(capped).toBe(true);
    const exact = await scan(content(), 'needle', 6);
    expect(exact.hits).toHaveLength(6);
  });

  // Placement is the row model's (reviewInsertsByFile): a thread on a
  // line the diff does not show sits under the header.
  it('inserts comment bodies after the line they anchor, the rest under the header', async () => {
    const source = content({
      drafts: [draft('d1', 2, 'a needle draft'), { ...draft('d2', 0, 'needle at file level'), side: 'file', newLine: undefined }],
      prThreads: [
        thread('p1', 2, ['first needle', '<!-- needle marker -->', 'reply needle needle']),
        thread('p2', 40, ['gone needle']),
      ],
    });
    const { hits } = await scan(source, 'needle');
    expect(hits.map((hit) => [hit.rowKey, hit.occurrence])).toEqual([
      ['h:src/needle.ts', 0],
      ['dc:d2', 0],
      ['pc:p2:1', 0],
      ['l:5:src/needle.ts', 0],
      ['l:6:src/needle.ts', 0],
      // New line 2 is the added row (line 7), not the deleted row before it.
      ['l:7:src/needle.ts', 0],
      ['l:7:src/needle.ts', 1],
      ['dc:d1', 0],
      ['pc:p1:1', 0],
      ['pc:p1:3', 0],
      ['pc:p1:3', 1],
      ['l:6:b.ts', 0],
    ]);
    expect(hits.find((hit) => hit.rowKey === 'pc:p1:1')?.place).toEqual({ kind: 'thread', threadId: 'p1' });
  });

  it('counts a thread\'s held reply at its foot, and nothing open in its editor', async () => {
    const reply: DiffReviewComment = { ...draft('r1', 0, 'held needle reply'), scope: 'pr', side: 'reply', newLine: undefined, replyThreadId: 'p1' };
    const source = content({ drafts: [reply], prThreads: [thread('p1', 2, ['first needle', 'second needle'])] });
    expect((await scan(source, 'needle')).hits.filter((hit) => hit.rowKey.startsWith('pc:') || hit.rowKey.startsWith('dc:')).map((hit) => hit.rowKey))
      .toEqual(['pc:p1:1', 'pc:p1:2', 'dc:r1']);
    // A sent comment and a held reply open in their editors show the
    // editor, so their bodies are not counted.
    const editing = content({ ...source, editing: new Set(['pc:p1:2', 'dc:r1']) });
    expect((await scan(editing, 'needle')).hits.filter((hit) => hit.rowKey.startsWith('pc:') || hit.rowKey.startsWith('dc:')).map((hit) => hit.rowKey))
      .toEqual(['pc:p1:1']);
  });

  it('searches a collapsed conflict fold through its hidden lines, with the identity it has once open', async () => {
    const merged = ['a', 'b', 'needle hidden', 'c', 'd', 'e', 'f', 'g', 'h', '<<<<<<< ours', 'mine', '=======', 'theirs needle', '>>>>>>> theirs', ''].join('\n');
    const folded = reviewFileFromPatchFile(conflictPatchFile('m.go', merged));
    const folds = new Map<number, readonly string[]>();
    conflictPatchFile('m.go', merged, { onFold: (id, lines) => folds.set(id, lines) });
    const closed = await scan({ files: [folded], drafts: [], prThreads: [], commentText: (body) => [{ text: body, shown: true }], foldLines: () => folds }, 'needle');
    expect(closed.hits.map((hit) => hit.place.kind)).toEqual(['fold', 'line']);
    const opened = reviewFileFromPatchFile(conflictPatchFile('m.go', merged, { expandedFolds: new Set([0]) }));
    const open = await scan({ files: [opened], drafts: [], prThreads: [], commentText: (body) => [{ text: body, shown: true }], foldLines: () => new Map() }, 'needle');
    expect(open.hits.map((hit) => hit.place.kind)).toEqual(['line', 'line']);
    expect(open.hits.map((hit) => hit.identity)).toEqual(closed.hits.map((hit) => hit.identity));
    // Markers are content; the fold row's own label is not.
    expect((await scan({ files: [folded], drafts: [], prThreads: [], commentText: (body) => [{ text: body, shown: true }], foldLines: () => folds }, '=======')).hits).toHaveLength(1);
    expect((await scan({ files: [folded], drafts: [], prThreads: [], commentText: (body) => [{ text: body, shown: true }], foldLines: () => folds }, 'unchanged lines')).hits).toHaveLength(0);
  });

  it('pauses between slices and stops when it is no longer live', async () => {
    const lines = Array.from({ length: 5000 }, (_, i) => `+line ${i} needle`);
    const big = files(['diff --git a/big b/big', '--- a/big', '+++ b/big', `@@ -0,0 +1,${lines.length} @@`, ...lines, ''].join('\n'));
    let clock = 0;
    let pauses = 0;
    const all: ReviewFindHit[] = [];
    await scanReviewFind({ files: big, drafts: [], prThreads: [], commentText: (body) => [{ text: body, shown: true }] }, 'needle', {
      limit: 10_000, live: () => true, emit: (hit) => all.push(hit),
      pause: async () => { pauses += 1; }, now: () => (clock += 1), sliceMs: 0,
    });
    expect(all).toHaveLength(5000);
    expect(pauses).toBeGreaterThan(10);

    let live = true;
    const some: ReviewFindHit[] = [];
    const capped = await scanReviewFind({ files: big, drafts: [], prThreads: [], commentText: (body) => [{ text: body, shown: true }] }, 'needle', {
      limit: 10_000, live: () => live, emit: (hit) => some.push(hit),
      pause: async () => { live = false; }, now: () => (clock += 1), sliceMs: 10,
    });
    expect(capped).toBe(false);
    expect(some.length).toBeGreaterThan(0);
    expect(some.length).toBeLessThan(5000);
  });

  // A scan on a clock that spends a slice per read, which its owner stops
  // at the first pause: the matches it reached first.
  async function stopsInside(source: ReviewFindContent, query: string): Promise<{ pauses: number; found: number }> {
    let live = true;
    let pauses = 0;
    let clock = 0;
    let found = 0;
    await scanReviewFind(source, query, {
      limit: 1_000_000, live: () => live, emit: () => { found += 1; },
      pause: async () => { pauses += 1; live = false; }, now: () => (clock += 1), sliceMs: 0,
    });
    return { pauses, found };
  }

  it('one huge unit of work still pauses and stops inside it', async () => {
    // A conflict fold of many unchanged lines.
    const merged = [...Array.from({ length: 300_000 }, (_, i) => `same ${i}`), '<<<<<<< ours', 'a', '=======', 'b', '>>>>>>> theirs', ''].join('\n');
    const folded = reviewFileFromPatchFile(conflictPatchFile('m.go', merged));
    const folds = new Map<number, readonly string[]>();
    conflictPatchFile('m.go', merged, { onFold: (id, hidden) => folds.set(id, hidden) });
    expect([...folds.values()][0].length).toBeGreaterThan(299_000);
    const fold = await stopsInside({ files: [folded], drafts: [], prThreads: [], commentText: (body) => [{ text: body, shown: true }], foldLines: () => folds }, 'same');
    expect(fold.pauses).toBe(1);
    expect(fold.found).toBeLessThan(1000);

    // One line of 8 MB with a match every kilobyte.
    const line = `${'x'.repeat(994)}needle`.repeat(8000);
    const long = files(['diff --git a/min.js b/min.js', '--- a/min.js', '+++ b/min.js', '@@ -0,0 +1,1 @@', `+${line}`, ''].join('\n'));
    const minified = await stopsInside({ files: long, drafts: [], prThreads: [], commentText: (body) => [{ text: body, shown: true }] }, 'needle');
    expect(minified.pauses).toBe(1);
    expect(minified.found).toBeLessThan(100);

    // A comment of many blocks.
    const runs = Array.from({ length: 5000 }, (_, i) => ({ text: `paragraph ${i}`, shown: true }));
    const comment = await stopsInside(content({ prThreads: [thread('p1', 2, ['body'])], commentText: () => runs }), 'paragraph');
    expect(comment.pauses).toBe(1);
    expect(comment.found).toBeLessThan(1000);

    // Many files with no rows: only their headers.
    const headers = Array.from({ length: 5000 }, (_, i) => reviewFileFromPatchFile({ path: `f${i}.ts`, kind: 'conflict', additions: 0, deletions: 0, lines: [] }));
    const listed = await stopsInside({ files: headers, drafts: [], prThreads: [], commentText: (body) => [{ text: body, shown: true }] }, '.ts');
    expect(listed.pauses).toBe(1);
    expect(listed.found).toBeLessThan(1000);
  });

  it('a long line searched a slice at a time finds what a whole search finds, across slice edges', async () => {
    // Matches straddle the edges at 16384 and 32768; astral pairs sit on
    // the later ones.
    const body = 'x'.repeat(16_381) + 'NEEDLE' + 'y'.repeat(16_378) + 'needleneedle' + '\u{10400}'.repeat(20_000) + 'needle';
    const long = files(['diff --git a/l b/l', '--- a/l', '+++ b/l', '@@ -0,0 +1,1 @@', `+${body}`, ''].join('\n'));
    const { hits } = await scan({ files: long, drafts: [], prThreads: [], commentText: (b) => [{ text: b, shown: true }] }, 'needle');
    expect(hits.map((hit) => hit.occurrence)).toEqual([0, 1, 2, 3]);
    // Overlapping candidates count left to right without overlap.
    const repeat = files(['diff --git a/r b/r', '--- a/r', '+++ b/r', '@@ -0,0 +1,1 @@', `+b${'a'.repeat(40_001)}`, ''].join('\n'));
    expect((await scan({ files: repeat, drafts: [], prThreads: [], commentText: (b) => [{ text: b, shown: true }] }, 'aa', 100_000)).hits).toHaveLength(20_000);
  });

  it('searches a PR comment as its markdown renders, never its markup', async () => {
    const source = content({
      prThreads: [thread('p1', 2, ['**nee**dle in `code`', 'plain [linked](https://example.com/needle) words'])],
      commentText: ((texts) => (body: string) => texts(body).all)(createMarkdownFindTextCache({ embeddedHtml: true })),
    });
    expect((await scan(source, 'needle')).hits.filter((hit) => hit.place.kind === 'thread').map((hit) => hit.rowKey)).toEqual(['pc:p1:1']);
    expect((await scan(source, 'in code')).hits).toHaveLength(1);
    expect((await scan(source, '**')).hits).toHaveLength(0);
    expect((await scan(source, 'example.com')).hits).toHaveLength(0);
    expect((await scan(source, 'linked words')).hits).toHaveLength(1);
  });
});

describe('scanReviewFind over hunk gaps', () => {
  const GAPPED = [
    'diff --git a/app.ts b/app.ts',
    '--- a/app.ts',
    '+++ b/app.ts',
    '@@ -10,2 +10,2 @@',
    ' ctx1',
    '-old1',
    '+new1',
    '@@ -40,2 +40,2 @@',
    ' ctx2',
    '-old2',
    '+new2',
    '',
  ].join('\n');

  it('a gap the reader expanded is searched, an unexpanded one is not', async () => {
    const [file] = parseReviewFiles(GAPPED);
    expect((await scan(content({ files: [file] }), 'gap needle')).hits).toHaveLength(0);
    const lines = new Map<number, string>([[12, 'const gap = "gap needle";'], [13, 'plain']]);
    const expanded = applyContextExpansion(file, { lines, eofLine: null, version: 1 });
    const { hits } = await scan(content({ files: [expanded] }), 'gap needle');
    expect(hits.map((hit) => [hit.place.kind, hit.identity.split('\u0000')[1]])).toEqual([['line', 'c12:12']]);
  });
});

describe('scanReviewFind over evicted text', () => {
  const disposers: (() => void)[] = [];
  afterEach(() => {
    for (const dispose of disposers.splice(0)) dispose();
    setPatchTextBudgetForTest(null);
  });

  function bigPatch(count = 400): string {
    const lines = Array.from({ length: count }, (_, i) => `+line ${i}${i % 50 === 7 ? ' needle' : ''}`);
    return ['diff --git a/big.ts b/big.ts', '--- a/big.ts', '+++ b/big.ts', `@@ -0,0 +1,${lines.length} @@`, ...lines, ''].join('\n');
  }

  function streamed(count = 400) {
    setPatchTextBudgetForTest(300);
    const out = streamPatch(bigPatch(count), 200);
    disposers.push(() => out.parser.store.dispose());
    expect(out.files[0].body.resident()).toBe(false);
    return out;
  }

  it('reads evicted text back and finds what a resident copy finds', async () => {
    const { files: evicted, source } = streamed();
    const reference = parseReviewFiles(bigPatch());
    disposers.push(() => reference[0].body.segments[0].store.dispose());
    const want = (await scan(content({ files: reference }), 'needle')).hits.map((hit) => hit.identity);
    expect(want).toHaveLength(8);
    const got = (await scan(content({ files: evicted }), 'needle')).hits.map((hit) => hit.identity);
    expect(got).toEqual(want);
    expect(source.read).toHaveBeenCalled();
  });

  it('a scan replaced while a read is out goes no further once it lands', async () => {
    // Four read-back ranges of text: the scan stops within the first.
    const { files: evicted, source, spans } = streamed(2000);
    const gates: (() => void)[] = [];
    const real = source.read.getMockImplementation()!;
    source.read.mockImplementation(async (offset, next, packed) => {
      await new Promise<void>((resolve) => gates.push(resolve));
      return real(offset, next, packed);
    });
    let live = true;
    const hits: ReviewFindHit[] = [];
    const done = scanReviewFind(content({ files: evicted }), 'needle', {
      limit: 1000, live: () => live, emit: (hit) => hits.push(hit), pause: async () => {},
    });
    await expect.poll(() => gates.length).toBeGreaterThan(0);
    const emitted = hits.length;
    const reads = source.read.mock.calls.length;
    live = false;
    // The pinned range may span chunks that read one after another.
    let settled = false;
    void done.finally(() => { settled = true; });
    while (!settled) {
      gates.shift()?.();
      await new Promise((resolve) => setTimeout(resolve, 0));
    }
    await expect(done).resolves.toBe(false);
    expect(hits.length).toBe(emitted);
    expect(reads).toBeGreaterThan(0);
    // Only the range that was out was read, never the ranges after it.
    expect(source.read.mock.calls.length).toBeLessThan(spans.length / 2);
  });

  it('a read that fails rejects the scan', async () => {
    const { files: evicted, source } = streamed();
    source.read.mockRejectedValue(new Error('backend gone'));
    await expect(scan(content({ files: evicted }), 'needle')).rejects.toThrow();
  });

  it('a scan replaced while a block read is out emits nothing more and resolves false', async () => {
    const { files: evicted, source } = streamed(2000);
    // A budget that holds the block once it is read back.
    setPatchTextBudgetForTest(1 << 30);
    const gates: (() => void)[] = [];
    const real = source.read.getMockImplementation()!;
    source.read.mockImplementation(async (offset, next, packed) => {
      await new Promise<void>((resolve) => gates.push(resolve));
      return real(offset, next, packed);
    });
    let live = true;
    let emitted = 0;
    // Every line matches; the path does not.
    const done = scanReviewFind(content({ files: evicted }), 'line', {
      limit: 100_000, live: () => live, emit: () => { emitted += 1; }, pause: async () => {},
    });
    await expect.poll(() => gates.length).toBeGreaterThan(0);
    expect(emitted).toBe(0);
    live = false;
    let settled = false;
    void done.finally(() => { settled = true; });
    while (!settled) {
      gates.shift()?.();
      await new Promise((resolve) => setTimeout(resolve, 0));
    }
    await expect(done).resolves.toBe(false);
    expect(emitted).toBe(0);
  });
});

type ReferenceHit = ReviewFindHit & ReviewFindFileSpot & { fileIndex: number };

// The scan as it was before the filter: the rule run on every row's text.
// The filtered scan must emit what this emits, in this order.
async function referenceScan(content: ReviewFindContent, query: string, limit: number): Promise<{ hits: ReferenceHit[]; capped: boolean }> {
  const hits: ReferenceHit[] = [];
  const needle = findNeedle(query);
  if (!needle) return { hits, capped: false };
  let emitted = 0;
  const eachMatch = (run: string, each: () => boolean): boolean => {
    for (let from = 0; from + needle.length <= run.length;) {
      const chunk = findMatchChunk(run, from, needle);
      for (let i = 0; i < chunk.count; i += 1) if (each()) return true;
      if (chunk.last) break;
      from = chunk.next;
    }
    return false;
  };
  const emitText = (text: string | readonly FindRun[], file: ReviewFile, at: ReviewFindAt, spot: ReviewFindFileSpot): boolean => {
    const runs = typeof text === 'string' ? [{ text, shown: true }] : text;
    for (let i = 0; i < runs.length; i += 1) {
      if (!runs[i].shown) continue;
      let occurrence = 0;
      if (eachMatch(runs[i].text, () => {
        hits.push({ ...makeReviewFindHit(file.path, at, i, occurrence), ...spot, fileIndex: content.files.indexOf(file) });
        occurrence += 1;
        emitted += 1;
        return emitted >= limit;
      })) return true;
    }
    return false;
  };
  const lineText = async (file: ReviewFile, lineIndex: number): Promise<string> => {
    const body = file.body;
    if (body.residentLine(lineIndex)) return body.text(lineIndex);
    return await body.whenResident(() => body.text(lineIndex), lineIndex, Math.min(body.lineCount, lineIndex + 512));
  };
  const inserts = reviewInsertsByFile(content.files, content.drafts, [], content.prThreads, new Set());
  for (const file of content.files) {
    const path = file.path;
    if (emitText(path, file, { kind: 'header' }, { rowKey: findHeaderKey(path), line: -2, place: { kind: 'header' } })) return { hits, capped: true };
    const fileInserts = inserts.get(path);
    const emitInsert = (insert: InsertRow, line: number): boolean => {
      if (insert.kind === 'comment-thread') {
        const draft = content.drafts.find((comment) => comment.id === insert.threadKey);
        if (!draft) return false;
        const rowKey = findDraftKey(draft.id);
        if (content.editing?.has(rowKey)) return false;
        return emitText(draft.body, file, { kind: 'draft', commentId: draft.id }, { rowKey, line, place: { kind: 'draft', commentId: draft.id } });
      }
      if (insert.kind !== 'pr-thread') return false;
      const thread = insert.thread;
      for (const comment of thread.comments) {
        const key = reviewCommentKey(comment);
        const rowKey = findThreadCommentKey(thread.id, key);
        if (content.editing?.has(rowKey)) continue;
        const shown = findableBody(comment);
        if (shown !== '' && emitText(content.commentText(shown, rowKey), file, { kind: 'thread', threadId: thread.id, comment: key }, {
          rowKey, line, place: { kind: 'thread', threadId: thread.id },
        })) return true;
      }
      for (const draft of content.drafts) {
        if (draft.status !== 'draft' || draft.replyThreadId !== thread.id) continue;
        const rowKey = findDraftKey(draft.id);
        if (content.editing?.has(rowKey)) continue;
        if (emitText(draft.body, file, { kind: 'draft', commentId: draft.id }, { rowKey, line, place: { kind: 'thread', threadId: thread.id } })) return true;
      }
      return false;
    };
    const emitInserts = (bucket: readonly InsertRow[] | undefined, line: number): boolean => (bucket ?? []).some((insert) => emitInsert(insert, line));
    if (emitInserts(fileInserts?.takeHead(), -1)) return { hits, capped: true };
    const body = file.body;
    const folds = body.hasMarkers ? (content.foldLines?.(path) ?? null) : null;
    const walker = new RowWalker(file);
    let markerRun = 0;
    let lastLine = -1;
    const media = content.showsMedia?.(file) ?? false;
    while (!media && walker.next()) {
      if (walker.type === 'gap') continue;
      const lineIndex = walker.lineIndex;
      lastLine = lineIndex;
      const lineSpot: ReviewFindFileSpot = { rowKey: findLineKey(path, lineIndex), line: lineIndex, place: { kind: 'line', lineIndex } };
      if (walker.type === 'marker') {
        const fold = body.fold(lineIndex);
        const { oldAt, newAt } = findMarkerLines(walker);
        if (fold) {
          markerRun = 0;
          const hidden = folds?.get(fold.id) ?? [];
          const place: ReviewFindPlace = { kind: 'fold', foldId: fold.id };
          for (let h = 0; h < hidden.length; h += 1) {
            if (emitText(hidden[h], file, { kind: 'line', tag: `c${oldAt + h}:${newAt + h}` }, { rowKey: lineSpot.rowKey, line: lineIndex, place, foldLine: h })) return { hits, capped: true };
          }
        } else {
          const tag = `m${oldAt}:${newAt}:${markerRun}`;
          markerRun += 1;
          if (emitText(await lineText(file, lineIndex), file, { kind: 'line', tag }, lineSpot)) return { hits, capped: true };
        }
      } else {
        markerRun = 0;
        if (emitText((await lineText(file, lineIndex)).slice(1), file, { kind: 'line', tag: findRowTag(walker) }, lineSpot)) return { hits, capped: true };
      }
      const bucket = fileInserts?.take(walker.type, walker.oldLine, walker.newLine);
      if (bucket && emitInserts(bucket, lineIndex)) return { hits, capped: true };
    }
    if (emitInserts(fileInserts?.takeRest(), lastLine)) return { hits, capped: true };
  }
  return { hits, capped: false };
}

async function filteredScan(content: ReviewFindContent, query: string, limit: number, over: Partial<ReviewFindScanOptions> = {}): Promise<{ hits: ReferenceHit[]; capped: boolean }> {
  const hits: ReferenceHit[] = [];
  const capped = await scanReviewFind(content, query, {
    limit, live: () => true, pause: async () => {},
    emit: (hit, file, spot) => hits.push({ ...hit, ...spot, fileIndex: content.files.indexOf(file) }),
    ...over,
  });
  return { hits, capped };
}

function random(seed: number): (n: number) => number {
  let state = seed >>> 0;
  return (n) => {
    state = (Math.imul(state, 1664525) + 1013904223) >>> 0;
    return state % n;
  };
}

// Capitals, letters past ASCII with and without a fold, the Kelvin sign,
// dotted and dotless i, Deseret, regex syntax, the prefix characters, and
// characters whose plain form is a packed spelling (Ã©).
const SCAN_ALPHABET = [
  'a', 'b', 'x', 'A', 'B', 'k', 'K', 'K', 'i', 'I', 'İ', 'ı', 'é', 'É', 'Ã', '©', 'σ', 'Σ', 'ς',
  '中', '\u{10400}', '\u{10428}', '�', '+', '-', ' ', '.', '[', '\\', '(', 'Needle', 'needle', 'NeedleNeedle',
];

const SCAN_QUERIES = [
  '+', '-', ' ', 'a', 'A', 'ab', 'k', 'K', 'K', 'i', 'İ', 'ı', 'é', 'É', 'Ã©', 'σ', 'Σ', '\u{10400}', '\ud801',
  '�', '+a', '-b', ' a', '.', '[', '\\', 'needle', 'NEEDLE', 'x\nx', 'a.b', 'École', '<', '|', '=', '>', 'xxN',
  // Longer than the spelled prefix (FIND_FILTER_CODE_POINTS), which lines
  // hold without the rest.
  'needleNeedleneedleX', 'NEEDLENEEDLEab', 'needleneedle needle',
];

function randomWord(next: (n: number) => number, length: number): string {
  let out = '';
  for (let i = 0; i < length; i += 1) out += SCAN_ALPHABET[next(SCAN_ALPHABET.length)];
  return out;
}

// A line of `length` characters with a match of `word` straddling each
// LONG_TEXT edge of the code text.
function longLine(length: number, word: string): string {
  let out = '';
  for (let edge = LONG_TEXT; out.length < length; edge += LONG_TEXT) {
    out += 'x'.repeat(Math.max(0, edge - 2 - out.length)) + word;
  }
  return out;
}

const SCAN_PATHS = ['src/a.ts', 'src/École.ts', 'K/needle.ts', 'c.ts'];

function randomPatch(next: (n: number) => number, long: boolean): string {
  const out: string[] = [];
  for (const path of SCAN_PATHS.slice(0, 1 + next(SCAN_PATHS.length))) {
    out.push(`diff --git a/${path} b/${path}`, 'index 1..2 100644', `--- a/${path}`, `+++ b/${path}`);
    let old = 1 + next(5);
    let neu = old;
    for (let hunk = 1 + next(3); hunk > 0; hunk -= 1) {
      const rows: string[] = [];
      let oldCount = 0;
      let newCount = 0;
      for (let row = 1 + next(25); row > 0; row -= 1) {
        const prefix = [' ', '+', '-'][next(3)];
        if (prefix !== '+') oldCount += 1;
        if (prefix !== '-') newCount += 1;
        rows.push(prefix + randomWord(next, next(10)));
      }
      if (long && hunk === 1) {
        // Inside a block, and over the block size.
        rows.push(`+${longLine(40_000, 'Needle')}`, `+${longLine(70_000, 'aB')}`);
        newCount += 2;
      }
      out.push(`@@ -${old},${oldCount} +${neu},${newCount} @@ needle ${randomWord(next, 3)}`, ...rows);
      const gap = next(2) === 0 ? 0 : 3 + next(10);
      old += oldCount + gap;
      neu += newCount + gap;
    }
  }
  // A type change: one file over two segments of the store.
  out.push(
    'diff --git a/t.ts b/t.ts', 'deleted file mode 100644', '--- a/t.ts', '+++ /dev/null', '@@ -1,2 +0,0 @@',
    `-${randomWord(next, 6)}`, `-${randomWord(next, 6)}`,
    'diff --git a/t.ts b/t.ts', 'new file mode 120000', '--- /dev/null', '+++ b/t.ts', '@@ -0,0 +1,2 @@',
    `+${randomWord(next, 6)}`, `+${randomWord(next, 6)}`,
  );
  return out.join('\n') + '\n';
}

// A conflict file with folds, as the review pane shows one collapsed.
function randomConflict(next: (n: number) => number): { file: ReviewFile; folds: Map<number, readonly string[]> } {
  const run = (count: number) => Array.from({ length: count }, () => randomWord(next, next(8)));
  const merged = [
    ...run(2 + next(10)), '<<<<<<< ours', ...run(1 + next(3)), '=======', ...run(1 + next(3)), '>>>>>>> theirs',
    ...run(next(12)), '<<<<<<< ours', ...run(1 + next(2)), '||||||| base', ...run(next(2)), '=======', ...run(next(2)), '>>>>>>> theirs', '',
  ].join('\n');
  const folds = new Map<number, readonly string[]>();
  const file = reviewFileFromPatchFile(conflictPatchFile('m.go', merged, { onFold: (id, lines) => folds.set(id, lines) }));
  return { file, folds };
}

function randomComments(next: (n: number) => number, files: readonly ReviewFile[]): { drafts: DiffReviewComment[]; prThreads: ReviewThread[] } {
  const drafts: DiffReviewComment[] = [];
  const prThreads: ReviewThread[] = [];
  for (let i = next(6); i > 0; i -= 1) {
    const path = files[next(files.length)].path;
    drafts.push({ ...draft(`d${i}`, 1 + next(30), randomWord(next, 1 + next(6))), filePath: path });
  }
  for (let i = next(4); i > 0; i -= 1) {
    const path = files[next(files.length)].path;
    prThreads.push({ ...thread(`p${i}`, next(4) === 0 ? null : 1 + next(30), Array.from({ length: 1 + next(3) }, () => randomWord(next, 1 + next(6)))), path });
  }
  return { drafts, prThreads };
}

describe('scanReviewFind filtered by the stored text', () => {
  const disposers: (() => void)[] = [];
  afterEach(() => {
    for (const dispose of disposers.splice(0)) dispose();
    setPatchTextBudgetForTest(null);
  });

  it('emits what the rule on every row emits, over random diffs in every form', async () => {
    const forms: (PatchForm | 'resident')[] = ['resident', 'plain', 'packed', 'plain-first'];
    let compared = 0;
    let prefixOnly = 0;
    for (let round = 0; round < 48; round += 1) {
      const next = random(1000 + round);
      const patch = randomPatch(next, round % 8 === 0);
      const form = forms[round % forms.length];
      let files: ReviewFile[];
      if (form === 'resident') {
        files = parseReviewFiles(patch);
      } else {
        setPatchTextBudgetForTest(300 + next(600));
        const streamed = streamPatch(patch, () => 40 + next(300), form);
        disposers.push(() => streamed.parser.store.dispose());
        files = streamed.files;
      }
      const conflict = randomConflict(next);
      files = [...files, conflict.file];
      const { drafts, prThreads } = randomComments(next, files);
      const source = content({ files, drafts, prThreads, foldLines: (path) => (path === 'm.go' ? conflict.folds : null) });
      const lines = patch.split('\n');
      const queries = [...SCAN_QUERIES, ...Array.from({ length: 6 }, () => {
        const line = [...lines[next(lines.length)]];
        const at = next(line.length + 1);
        return line.slice(at, at + 1 + next(4)).join('');
      })];
      for (const query of queries) {
        const limit = next(4) === 0 ? 1 + next(20) : 100_000;
        const want = await referenceScan(source, query, limit);
        const got = await filteredScan(source, query, limit);
        expect(got, `round ${round} (${form}) ${JSON.stringify(query)} limit ${limit}`).toEqual(want);
        if (want.hits.some((hit) => hit.place.kind === 'line')) compared += 1;
        const needle = findNeedle(query);
        if ([...needle].length > FIND_FILTER_CODE_POINTS) {
          const prefix = findFilterPattern(needle)!;
          prefixOnly += lines.filter((line) => {
            prefix.lastIndex = 0;
            return prefix.test(line) && !foldFindText(line).includes(needle);
          }).length;
        }
      }
    }
    expect(compared).toBeGreaterThan(400);
    // The prefix flagged lines the whole needle does not match.
    expect(prefixOnly).toBeGreaterThan(20);
  });

  it('reads no line text for lines that cannot match, and still yields by the clock', async () => {
    const lines = Array.from({ length: 20_000 }, (_, i) => `+line ${i}${i % 7 === 0 ? ' Ã©' : ''} café`);
    const patch = ['diff --git a/big b/big', '--- a/big', '+++ b/big', `@@ -0,0 +1,${lines.length} @@`, ...lines, ''].join('\n');
    setPatchTextBudgetForTest(64 * 1024);
    const packed = streamPatch(patch, 4096, 'packed');
    disposers.push(() => packed.parser.store.dispose());
    expect(packed.files[0].body.resident()).toBe(false);
    for (const files of [parseReviewFiles(patch), packed.files]) {
      const reads = [
        vi.spyOn(PatchBody.prototype, 'text'),
        vi.spyOn(PatchStore.prototype, 'text'),
        vi.spyOn(PatchStore.prototype, 'textRange'),
        vi.spyOn(PatchBody.prototype, 'patchText'),
      ];
      try {
        let clock = 0;
        let pauses = 0;
        const { hits } = await filteredScan(content({ files }), 'needle', 1000, {
          now: () => (clock += 1), sliceMs: 0, pause: async () => { pauses += 1; },
        });
        expect(hits).toHaveLength(0);
        // A needle holding a newline matches no line.
        expect((await filteredScan(content({ files }), 'café\n+line', 1000)).hits).toHaveLength(0);
        for (const read of reads) expect(read).not.toHaveBeenCalled();
        // A clock read per CLOCK_ROWS (256) rows, each past its slice.
        expect(pauses).toBeGreaterThanOrEqual(Math.floor(lines.length / 256));
        // A line that can match is read, once.
        expect((await filteredScan(content({ files }), 'é', 100_000)).hits).toHaveLength(lines.length);
        expect(reads[0]).toHaveBeenCalledTimes(lines.length);
      } finally {
        for (const read of reads) read.mockRestore();
      }
    }
  });

  it('reads blocks of bounded size, one segment at a time', async () => {
    const wide = Array.from({ length: 40 }, (_, i) => `+${String(i).padStart(10_000, 'w')}`);
    const short = Array.from({ length: 3000 }, (_, i) => `+s${i}`);
    const patch = [
      'diff --git a/w b/w', '--- a/w', '+++ b/w', `@@ -0,0 +1,${wide.length + short.length} @@`, ...wide, ...short,
      'diff --git a/t b/t', 'deleted file mode 100644', '--- a/t', '+++ /dev/null', '@@ -1,1 +0,0 @@', '-gone',
      'diff --git a/t b/t', 'new file mode 120000', '--- /dev/null', '+++ b/t', '@@ -0,0 +1,1 @@', '+came',
      '',
    ].join('\n');
    const files = parseReviewFiles(patch);
    expect(files[1].body.segments).toHaveLength(2);
    const blocks = vi.spyOn(PatchBody.prototype, 'storedRange');
    try {
      await filteredScan(content({ files }), 'needle', 1000);
      const read = blocks.mock.results.map((result) => result.value as string);
      expect(read.length).toBeGreaterThan(wide.length + short.length / 512);
      for (const stored of read) {
        expect(stored.length).toBeLessThanOrEqual(LONG_TEXT);
        expect(stored.split('\n').length).toBeLessThanOrEqual(512);
      }
      // Each segment of the type change is its own block.
      expect(read.slice(-2)).toEqual(['-gone', '+came']);
    } finally {
      blocks.mockRestore();
    }
  });

  it('searches on from the end of a line it flags, not from each match', async () => {
    const lines = [
      `+${'needle '.repeat(2000)}`,
      ...Array.from({ length: 100 }, (_, i) => `+plain ${i}`),
      ...Array.from({ length: 100 }, (_, i) => `+needle ${i}`),
    ];
    const files = parseReviewFiles(['diff --git a/n b/n', '--- a/n', '+++ b/n', `@@ -0,0 +1,${lines.length} @@`, ...lines, ''].join('\n'));
    const exec = vi.spyOn(RegExp.prototype, 'exec');
    const text = vi.spyOn(PatchBody.prototype, 'text');
    try {
      const { hits } = await filteredScan(content({ files }), 'needle', 100_000);
      expect(hits).toHaveLength(2100);
      // Only the lines that match are read.
      expect(text).toHaveBeenCalledTimes(101);
      const source = findFilterPattern(findNeedle('needle'))!.source;
      const filter = exec.mock.contexts.filter((pattern) => (pattern as RegExp).source === source);
      // One search per matching line, and the one that finds nothing more.
      expect(filter.length).toBeLessThanOrEqual(102);
    } finally {
      exec.mockRestore();
      text.mockRestore();
    }
  });

  it('a line over the block size is searched by the rule, which pauses inside it', async () => {
    // One line of 8 MB with no match.
    const long = files(['diff --git a/min.js b/min.js', '--- a/min.js', '+++ b/min.js', '@@ -0,0 +1,1 @@', `+${'x'.repeat(8_000_000)}`, ''].join('\n'));
    let live = true;
    let pauses = 0;
    let clock = 0;
    await scanReviewFind(content({ files: long }), 'needle', {
      limit: 1000, live: () => live, emit: () => {},
      pause: async () => { pauses += 1; live = false; }, now: () => (clock += 1), sliceMs: 0,
    });
    expect(pauses).toBe(1);
  });

  it('finds U+FFFD where packed text holds an invalid byte', async () => {
    const parser = new PatchParser();
    const data = ['diff --git a/l b/l', '--- a/l', '+++ b/l', '@@ -0,0 +1,2 @@', '+café ok', '+bad ÿ byte', ''].join('\n');
    // The packed chunk: 'é' as its UTF-8 bytes, and a lone 0xff byte.
    const packed = data.replace('é', 'Ã©');
    parser.append(packed, { offset: 0, nextOffset: packed.length, wide: wideMarks(packed), key: textKey(unpackText(packed)) });
    parser.end();
    const { hits } = await filteredScan(content({ files: parser.files }), '�', 1000);
    expect(hits.map((hit) => hit.identity.split('\u0000')[1])).toEqual(['a2']);
    expect((await filteredScan(content({ files: parser.files }), 'CAFÉ', 1000)).hits).toHaveLength(1);
  });
});

// One added file of `count` lines of `text`, in `form`.
function repeatedFiles(text: string, count: number, form: 'plain' | 'packed'): ReviewFile[] {
  const lines = Array.from({ length: count }, () => `+${text}`);
  const patch = ['diff --git a/r b/r', '--- a/r', '+++ b/r', `@@ -0,0 +1,${count} @@`, ...lines, ''].join('\n');
  if (form === 'plain') return parseReviewFiles(patch);
  return streamPatch(patch, 1 << 20, 'packed').files;
}

describe('scanReviewFind work between clock reads', () => {
  afterEach(() => setPatchTextBudgetForTest(null));

  // Stored characters the body reads between two clock reads, the largest
  // line read whole in each stretch set apart, on a clock that reads every
  // slice as spent.
  async function stretches(files: ReviewFile[], query: string): Promise<{ widest: number; pauses: number }> {
    const realText = PatchBody.prototype.text;
    const realStored = PatchBody.prototype.storedRange;
    let since = 0;
    let line = 0;
    let widest = 0;
    let pauses = 0;
    let clock = 0;
    const text = vi.spyOn(PatchBody.prototype, 'text').mockImplementation(function (this: PatchBody, index: number) {
      const size = this.lineSize(index);
      since += size;
      line = Math.max(line, size);
      return realText.call(this, index);
    });
    const stored = vi.spyOn(PatchBody.prototype, 'storedRange').mockImplementation(function (this: PatchBody, start: number, end: number) {
      const out = realStored.call(this, start, end);
      since += out.length;
      return out;
    });
    const close = () => {
      widest = Math.max(widest, since - line);
      since = 0;
      line = 0;
    };
    try {
      await scanReviewFind(content({ files }), query, {
        limit: 1000, live: () => true, emit: () => {}, sliceMs: 0,
        now: () => { close(); return (clock += 1); },
        pause: async () => { pauses += 1; },
      });
      close();
    } finally {
      text.mockRestore();
      stored.mockRestore();
    }
    return { widest, pauses };
  }

  it('counts what it reads and searches, so no stretch between clock reads is long', async () => {
    setPatchTextBudgetForTest(1 << 30);
    const longE = 'é'.repeat(60_000);
    const sigma = 'σ'.repeat(5000);
    const cases: [string, ReviewFile[], string][] = [
      // Long lines whose prefix matches everywhere and whole never.
      ['long é, plain', repeatedFiles(longE, 40, 'plain'), `${'é'.repeat(63)}x`],
      ['long é, packed', repeatedFiles(longE, 40, 'packed'), `${'é'.repeat(63)}x`],
      ['σ rows the rule reads', repeatedFiles(sigma, 2000, 'plain'), `${'σ'.repeat(63)}x`],
      // A line over the block size is never searched as a block.
      ['long é, spelled whole', repeatedFiles(longE, 40, 'plain'), `${'é'.repeat(11)}x`],
      // Blocks searched, no row flagged.
      ['σ blocks', repeatedFiles(sigma, 1000, 'plain'), `${'σ'.repeat(11)}x`],
      // Every row read (a needle the filter cannot spell).
      ['σ rows, every one', repeatedFiles(sigma, 1000, 'plain'), 'σ�'],
    ];
    for (const [name, files, query] of cases) {
      const { widest, pauses } = await stretches(files, query);
      expect(pauses, name).toBeGreaterThan(10);
      // Under CLOCK_ROWS units counted plus one item of at most LONG_TEXT.
      expect(widest, name).toBeLessThan(2 * LONG_TEXT);
    }
  });

  it('counts what a needle longer than a chunk folds past the chunk', async () => {
    // Text that holds the needle's prefix everywhere: every chunk folds
    // the needle's length past its end.
    const needle = `${'a'.repeat(3 * LONG_TEXT)}x`;
    const code = 'a'.repeat(20 * LONG_TEXT);
    const files = repeatedFiles(code, 1, 'plain');
    const realSlice = String.prototype.slice;
    let since = 0;
    let widest = 0;
    let folded = 0;
    let reads = 0;
    let clock = 0;
    // The rule's chunks: slices of the row's code text.
    const slice = vi.spyOn(String.prototype, 'slice').mockImplementation(function (this: string, ...args: [number?, number?]) {
      const out = realSlice.apply(this, args);
      if (this.length === code.length && out.length < code.length) {
        since += out.length;
        folded += out.length;
      }
      return out;
    });
    try {
      await scanReviewFind(content({ files }), needle, {
        limit: 1000, live: () => true, emit: () => {}, sliceMs: 0, pause: async () => {},
        now: () => { reads += 1; widest = Math.max(widest, since); since = 0; return (clock += 1); },
      });
    } finally {
      slice.mockRestore();
    }
    widest = Math.max(widest, since);
    expect(folded).toBeGreaterThan(20 * LONG_TEXT);
    const bound = 2 * LONG_TEXT + needle.length;
    expect(widest).toBeLessThan(bound);
    expect(reads).toBeGreaterThanOrEqual(Math.floor(folded / bound));
  });
});

describe('scanReviewFind pattern work', () => {
  afterEach(() => setPatchTextBudgetForTest(null));

  // A pattern step costs at most this fraction of a character the rule
  // folds and searches: measured on "é" text, a step (one spelled code
  // point tried at one position) costs about 2 ns and a rule character
  // about 110 ns (findMatch.ts#FIND_FILTER_CODE_POINTS).
  const STEPS_PER_RULE_CHAR = 16;

  // Characters the rule reads (`text`), and pattern steps: characters the
  // filter searched, each start trying every spelled code point.
  async function work(files: ReviewFile[], query: string, scan: 'filtered' | 'reference'): Promise<number> {
    const realText = PatchBody.prototype.text;
    const realExec = RegExp.prototype.exec;
    const source = findFilterPattern(findNeedle(query))!.source;
    // Its spelled code points: one class or group each.
    const spelled = source.match(/\(\?:|(?<!\(\?:)\[/g)!.length;
    let rule = 0;
    let steps = 0;
    const text = vi.spyOn(PatchBody.prototype, 'text').mockImplementation(function (this: PatchBody, index: number) {
      const out = realText.call(this, index);
      rule += out.length;
      return out;
    });
    const exec = vi.spyOn(RegExp.prototype, 'exec').mockImplementation(function (this: RegExp, input: string) {
      const from = this.lastIndex;
      const out = realExec.call(this, input);
      if (this.source === source) steps += ((out ? out.index + out[0].length : input.length) - from) * spelled;
      return out;
    });
    try {
      if (scan === 'filtered') await filteredScan(content({ files }), query, 100_000);
      else await referenceScan(content({ files }), query, 100_000);
    } finally {
      text.mockRestore();
      exec.mockRestore();
    }
    return rule + steps / STEPS_PER_RULE_CHAR;
  }

  it('a long needle over repetitive text costs at most twice the rule on every row', async () => {
    setPatchTextBudgetForTest(1 << 30);
    for (const form of ['plain', 'packed'] as const) {
      const files = repeatedFiles('é'.repeat(100), 1000, form);
      for (const query of [`${'é'.repeat(63)}x`, `${'é'.repeat(11)}x`, `${'é'.repeat(5)}x`]) {
        const reference = await work(files, query, 'reference');
        // Each row's text, its prefix included.
        expect(reference, `${form} ${query.length}`).toBe(101 * 1000);
        expect(await work(files, query, 'filtered'), `${form} ${query.length}`).toBeLessThanOrEqual(2 * reference);
      }
    }
  });
});
