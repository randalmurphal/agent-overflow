import { describe, expect, it } from 'vitest';
import {
  applyContextExpansion,
  DIFF_CONTEXT_EXPAND_STEP,
  expansionFetchRange,
  type ContextExpansionState,
} from './diffContextExpansion';
import { buildPatchDisplayRows, parsePatchFiles, type DiffGap, type PatchFile } from './patchFiles';

// Two hunks with a known between-gap (new-side 14..41), a leading gap
// (1..9), and an unknown-size trailing gap starting at 44. Hunk 1 nets
// +2, so old lines run 2 behind new lines from there down (the second
// hunk sits at old 40 / new 42).
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

function fileOf(patch: string): PatchFile {
  return parsePatchFiles(patch)[0];
}

function gapsOf(file: PatchFile): DiffGap[] {
  return buildPatchDisplayRows(file.lines, file.newSideTotal)
    .filter((row) => row.gap)
    .map((row) => row.gap!);
}

function state(entries: [number, string][], eofLine: number | null = null, version = 1): ContextExpansionState {
  return { lines: new Map(entries), eofLine, version };
}

function range(startNew: number, endNew: number): [number, string][] {
  const out: [number, string][] = [];
  for (let line = startNew; line <= endNew; line += 1) out.push([line, `src ${line}`]);
  return out;
}

describe('expansionFetchRange', () => {
  const between: DiffGap = { id: 1, startNew: 14, endNew: 41, hidden: 28, location: 'between' };
  const trailing: DiffGap = { id: 2, startNew: 44, endNew: -1, hidden: -1, location: 'trailing' };
  const small: DiffGap = { id: 0, startNew: 1, endNew: 9, hidden: 9, location: 'leading' };

  it('steps down from the gap top and up from the gap bottom', () => {
    expect(expansionFetchRange(between, 'down')).toEqual({ start: 14, end: 14 + DIFF_CONTEXT_EXPAND_STEP - 1 });
    expect(expansionFetchRange(between, 'up')).toEqual({ start: 41 - DIFF_CONTEXT_EXPAND_STEP + 1, end: 41 });
    expect(expansionFetchRange(between, 'all')).toEqual({ start: 14, end: 41 });
  });

  it('clamps steps to the gap bounds', () => {
    expect(expansionFetchRange(small, 'down')).toEqual({ start: 1, end: 9 });
    expect(expansionFetchRange(small, 'up')).toEqual({ start: 1, end: 9 });
  });

  it('handles unknown-size trailing gaps: down steps, up/all are unaddressable', () => {
    expect(expansionFetchRange(trailing, 'down')).toEqual({ start: 44, end: 44 + DIFF_CONTEXT_EXPAND_STEP - 1 });
    expect(expansionFetchRange(trailing, 'up')).toBeNull();
    expect(expansionFetchRange(trailing, 'all')).toBeNull();
  });
});

describe('applyContextExpansion', () => {
  it('returns the input file untouched when there is nothing to apply', () => {
    const file = fileOf(midFilePatch);
    expect(applyContextExpansion(file, undefined)).toBe(file);
    expect(applyContextExpansion(file, state([]))).toBe(file);
  });

  it('extends the upper hunk downward with a top-anchored run', () => {
    const file = fileOf(midFilePatch);
    const expanded = applyContextExpansion(file, state(range(14, 18)));
    expect(expanded).not.toBe(file);

    const rows = buildPatchDisplayRows(expanded.lines, expanded.newSideTotal);
    const merged = rows.filter((row) => row.line.content.startsWith(' src '));
    // Unchanged lines continue hunk 1's numbering: old runs 2 behind new.
    expect(merged.map((row) => [row.oldLine, row.newLine])).toEqual(
      [[12, 14], [13, 15], [14, 16], [15, 17], [16, 18]],
    );
    const between = gapsOf(expanded).find((gap) => gap.location === 'between');
    expect(between).toMatchObject({ startNew: 19, endNew: 41, hidden: 23 });
  });

  it('extends the lower hunk upward with a bottom-anchored run, shifting its header', () => {
    const file = fileOf(midFilePatch);
    const expanded = applyContextExpansion(file, state(range(37, 41)));

    const rows = buildPatchDisplayRows(expanded.lines, expanded.newSideTotal);
    const merged = rows.filter((row) => row.line.content.startsWith(' src '));
    // Second hunk was old 40 / new 42; prepending 5 lines shifts both.
    expect(merged.map((row) => [row.oldLine, row.newLine])).toEqual(
      [[35, 37], [36, 38], [37, 39], [38, 40], [39, 41]],
    );
    const between = gapsOf(expanded).find((gap) => gap.location === 'between');
    expect(between).toMatchObject({ startNew: 14, endNew: 36, hidden: 23 });
  });

  it('closes a fully fetched between-gap without duplicating lines', () => {
    const file = fileOf(midFilePatch);
    const expanded = applyContextExpansion(file, state(range(14, 41)));

    const rows = buildPatchDisplayRows(expanded.lines, expanded.newSideTotal);
    const merged = rows.filter((row) => row.line.content.startsWith(' src '));
    expect(merged).toHaveLength(28);
    expect(merged[0]).toMatchObject({ oldLine: 12, newLine: 14 });
    expect(merged.at(-1)).toMatchObject({ oldLine: 39, newLine: 41 });
    expect(gapsOf(expanded).map((gap) => gap.location)).toEqual(['leading', 'trailing']);
  });

  it('prepends a leading run to the first hunk', () => {
    const file = fileOf(midFilePatch);
    const expanded = applyContextExpansion(file, state(range(1, 9)));

    const rows = buildPatchDisplayRows(expanded.lines, expanded.newSideTotal);
    expect(rows.find((row) => row.newLine === 1)?.line.content).toBe(' src 1');
    expect(gapsOf(expanded).map((gap) => gap.location)).toEqual(['between', 'trailing']);
  });

  it('learns the file length from an EOF response and retires the trailing gap', () => {
    const file = fileOf(midFilePatch);
    const expanded = applyContextExpansion(file, state(range(44, 50), 50));

    expect(expanded.newSideTotal).toBe(50);
    const rows = buildPatchDisplayRows(expanded.lines, expanded.newSideTotal);
    expect(rows.at(-1)).toMatchObject({ oldLine: 48, newLine: 50 });
    expect(gapsOf(expanded).some((gap) => gap.location === 'trailing')).toBe(false);
  });

  it('keeps a stable identity per (file, version) for downstream memos', () => {
    const file = fileOf(midFilePatch);
    const expansion = state(range(14, 18), null, 7);
    const first = applyContextExpansion(file, expansion);
    expect(applyContextExpansion(file, expansion)).toBe(first);

    expansion.lines.set(18, 'src 18');
    expansion.version = 8;
    const second = applyContextExpansion(file, expansion);
    expect(second).not.toBe(first);
  });

  it('leaves added files and conflict pseudo-content untouched', () => {
    const added = fileOf(`diff --git a/new.ts b/new.ts
new file mode 100644
--- /dev/null
+++ b/new.ts
@@ -0,0 +1,2 @@
+a
+b
`);
    expect(applyContextExpansion(added, state(range(1, 2)))).toBe(added);

    const conflict: PatchFile = {
      path: 'x',
      kind: 'conflict',
      additions: 0,
      deletions: 0,
      lines: [{ content: '<<<<<<<', type: 'marker' }],
    };
    expect(applyContextExpansion(conflict, state(range(1, 2), null, 2))).toBe(conflict);
  });
});
