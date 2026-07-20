import { beforeEach, describe, expect, it } from 'vitest';
import {
  PATCH_PARSE_CACHE_MAX_ENTRY_CHARS,
  __parsePatchCacheStatsForTest,
  __resetParsePatchCacheForTest,
  buildPatchDisplayRows,
  buildSplitDisplayRows,
  extractPatchFile,
  mergePatchFilesByPath,
  mergePatchFilesByPathCached,
  parsePatchFiles,
  parsePatchFilesCached,
  patchFileRowId,
  stripPatchLinePrefix,
} from './patchFiles';

describe('parsePatchFiles', () => {
  it('builds aligned split rows for replacement hunks', () => {
    const [file] = parsePatchFiles(`diff --git a/app.ts b/app.ts
--- a/app.ts
+++ b/app.ts
@@ -1,3 +1,3 @@
 const keep = true;
-const value = 'old';
+const value = 'new';
 console.log(value);
`);

    expect(file.path).toBe('app.ts');
    const splitRows = buildSplitDisplayRows(buildPatchDisplayRows(file.lines));
    const replacement = splitRows.find(
      (row) => row.left?.line.type === 'del' && row.right?.line.type === 'add',
    );
    expect(replacement?.left?.line.content).toBe("-const value = 'old';");
    expect(replacement?.right?.line.content).toBe("+const value = 'new';");
  });

  it('places added-only rows on the right side', () => {
    const [file] = parsePatchFiles(`diff --git a/app.ts b/app.ts
--- a/app.ts
+++ b/app.ts
@@ -1 +1,2 @@
 const keep = true;
+const added = true;
`);

    const splitRows = buildSplitDisplayRows(buildPatchDisplayRows(file.lines));
    const addedOnly = splitRows.find(
      (row) => row.right?.line.content === '+const added = true;',
    );
    expect(addedOnly?.left).toBeNull();
  });

  it('classifies git extended headers as metadata', () => {
    const [file] = parsePatchFiles(`diff --git a/old.txt b/new.txt
similarity index 88%
rename from old.txt
rename to new.txt
index 1111111..2222222 100644
--- a/old.txt
+++ b/new.txt
@@ -1 +1 @@
-old
+new
`);

    expect(file.lines.filter((line) => line.type !== 'meta' && line.content !== '').map((line) => line.content)).toEqual([
      '-old',
      '+new',
    ]);
  });

  it('extracts a single file patch without changing its content', () => {
    const patch = `diff --git a/first.ts b/first.ts
--- a/first.ts
+++ b/first.ts
@@ -1 +1 @@
-old
+new
diff --git a/second.ts b/second.ts
--- a/second.ts
+++ b/second.ts
@@ -1 +1 @@
-before
+after
`;

    const extracted = extractPatchFile(patch, 'second.ts');

    expect(extracted).toContain('diff --git a/second.ts b/second.ts');
    expect(extracted).toContain('+after');
    expect(extracted).not.toContain('first.ts');
  });

  it('builds distinct row ids for duplicate file paths', () => {
    const files = parsePatchFiles(`diff --git a/notes.txt b/notes.txt
--- a/notes.txt
+++ b/notes.txt
@@ -1 +1 @@
-one
+two
diff --git a/notes.txt b/notes.txt
--- a/notes.txt
+++ b/notes.txt
@@ -3 +3 @@
-three
+four
`);

    expect(files.map((file, index) => patchFileRowId(file, index))).toEqual([
      '0:notes.txt',
      '1:notes.txt',
    ]);
  });

  it('derives old and new line anchors from hunk headers', () => {
    const [file] = parsePatchFiles(`diff --git a/app.ts b/app.ts
--- a/app.ts
+++ b/app.ts
@@ -8,3 +8,4 @@
 keep
-old
+new
+added
`);

    // Gap rows (hidden-context affordances) carry no anchors; drop them
    // here — their emission has its own suite below.
    const rows = buildPatchDisplayRows(file.lines).filter((row) => !row.gap);

    expect(rows.map((row) => ({
      content: row.line.content,
      oldLine: row.oldLine,
      newLine: row.newLine,
      side: row.side,
    }))).toEqual([
      { content: ' keep', oldLine: 8, newLine: 8, side: 'context' },
      { content: '-old', oldLine: 9, newLine: 0, side: 'old' },
      { content: '+new', oldLine: 0, newLine: 9, side: 'new' },
      { content: '+added', oldLine: 0, newLine: 10, side: 'new' },
    ]);
  });
});

