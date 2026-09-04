import { beforeEach, describe, expect, it } from 'vitest';
import {
  PATCH_PARSE_CACHE_MAX_ENTRY_CHARS,
  __parsePatchCacheStatsForTest,
  __resetParsePatchCacheForTest,
  buildPatchDisplayRows,
  buildSplitDisplayRows,
  extractPatchFile,
  filePatchDisplayRows,
  mergePatchFilesByPath,
  mergePatchFilesByPathCached,
  parsePatchFileSummaries,
  parsePatchFiles,
  parsePatchFilesCached,
  patchFileRowId,
  stripPatchLinePrefix,
} from './patchFiles';

describe('parsePatchFiles', () => {
  it('builds file summaries without retaining hunk lines', () => {
    const summaries = parsePatchFileSummaries(`diff --git a/app.ts b/app.ts
--- a/app.ts
+++ b/app.ts
@@ -1 +1,2 @@
-old
+new
+added
diff --git a/new.ts b/new.ts
new file mode 100644
--- /dev/null
+++ b/new.ts
@@ -0,0 +1 @@
+created
`);

    expect(summaries).toMatchObject([
      { path: 'app.ts', kind: 'modified', additions: 2, deletions: 1, lines: [] },
      { path: 'new.ts', kind: 'added', additions: 1, deletions: 0, lines: [] },
    ]);
  });

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

  // git emits a file→symlink (or symlink→file) type change as two
  // adjacent same-path sections: the old form deleted, the new form
  // created. MR !309 (apps/demo/CLAUDE.md → symlink) crashed the review
  // pane's path-keyed file tree on the duplicate (2026-09-04).
  const typeChangePatch = `diff --git a/apps/demo/CLAUDE.md b/apps/demo/CLAUDE.md
deleted file mode 100644
index 596b365..0000000
--- a/apps/demo/CLAUDE.md
+++ /dev/null
@@ -1,2 +0,0 @@
-# Demo
-Old guide.
diff --git a/apps/demo/CLAUDE.md b/apps/demo/CLAUDE.md
new file mode 120000
index 0000000..47dc3e3
--- /dev/null
+++ b/apps/demo/CLAUDE.md
@@ -0,0 +1 @@
+AGENTS.md
diff --git a/other.ts b/other.ts
--- a/other.ts
+++ b/other.ts
@@ -1 +1 @@
-a
+b
`;

  it('folds a type change (file ↔ symlink) into one modified file', () => {
    const files = parsePatchFiles(typeChangePatch);
    expect(files.map((file) => file.path)).toEqual(['apps/demo/CLAUDE.md', 'other.ts']);
    const merged = files[0]!;
    expect(merged).toMatchObject({ kind: 'modified', additions: 1, deletions: 2, suppressGaps: true });
    // One preamble (the deletion's), both hunks, the creation's header
    // block dropped.
    expect(merged.lines.filter((line) => line.content.startsWith('diff --git'))).toHaveLength(1);
    expect(merged.lines.filter((line) => line.content.startsWith('@@')).map((line) => line.content)).toEqual([
      '@@ -1,2 +0,0 @@',
      '@@ -0,0 +1 @@',
    ]);
    // Old content numbers on the old side, the symlink target on the
    // new side, and no synthetic gap row between the two hunks.
    const rows = filePatchDisplayRows(merged);
    expect(rows.some((row) => row.gap)).toBe(false);
    expect(rows.map((row) => [row.side, row.oldLine, row.newLine])).toEqual([
      ['old', 1, 0],
      ['old', 2, 0],
      ['new', 0, 1],
    ]);
    expect(parsePatchFileSummaries(typeChangePatch)).toMatchObject([
      { path: 'apps/demo/CLAUDE.md', kind: 'modified', additions: 1, deletions: 2, lines: [] },
      { path: 'other.ts', kind: 'modified' },
    ]);
  });

  it('does not fold a deletion followed by an unrelated re-creation of another path', () => {
    const files = parsePatchFiles(`diff --git a/a.ts b/a.ts
deleted file mode 100644
--- a/a.ts
+++ /dev/null
@@ -1 +0,0 @@
-a
diff --git a/b.ts b/b.ts
new file mode 100644
--- /dev/null
+++ b/b.ts
@@ -0,0 +1 @@
+b
`);
    expect(files.map((file) => [file.path, file.kind])).toEqual([
      ['a.ts', 'deleted'],
      ['b.ts', 'added'],
    ]);
  });

  it('extracts both sections of a type change as that file’s patch', () => {
    const extracted = extractPatchFile(typeChangePatch, 'apps/demo/CLAUDE.md');
    expect(extracted).not.toBeNull();
    expect(extracted!.match(/^diff --git /gm)).toHaveLength(2);
    expect(extracted).toContain('+AGENTS.md');
    expect(extracted).not.toContain('other.ts');
    expect(extractPatchFile(typeChangePatch, 'other.ts')).toContain('+b');
  });

  it('treats the no-newline marker as metadata, not a numbered line', () => {
    const file = parsePatchFiles(`diff --git a/a.ts b/a.ts
--- a/a.ts
+++ b/a.ts
@@ -1 +1 @@
-old
\\ No newline at end of file
+new
\\ No newline at end of file
`)[0]!;
    expect(file).toMatchObject({ additions: 1, deletions: 1 });
    expect(file.lines.filter((line) => line.content.startsWith('\\')).map((line) => line.type)).toEqual([
      'meta',
      'meta',
    ]);
    // Before: the marker took old 2 / new 1 as a context row and `+new`
    // landed on new line 2.
    const rows = filePatchDisplayRows(file).filter((row) => !row.gap);
    expect(rows.map((row) => [row.line.type, row.oldLine, row.newLine])).toEqual([
      ['del', 1, 0],
      ['add', 0, 1],
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

  it('composes a created-then-edited file into one final-content added section', () => {
    const created = sectionFrom([
      'diff --git a/z.ts b/z.ts',
      'new file mode 100644',
      '--- /dev/null',
      '+++ b/z.ts',
      '@@ -0,0 +1,4 @@',
      '+alpha',
      '+beta',
      '+gamma',
      '+delta',
    ]);
    const edited = sectionFrom([
      'diff --git a/z.ts b/z.ts',
      '--- a/z.ts',
      '+++ b/z.ts',
      '@@ -2,2 +2,3 @@',
      ' beta',
      '-gamma',
      '+gamma prime',
      '+gamma extra',
    ]);
    const prepended = sectionFrom([
      'diff --git a/z.ts b/z.ts',
      '--- a/z.ts',
      '+++ b/z.ts',
      '@@ -0,0 +1,1 @@',
      '+header',
    ]);
    const merged = mergePatchFilesByPath([created, edited, prepended]);

    // Real composition: later hunks applied to the creation content,
    // emitted as ONE clean added-file section of end-of-turn content.
    expect(merged).toHaveLength(1);
    expect(merged[0].kind).toBe('added');
    expect(merged[0].additions).toBe(6);
    expect(merged[0].deletions).toBe(0);
    expect(merged[0].suppressGaps).toBeUndefined();
    expect(merged[0].lines.map((line) => line.content)).toEqual([
      'diff --git a/z.ts b/z.ts',
      'new file mode 100644',
      '--- /dev/null',
      '+++ b/z.ts',
      '@@ -0,0 +1,6 @@',
      '+header',
      '+alpha',
      '+beta',
      '+gamma prime',
      '+gamma extra',
      '+delta',
    ]);
    // An added file is fully present — no gap rows to expand.
    expect(filePatchDisplayRows(merged[0]).some((row) => row.gap)).toBe(false);
    // Shared parse results are never mutated.
    expect(created.lines.map((line) => line.content)).toContain('@@ -0,0 +1,4 @@');
  });

  it('composes a multi-hunk later section with cumulative offsets', () => {
    // One MultiEdit payload = one section with several hunks; later
    // hunks' coordinates must shift by the earlier hunks' net delta
    // within the same section.
    const created = sectionFrom([
      'diff --git a/z.ts b/z.ts',
      'new file mode 100644',
      '--- /dev/null',
      '+++ b/z.ts',
      '@@ -0,0 +1,6 @@',
      '+l1',
      '+l2',
      '+l3',
      '+l4',
      '+l5',
      '+l6',
    ]);
    const multiEdit = sectionFrom([
      'diff --git a/z.ts b/z.ts',
      '--- a/z.ts',
      '+++ b/z.ts',
      '@@ -2,1 +2,3 @@',
      '-l2',
      '+l2a',
      '+l2b',
      '+l2c',
      '@@ -5,2 +7,1 @@',
      ' l5',
      '-l6',
    ]);
    const merged = mergePatchFilesByPath([created, multiEdit]);
    expect(merged[0].suppressGaps).toBeUndefined();
    expect(merged[0].lines.map((line) => line.content).slice(4)).toEqual([
      '@@ -0,0 +1,7 @@',
      '+l1',
      '+l2a',
      '+l2b',
      '+l2c',
      '+l3',
      '+l4',
      '+l5',
    ]);
  });

  it('falls back to concatenation when an edit mismatches the created content', () => {
    const created = sectionFrom([
      'diff --git a/z.ts b/z.ts',
      'new file mode 100644',
      '--- /dev/null',
      '+++ b/z.ts',
      '@@ -0,0 +1,2 @@',
      '+alpha',
      '+beta',
    ]);
    const edited = sectionFrom([
      'diff --git a/z.ts b/z.ts',
      '--- a/z.ts',
      '+++ b/z.ts',
      '@@ -1,1 +1,1 @@',
      '-not alpha',
      '+changed',
    ]);
    const merged = mergePatchFilesByPath([created, edited]);
    // The old side doesn't match what the creation built — composing
    // would fabricate content, so the sections concatenate instead.
    expect(merged[0].kind).toBe('added');
    expect(merged[0].suppressGaps).toBe(true);
    expect(merged[0].lines).toEqual([...created.lines, ...edited.lines]);
  });

  it('falls back when the created file is later deleted', () => {
    const created = sectionFrom([
      'diff --git a/z.ts b/z.ts',
      'new file mode 100644',
      '--- /dev/null',
      '+++ b/z.ts',
      '@@ -0,0 +1,2 @@',
      '+alpha',
      '+beta',
    ]);
    const deleted = sectionFrom([
      'diff --git a/z.ts b/z.ts',
      'deleted file mode 100644',
      '--- a/z.ts',
      '+++ /dev/null',
      '@@ -1,2 +0,0 @@',
      '-alpha',
      '-beta',
    ]);
    const merged = mergePatchFilesByPath([created, deleted]);
    // The end state isn't an added file; deletion wins the kind and the
    // sections render in edit order.
    expect(merged[0].kind).toBe('deleted');
    expect(merged[0].suppressGaps).toBe(true);
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
