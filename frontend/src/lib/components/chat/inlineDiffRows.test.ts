import { describe, expect, it } from 'vitest';
import { parsePatchFiles } from '../../utils/patchFiles';
import { buildInlineDiffRows, buildInlineDiffRowsCached } from './inlineDiffRows';

const PATCH = `diff --git a/app.ts b/app.ts
--- a/app.ts
+++ b/app.ts
@@ -1,3 +1,3 @@
 const keep = true;
-const value = 'old';
+const value = 'new';
 console.log(value);
@@ -10,2 +10,3 @@
 more();
+added();
 tail();
`;

describe('buildInlineDiffRowsCached', () => {
  it('returns the same built rows for the same lines array (remount reuse)', () => {
    const [file] = parsePatchFiles(PATCH);
    const first = buildInlineDiffRowsCached(file.lines);
    const second = buildInlineDiffRowsCached(file.lines);
    expect(second).toBe(first);
    // And the cached result matches the uncached build exactly.
    expect(first).toEqual(buildInlineDiffRows(file.lines));
    expect(first.rows.some((row) => row.kind === 'separator')).toBe(true);
  });

  it('is identity-keyed: an equal-content but distinct lines array rebuilds', () => {
    const [fileA] = parsePatchFiles(PATCH);
    const [fileB] = parsePatchFiles(PATCH);
    const builtA = buildInlineDiffRowsCached(fileA.lines);
    const builtB = buildInlineDiffRowsCached(fileB.lines);
    expect(builtB).not.toBe(builtA);
    expect(builtB).toEqual(builtA);
  });
});