describe('stripPatchLinePrefix', () => {
  it('strips the leading + from add lines', () => {
    expect(stripPatchLinePrefix({ type: 'add', content: '+const x = 1;' })).toBe('const x = 1;');
  });

  it('strips the leading - from del lines', () => {
    expect(stripPatchLinePrefix({ type: 'del', content: '-const x = 1;' })).toBe('const x = 1;');
  });

  it('returns meta lines unchanged', () => {
    expect(stripPatchLinePrefix({ type: 'meta', content: '@@ -1,3 +1,4 @@' })).toBe('@@ -1,3 +1,4 @@');
  });

  it('returns context lines unchanged including the leading space', () => {
    expect(stripPatchLinePrefix({ type: 'context', content: ' unchanged' })).toBe(' unchanged');
  });

  it('handles a bare "+" or "-" (empty source line)', () => {
    expect(stripPatchLinePrefix({ type: 'add', content: '+' })).toBe('');
    expect(stripPatchLinePrefix({ type: 'del', content: '-' })).toBe('');
  });
});

describe('parsePatchFilesCached', () => {
  // Module-global cache: reset before each test so eviction-budget math
  // never observes entries left by a prior test.
  beforeEach(() => {
    __resetParsePatchCacheForTest();
  });

  // Builds a syntactically valid patch padded to ~chars length, with
  // the path baked in so every generated patch is distinct content.
  function patchOfSize(path: string, chars: number): string {
    const header = `diff --git a/${path} b/${path}\n--- a/${path}\n+++ b/${path}\n@@ -1 +1,9 @@\n`;
    const line = `+${'x'.repeat(62)}\n`;
    const bodyLines = Math.max(1, Math.ceil((chars - header.length) / line.length));
    return header + line.repeat(bodyLines);
  }

  it('caches an input exactly at the per-entry cap (boundary is inclusive)', () => {
    // The bypass check is `length > cap`, so length === cap must still
    // cache. Pad an arbitrary string to exactly the cap.
    const atCap = 'x'.repeat(PATCH_PARSE_CACHE_MAX_ENTRY_CHARS);
    expect(atCap.length).toBe(PATCH_PARSE_CACHE_MAX_ENTRY_CHARS);
    const first = parsePatchFilesCached(atCap);
    expect(parsePatchFilesCached(atCap)).toBe(first);
    expect(__parsePatchCacheStatsForTest().entries).toBe(1);
  });

  it('caches the empty string without error', () => {
    const first = parsePatchFilesCached('');
    expect(first).toEqual(parsePatchFiles(''));
    expect(parsePatchFilesCached('')).toBe(first);
    expect(__parsePatchCacheStatsForTest().entries).toBe(1);
  });

  it('returns the same parsed instance for repeated same-content calls', () => {
    const patch = patchOfSize('cached.ts', 200);
    const first = parsePatchFilesCached(patch);
    const second = parsePatchFilesCached(patch);
    expect(second).toBe(first);
    // And it still parses correctly — same result as the uncached path.
    expect(first).toEqual(parsePatchFiles(patch));
    expect(first[0]?.path).toBe('cached.ts');
  });

  it('bypasses the cache for inputs over the per-entry size cap', () => {
    const huge = patchOfSize('huge.ts', PATCH_PARSE_CACHE_MAX_ENTRY_CHARS + 1024);
    const first = parsePatchFilesCached(huge);
    const second = parsePatchFilesCached(huge);
    expect(second).not.toBe(first);
    expect(second).toEqual(first);
  });

  it('evicts least-recently-used entries once the char budget is exceeded', () => {
    const entryChars = Math.floor(PATCH_PARSE_CACHE_MAX_ENTRY_CHARS * 0.8);
    const a = patchOfSize('a.ts', entryChars);
    const b = patchOfSize('b.ts', entryChars);
    const aFirst = parsePatchFilesCached(a);
    const bFirst = parsePatchFilesCached(b);
    // Touch `a` so `b` becomes the least-recently-used entry.
    expect(parsePatchFilesCached(a)).toBe(aFirst);

    // Fill until the budget forces eviction of exactly the LRU tail:
    // 2×0.8 + 3×~0.9 entry-caps ≈ 4.3 quarters of the budget > 4.
    // Fillers sit safely UNDER the per-entry cap so they cache (and
    // evict) rather than taking the oversized-bypass path.
    const fillerChars = PATCH_PARSE_CACHE_MAX_ENTRY_CHARS - 1024;
    const fillers = [
      patchOfSize('fill-0.ts', fillerChars),
      patchOfSize('fill-1.ts', fillerChars),
      patchOfSize('fill-2.ts', fillerChars),
    ];
    const fillerResults = fillers.map((patch) => parsePatchFilesCached(patch));

    // `a` and the newest filler survive with identity intact. Assert
    // these BEFORE re-querying `b`: the `b` miss re-inserts it, which
    // can evict `a` in turn.
    expect(parsePatchFilesCached(a)).toBe(aFirst);
    expect(parsePatchFilesCached(fillers[2])).toBe(fillerResults[2]);
    // `b` (LRU at fill time) was evicted and re-parses fresh.
    const bAfter = parsePatchFilesCached(b);
    expect(bAfter).not.toBe(bFirst);
    expect(bAfter).toEqual(bFirst);
  });
});

