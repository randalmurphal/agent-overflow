import { describe, expect, it } from 'vitest';
import {
  buildPatchDisplayRows,
  buildSplitDisplayRows,
  extractPatchFile,
  parsePatchFiles,
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
