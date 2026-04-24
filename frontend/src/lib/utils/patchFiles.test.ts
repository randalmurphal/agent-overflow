import { describe, expect, it } from 'vitest';
import { buildSplitRows, extractPatchFile, parsePatchFiles } from './patchFiles';

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
    const replacement = buildSplitRows(file.lines).find((row) => row.left?.type === 'del' && row.right?.type === 'add');
    expect(replacement?.left?.content).toBe("-const value = 'old';");
    expect(replacement?.right?.content).toBe("+const value = 'new';");
  });

  it('places added-only rows on the right side', () => {
    const [file] = parsePatchFiles(`diff --git a/app.ts b/app.ts
--- a/app.ts
+++ b/app.ts
@@ -1 +1,2 @@
 const keep = true;
+const added = true;
`);

    const addedOnly = buildSplitRows(file.lines).find((row) => row.right?.content === '+const added = true;');
    expect(addedOnly?.left).toBeNull();
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
});