describe('hunk gap rows', () => {
  // Hunk 1 nets +2, so hunk 2's new side runs 2 ahead of its old side
  // (a real diff's between-gap hides EQUAL old/new line counts).
  const midFilePatch = `diff --git a/app.ts b/app.ts
--- a/app.ts
+++ b/app.ts
@@ -10,2 +10,4 @@ function first()
 ctx1
-old1
+new1
+extra1
+extra2
@@ -40,2 +42,2 @@ function second()
 ctx2
-old2
+new2
`;

  function gapsOf(rows: ReturnType<typeof buildPatchDisplayRows>) {
    return rows.filter((row) => row.gap).map((row) => row.gap!);
  }

  it('emits leading, between, and trailing gaps for a mid-file diff', () => {
    const [file] = parsePatchFiles(midFilePatch);
    const gaps = gapsOf(buildPatchDisplayRows(file.lines));
    expect(gaps).toEqual([
      expect.objectContaining({ location: 'leading', startNew: 1, endNew: 9, hidden: 9 }),
      expect.objectContaining({ location: 'between', startNew: 14, endNew: 41, hidden: 28 }),
      expect.objectContaining({ location: 'trailing', startNew: 44, endNew: -1, hidden: -1 }),
    ]);
  });

  it('gap rows render as blank context lines for non-gap-aware consumers', () => {
    const [file] = parsePatchFiles(midFilePatch);
    const gapRow = buildPatchDisplayRows(file.lines).find((row) => row.gap);
    expect(gapRow).toMatchObject({
      line: { content: '', type: 'context' },
      oldLine: 0,
      newLine: 0,
      side: 'context',
    });
  });

  it('sizes the trailing gap once the new-side total is known', () => {
    const [file] = parsePatchFiles(midFilePatch);
    const gaps = gapsOf(buildPatchDisplayRows(file.lines, 50));
    expect(gaps.at(-1)).toMatchObject({ location: 'trailing', startNew: 44, endNew: 50, hidden: 7 });
  });

  it('retires the trailing gap when the last hunk reaches EOF', () => {
    const [file] = parsePatchFiles(midFilePatch);
    const gaps = gapsOf(buildPatchDisplayRows(file.lines, 43));
    expect(gaps.map((gap) => gap.location)).toEqual(['leading', 'between']);
  });

  it('emits no gaps for added or deleted files', () => {
    const [added] = parsePatchFiles(`diff --git a/new.ts b/new.ts
new file mode 100644
--- /dev/null
+++ b/new.ts
@@ -0,0 +1,3 @@
+a
+b
+c
`);
    expect(gapsOf(buildPatchDisplayRows(added.lines))).toEqual([]);

    const [deleted] = parsePatchFiles(`diff --git a/gone.ts b/gone.ts
deleted file mode 100644
--- a/gone.ts
+++ /dev/null
@@ -1,3 +0,0 @@
-a
-b
-c
`);
    expect(gapsOf(buildPatchDisplayRows(deleted.lines))).toEqual([]);
  });

  it('suppresses gaps in conflict pseudo-files', () => {
    // Conflict pseudo-files carry marker/fold rows and represent hidden
    // runs as their own folds — hunk gaps would double up.
    const lines = [
      { content: '@@ -10,3 +10,3 @@', type: 'meta' as const },
      { content: '<<<<<<< ours', type: 'marker' as const },
      { content: '-ours line', type: 'del' as const },
      { content: '+theirs line', type: 'add' as const },
      { content: '>>>>>>> theirs', type: 'marker' as const },
    ];
    expect(gapsOf(buildPatchDisplayRows(lines))).toEqual([]);
  });

  it('mirrors gap rows across both split-view sides', () => {
    const [file] = parsePatchFiles(midFilePatch);
    const splitRows = buildSplitDisplayRows(buildPatchDisplayRows(file.lines));
    const gapPair = splitRows.find((pair) => pair.left?.gap);
    expect(gapPair?.right).toBe(gapPair?.left);
  });
});

