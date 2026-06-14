import { beforeEach, describe, expect, it } from 'vitest';
import {
  PATCH_PARSE_CACHE_MAX_ENTRY_CHARS,
  __parsePatchCacheStatsForTest,
  __resetParsePatchCacheForTest,
  buildPatchDisplayRows,
  buildSplitDisplayRows,
  extractPatchFile,
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

    const rows = buildPatchDisplayRows(file.lines);

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
