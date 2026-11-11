import { afterEach, describe, expect, it } from 'vitest';
import { conflictPatchFile } from './conflictFile';
import { applyContextExpansion } from './diffContextExpansion';
import { setPatchTextBudgetForTest } from './patchMemory.svelte';
import { parseReviewFiles, reviewFileFromPatchFile, type ReviewFile } from './patchStore';
import { createMarkdownFindTextCache } from '../components/chat/markdown/markdownFindText';
import { streamPatch } from '../../test/helpers/streamedPatch';
import { scanReviewFind, type ReviewFindContent, type ReviewFindHit } from './reviewFindScan';
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
  return { files: files(), drafts: [], prThreads: [], commentText: (body) => [body], ...over };
}

async function scan(source: ReviewFindContent, query: string, limit = 1000): Promise<{ hits: ReviewFindHit[]; capped: boolean; pauses: number }> {
  const hits: ReviewFindHit[] = [];
  let pauses = 0;
  const capped = await scanReviewFind(source, query, {
    limit,
    live: () => true,
    emit: (hit) => hits.push(hit),
    pause: async () => { pauses += 1; },
  });
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
    comments: bodies.map((body, index) => ({ authorLogin: 'a', body, createdAt: '', databaseID: index })),
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
      ['pc:0:p2', 0],
      ['l:5:src/needle.ts', 0],
      ['l:6:src/needle.ts', 0],
      ['dc:d1', 0],
      ['pc:0:p1', 0],
      ['pc:2:p1', 0],
      ['pc:2:p1', 1],
      ['l:7:src/needle.ts', 0],
      ['l:7:src/needle.ts', 1],
      ['l:6:b.ts', 0],
    ]);
    expect(hits.find((hit) => hit.rowKey === 'pc:0:p1')?.place).toEqual({ kind: 'thread', threadId: 'p1' });
  });

  it('searches a collapsed conflict fold through its hidden lines, with the identity it has once open', async () => {
    const merged = ['a', 'b', 'needle hidden', 'c', 'd', 'e', 'f', 'g', 'h', '<<<<<<< ours', 'mine', '=======', 'theirs needle', '>>>>>>> theirs', ''].join('\n');
    const folded = reviewFileFromPatchFile(conflictPatchFile('m.go', merged));
    const folds = new Map<number, readonly string[]>();
    conflictPatchFile('m.go', merged, { onFold: (id, lines) => folds.set(id, lines) });
    const closed = await scan({ files: [folded], drafts: [], prThreads: [], commentText: (body) => [body], foldLines: () => folds }, 'needle');
    expect(closed.hits.map((hit) => hit.place.kind)).toEqual(['fold', 'line']);
    const opened = reviewFileFromPatchFile(conflictPatchFile('m.go', merged, { expandedFolds: new Set([0]) }));
    const open = await scan({ files: [opened], drafts: [], prThreads: [], commentText: (body) => [body], foldLines: () => new Map() }, 'needle');
    expect(open.hits.map((hit) => hit.place.kind)).toEqual(['line', 'line']);
    expect(open.hits.map((hit) => hit.identity)).toEqual(closed.hits.map((hit) => hit.identity));
    // Markers are content; the fold row's own label is not.
    expect((await scan({ files: [folded], drafts: [], prThreads: [], commentText: (body) => [body], foldLines: () => folds }, '=======')).hits).toHaveLength(1);
    expect((await scan({ files: [folded], drafts: [], prThreads: [], commentText: (body) => [body], foldLines: () => folds }, 'unchanged lines')).hits).toHaveLength(0);
  });

  it('pauses between slices and stops when it is no longer live', async () => {
    const lines = Array.from({ length: 5000 }, (_, i) => `+line ${i} needle`);
    const big = files(['diff --git a/big b/big', '--- a/big', '+++ b/big', `@@ -0,0 +1,${lines.length} @@`, ...lines, ''].join('\n'));
    let clock = 0;
    let pauses = 0;
    const all: ReviewFindHit[] = [];
    await scanReviewFind({ files: big, drafts: [], prThreads: [], commentText: (body) => [body] }, 'needle', {
      limit: 10_000, live: () => true, emit: (hit) => all.push(hit),
      pause: async () => { pauses += 1; }, now: () => (clock += 1), sliceMs: 0,
    });
    expect(all).toHaveLength(5000);
    expect(pauses).toBeGreaterThan(10);

    let live = true;
    const some: ReviewFindHit[] = [];
    const capped = await scanReviewFind({ files: big, drafts: [], prThreads: [], commentText: (body) => [body] }, 'needle', {
      limit: 10_000, live: () => live, emit: (hit) => some.push(hit),
      pause: async () => { live = false; }, now: () => (clock += 1), sliceMs: 10,
    });
    expect(capped).toBe(false);
    expect(some.length).toBeGreaterThan(0);
    expect(some.length).toBeLessThan(5000);
  });

  it('searches a PR comment as its markdown renders, never its markup', async () => {
    const source = content({
      prThreads: [thread('p1', 2, ['**nee**dle in `code`', 'plain [linked](https://example.com/needle) words'])],
      commentText: createMarkdownFindTextCache({ embeddedHtml: true }),
    });
    expect((await scan(source, 'needle')).hits.filter((hit) => hit.place.kind === 'thread').map((hit) => hit.rowKey)).toEqual(['pc:0:p1']);
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