describe('mergePatchFilesByPath', () => {
  function section(path: string, opts: { adds?: number; dels?: number; kind?: string } = {}) {
    const patch = [
      `diff --git a/${path} b/${path}`,
      ...(opts.kind === 'added' ? ['new file mode 100644'] : []),
      ...(opts.kind === 'deleted' ? ['deleted file mode 100644'] : []),
      `--- ${opts.kind === 'added' ? '/dev/null' : `a/${path}`}`,
      `+++ ${opts.kind === 'deleted' ? '/dev/null' : `b/${path}`}`,
      '@@ -1,2 +1,2 @@',
      ...Array.from({ length: opts.dels ?? 1 }, (_, i) => `-old ${i}`),
      ...Array.from({ length: opts.adds ?? 1 }, (_, i) => `+new ${i}`),
    ].join('\n');
    const [file] = parsePatchFiles(patch);
    return file;
  }

  function sectionFrom(lines: string[]) {
    const [file] = parsePatchFiles(lines.join('\n'));
    return file;
  }

  it('renumbers disjoint sections into one file-ordered section', () => {
    // Edit 1 replaces a line at 10-12; edit 2 (later, ABOVE it) inserts
    // a line at 4, shifting edit 1's real position down by one in the
    // final file.
    const first = sectionFrom([
      'diff --git a/a.ts b/a.ts',
      '--- a/a.ts',
      '+++ b/a.ts',
      '@@ -10,3 +10,3 @@ function outer() {',
      ' ctx1',
      '-old',
      '+new',
      ' ctx2',
    ]);
    const second = sectionFrom([
      'diff --git a/a.ts b/a.ts',
      '--- a/a.ts',
      '+++ b/a.ts',
      '@@ -3,2 +3,3 @@',
      ' top',
      '+inserted',
      ' bottom',
    ]);
    const merged = mergePatchFilesByPath([first, second]);

    expect(merged).toHaveLength(1);
    // One coherent section: single meta block, hunks interleaved in
    // final-file order, the earlier edit renumbered by the later
    // insertion's net delta. Header suffixes survive the rewrite.
    expect(merged[0].lines.map((line) => line.content)).toEqual([
      'diff --git a/a.ts b/a.ts',
      '--- a/a.ts',
      '+++ b/a.ts',
      '@@ -3,2 +3,3 @@',
      ' top',
      '+inserted',
      ' bottom',
      '@@ -11,3 +11,3 @@ function outer() {',
      ' ctx1',
      '-old',
      '+new',
      ' ctx2',
    ]);
    expect(merged[0].suppressGaps).toBeUndefined();
    expect(merged[0].additions).toBe(2);
    expect(merged[0].deletions).toBe(1);
    // Shared parse results are never mutated.
    expect(first.lines.map((line) => line.content)).toContain('@@ -10,3 +10,3 @@ function outer() {');
  });

  it('keeps positions for a later section editing below', () => {
    const first = sectionFrom([
      'diff --git a/a.ts b/a.ts',
      '--- a/a.ts',
      '+++ b/a.ts',
      '@@ -5,3 +5,3 @@',
      ' ctx',
      '-old',
      '+new',
      ' ctx2',
    ]);
    const second = sectionFrom([
      'diff --git a/a.ts b/a.ts',
      '--- a/a.ts',
      '+++ b/a.ts',
      '@@ -9,2 +9,3 @@',
      ' ctx3',
      '+later',
      ' ctx4',
    ]);
    const merged = mergePatchFilesByPath([first, second]);
    const contents = merged[0].lines.map((line) => line.content);
    expect(contents.filter((content) => content.startsWith('@@'))).toEqual([
      '@@ -5,3 +5,3 @@',
      '@@ -9,2 +9,3 @@',
    ]);
    expect(merged[0].suppressGaps).toBeUndefined();
  });

  it('accumulates shifts across three chronological sections', () => {
    const bottom = sectionFrom([
      'diff --git a/a.ts b/a.ts',
      '--- a/a.ts',
      '+++ b/a.ts',
      '@@ -30,2 +30,2 @@',
      ' c1',
      '-b old',
      '+b new',
    ]);
    const middle = sectionFrom([
      'diff --git a/a.ts b/a.ts',
      '--- a/a.ts',
      '+++ b/a.ts',
      '@@ -10,1 +10,2 @@',
      ' c2',
      '+m add',
    ]);
    const top = sectionFrom([
      'diff --git a/a.ts b/a.ts',
      '--- a/a.ts',
      '+++ b/a.ts',
      '@@ -3,1 +3,3 @@',
      ' c3',
      '+t add 1',
      '+t add 2',
    ]);
    const merged = mergePatchFilesByPath([bottom, middle, top]);
    // bottom shifts +1 (middle) then +2 (top); middle shifts +2 (top).
    expect(merged[0].lines.map((line) => line.content).filter((content) => content.startsWith('@@'))).toEqual([
      '@@ -3,1 +3,3 @@',
      '@@ -12,1 +12,2 @@',
      '@@ -33,2 +33,2 @@',
    ]);
  });

  it('places an insertion immediately after an earlier edit without shifting it', () => {
    // git's `-N,0` convention: a zero-old-count hunk inserts AFTER old
    // line N. Inserting right after a line an earlier section modified
    // must land BELOW it, not shift it.
    const modified = sectionFrom([
      'diff --git a/a.ts b/a.ts',
      '--- a/a.ts',
      '+++ b/a.ts',
      '@@ -2,1 +2,1 @@',
      '-old b',
      '+new b',
    ]);
    const inserted = sectionFrom([
      'diff --git a/a.ts b/a.ts',
      '--- a/a.ts',
      '+++ b/a.ts',
      '@@ -2,0 +3,1 @@',
      '+after b',
    ]);
    const merged = mergePatchFilesByPath([modified, inserted]);
    expect(merged[0].suppressGaps).toBeUndefined();
    expect(merged[0].lines.map((line) => line.content).filter((content) => content.startsWith('@@'))).toEqual([
      '@@ -2,1 +2,1 @@',
      '@@ -2,0 +3,1 @@',
    ]);
  });

  it('shifts an earlier hunk up when a later section deletes lines above it', () => {
    const below = sectionFrom([
      'diff --git a/a.ts b/a.ts',
      '--- a/a.ts',
      '+++ b/a.ts',
      '@@ -30,2 +30,2 @@',
      ' ctx',
      '-x',
      '+y',
    ]);
    const deletion = sectionFrom([
      'diff --git a/a.ts b/a.ts',
      '--- a/a.ts',
      '+++ b/a.ts',
      '@@ -10,3 +10,1 @@',
      ' ctx2',
      '-gone 1',
      '-gone 2',
    ]);
    const merged = mergePatchFilesByPath([below, deletion]);
    // Net -2 above: the earlier hunk's final position moves UP.
    expect(merged[0].lines.map((line) => line.content).filter((content) => content.startsWith('@@'))).toEqual([
      '@@ -10,3 +10,1 @@',
      '@@ -28,2 +28,2 @@',
    ]);
  });

  it('pins adjacency: a hunk touching-above shifts, touching-below does not', () => {
    // Earlier hunk occupies new lines [10, 13). The later section's
    // first hunk ends exactly at 10 (oldEnd == pos: still "above", so
    // it shifts); its second starts exactly at 13 (oldStart ==
    // pos + newCount: "below", no shift, no overlap bail).
    const earlier = sectionFrom([
      'diff --git a/a.ts b/a.ts',
      '--- a/a.ts',
      '+++ b/a.ts',
      '@@ -10,3 +10,3 @@',
      ' c1',
      '-a',
      '+b',
      ' c2',
    ]);
    const touching = sectionFrom([
      'diff --git a/a.ts b/a.ts',
      '--- a/a.ts',
      '+++ b/a.ts',
      '@@ -7,3 +7,4 @@',
      ' t',
      '-u',
      '+v',
      '+w',
      ' z',
      '@@ -13,2 +13,2 @@',
      ' c3',
      '-p',
      '+q',
    ]);
    const merged = mergePatchFilesByPath([earlier, touching]);
    expect(merged[0].suppressGaps).toBeUndefined();
    expect(merged[0].lines.map((line) => line.content).filter((content) => content.startsWith('@@'))).toEqual([
      '@@ -7,3 +7,4 @@',
      '@@ -11,3 +11,3 @@',
      '@@ -13,2 +13,2 @@',
    ]);
  });

  it('falls back to suppressed-gap concatenation when sections overlap', () => {
    const a1 = section('a.go', { adds: 2, dels: 1 });
    const b = section('b.go');
    const a2 = section('a.go', { adds: 1, dels: 3 });
    const merged = mergePatchFilesByPath([a1, b, a2]);

    expect(merged.map((file) => file.path)).toEqual(['a.go', 'b.go']);
    expect(merged[0].additions).toBe(3);
    expect(merged[0].deletions).toBe(4);
    // Overlapping regions can't be renumbered: both sections' rows
    // render as consecutive hunks in edit order, and gap rows retire
    // (their coordinates describe different moments of the file).
    expect(merged[0].lines).toEqual([...a1.lines, ...a2.lines]);
    expect(merged[0].suppressGaps).toBe(true);
    // Unmerged files pass through by identity.
    expect(merged[1]).toBe(b);
    expect(merged[1].suppressGaps).toBeUndefined();
    // Shared parse results are never mutated.
    expect(a1.additions).toBe(2);
    expect(a1.lines.length).toBeLessThan(merged[0].lines.length);
  });

  it('concatenates when a section edits a file created in the same turn', () => {
    const created = section('a.go', { kind: 'added' });
    const edited = section('a.go');
    const merged = mergePatchFilesByPath([created, edited]);
    expect(merged[0].kind).toBe('added');
    expect(merged[0].suppressGaps).toBe(true);
    expect(merged[0].lines).toEqual([...created.lines, ...edited.lines]);
  });

  it('keeps the first section kind unless a later section deletes the file', () => {
    const added = mergePatchFilesByPath([section('a.go', { kind: 'added' }), section('a.go')]);
    expect(added[0].kind).toBe('added');

    const deleted = mergePatchFilesByPath([section('a.go'), section('a.go', { kind: 'deleted' })]);
    expect(deleted[0].kind).toBe('deleted');
  });

  it('is identity-preserving when no path repeats', () => {
    const a = section('a.go');
    const b = section('b.go');
    expect(mergePatchFilesByPath([a, b])).toEqual([a, b]);
  });

  it('mergePatchFilesByPathCached returns the identical array per input identity', () => {
    const parsed = parsePatchFiles(
      [
        'diff --git a/a.ts b/a.ts',
        '--- a/a.ts',
        '+++ b/a.ts',
        '@@ -5,2 +5,2 @@',
        ' ctx',
        '-old',
        '+new',
        'diff --git a/a.ts b/a.ts',
        '--- a/a.ts',
        '+++ b/a.ts',
        '@@ -9,1 +9,2 @@',
        ' ctx2',
        '+later',
      ].join('\n'),
    );
    const mergedOnce = mergePatchFilesByPathCached(parsed);
    // Identity, not just equality: downstream memos (expansion rebuild
    // cache, span-cache predecessor chains) key on the lines array.
    expect(mergePatchFilesByPathCached(parsed)).toBe(mergedOnce);
    // A different input array (same content) misses — the memo is
    // keyed on the parse-cache result's identity.
    expect(mergePatchFilesByPathCached([...parsed])).not.toBe(mergedOnce);
  });
});
