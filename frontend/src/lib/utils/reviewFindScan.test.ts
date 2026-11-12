import { afterEach, describe, expect, it } from 'vitest';
import { conflictPatchFile } from './conflictFile';
import { applyContextExpansion } from './diffContextExpansion';
import { setPatchTextBudgetForTest } from './patchMemory.svelte';
import { parseReviewFiles, reviewFileFromPatchFile, type ReviewFile } from './patchStore';
import { createMarkdownFindTextCache } from '../components/chat/markdown/markdownFindText';
import { streamPatch } from '../../test/helpers/streamedPatch';
import { scanReviewFind, type ReviewFindContent, type ReviewFindFileSpot, type ReviewFindHit } from './reviewFindScan';
import { forgetReviewFindSpotsForTest, placeReviewFindHit, resolveReviewFindHit } from './reviewFindRows';
import { findNeedle } from './findMatch';
import { shownOf } from '../../test/helpers/reviewFindOracle';
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
    comments: bodies.map((body, index) => ({ authorLogin: 'a', body, createdAt: '', databaseID: index + 1 })),
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
    source.read.mockImplementation(async (offset, next) => {
      await new Promise<void>((resolve) => gates.push(resolve));
      return real(offset, next);
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
});
