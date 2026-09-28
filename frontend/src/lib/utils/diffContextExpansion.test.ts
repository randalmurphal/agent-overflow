import { afterEach, describe, expect, it } from 'vitest';
import {
  applyContextExpansion,
  DIFF_CONTEXT_EXPAND_STEP,
  expansionFetchRange,
  readHunkHeadings,
  type ContextExpansionState,
} from './diffContextExpansion';
import type { DiffGap, PatchDisplayRow } from './patchFiles';
import { displayRowCount, FILE_START, materializeRows } from './patchRows';
import { parseReviewFiles, reviewFileFromPatchFile, type ReviewFile } from './patchStore';
import { setPatchTextBudgetForTest } from './patchMemory.svelte';
import { streamPatch } from '../../test/helpers/streamedPatch';

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

function fileOf(patch: string): ReviewFile {
  return parseReviewFiles(patch)[0];
}

function rowsOf(file: ReviewFile): PatchDisplayRow[] {
  return materializeRows(file, FILE_START, displayRowCount(file)).rows;
}

function gapsOf(file: ReviewFile): DiffGap[] {
  return rowsOf(file)
    .filter((row) => row.gap)
    .map((row) => row.gap!);
}

function contents(file: ReviewFile): string[] {
  return file.body.toPatchLines().map((line) => line.content);
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

    const rows = rowsOf(expanded);
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

    const rows = rowsOf(expanded);
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

    const rows = rowsOf(expanded);
    const merged = rows.filter((row) => row.line.content.startsWith(' src '));
    expect(merged).toHaveLength(28);
    expect(merged[0]).toMatchObject({ oldLine: 12, newLine: 14 });
    expect(merged.at(-1)).toMatchObject({ oldLine: 39, newLine: 41 });
    expect(gapsOf(expanded).map((gap) => gap.location)).toEqual(['leading', 'trailing']);
  });

  it('prepends a leading run to the first hunk', () => {
    const file = fileOf(midFilePatch);
    const expanded = applyContextExpansion(file, state(range(1, 9)));

    const rows = rowsOf(expanded);
    expect(rows.find((row) => row.newLine === 1)?.line.content).toBe(' src 1');
    expect(gapsOf(expanded).map((gap) => gap.location)).toEqual(['between', 'trailing']);
  });

  it('learns the file length from an EOF response and retires the trailing gap', () => {
    const file = fileOf(midFilePatch);
    const expanded = applyContextExpansion(file, state(range(44, 50), 50));

    expect(expanded.newSideTotal).toBe(50);
    const rows = rowsOf(expanded);
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

  it('keeps two expansion states on the same base array fully independent', () => {
    // Two panes expanding identical content hit applyContextExpansion
    // with the SAME base file but different states. Each state must keep
    // its own memo slot (no rebuild ping-pong).
    const file = fileOf(midFilePatch);
    const paneA = state(range(14, 16), null, 21);
    const paneB = state(range(37, 39), null, 22);

    const a1 = applyContextExpansion(file, paneA);
    const b1 = applyContextExpansion(file, paneB);
    // Interleaved re-application returns the memoized files unchanged.
    expect(applyContextExpansion(file, paneA)).toBe(a1);
    expect(applyContextExpansion(file, paneB)).toBe(b1);

    paneA.lines.set(17, 'src 17');
    paneA.version = 23;
    const a2 = applyContextExpansion(file, paneA);
    expect(a2).not.toBe(a1);
    expect(applyContextExpansion(file, paneB)).toBe(b1);
  });

  it('never serves a build of the original to a copy with other file fields', () => {
    // The edits scope retires a refused file's gaps by copying it with
    // suppressGaps. The copy shares the original's lines, so a memo hit
    // on lines alone would hand back the original's build, arrows and
    // all.
    const file = fileOf(midFilePatch);
    const expansion = state(range(14, 16), null, 41);
    const expanded = applyContextExpansion(file, expansion);
    expect(rowsOf(expanded).some((row) => row.gap)).toBe(true);

    const retired = applyContextExpansion({ ...file, suppressGaps: true }, expansion);
    expect(retired.suppressGaps).toBe(true);
    expect(contents(retired)).toEqual(contents(expanded));
    expect(rowsOf(retired).some((row) => row.gap)).toBe(false);
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

    const conflict = reviewFileFromPatchFile({
      path: 'x',
      kind: 'conflict',
      additions: 0,
      deletions: 0,
      lines: [{ content: '<<<<<<<', type: 'marker' }],
    });
    expect(applyContextExpansion(conflict, state(range(1, 2), null, 2))).toBe(conflict);
  });

  it('keeps the original lines, preamble and trailing markers in place', () => {
    const file = fileOf(`diff --git a/app.ts b/app.ts
index 1111111..2222222 100644
--- a/app.ts
+++ b/app.ts
@@ -10,2 +10,2 @@ function first()
 ctx1
-old1
+new1
\\ No newline at end of file
`);
    const expanded = applyContextExpansion(file, state([...range(7, 9), ...range(12, 13)]));
    expect(contents(expanded)).toEqual([
      'diff --git a/app.ts b/app.ts',
      'index 1111111..2222222 100644',
      '--- a/app.ts',
      '+++ b/app.ts',
      '@@ -7,7 +7,7 @@ function first()',
      ' src 7',
      ' src 8',
      ' src 9',
      ' ctx1',
      '-old1',
      '+new1',
      '\\ No newline at end of file',
      ' src 12',
      ' src 13',
    ]);
    expect(expanded.body.patchText()).toBe(contents(expanded).join('\n'));
    // The original lines are read from the parsed store, not copied.
    expect(expanded.body.segments.some((segment) => segment.store === file.body.segments[0].store)).toBe(true);
  });
});

describe('applyContextExpansion over evicted text', () => {
  afterEach(() => setPatchTextBudgetForTest(null));

  it('builds from the headings read with the expansion, whatever text is resident', async () => {
    const padding = `diff --git a/pad.ts b/pad.ts\n--- a/pad.ts\n+++ b/pad.ts\n@@ -1,0 +1,1 @@\n+${'p'.repeat(400)}\n`;
    setPatchTextBudgetForTest(420);
    const { parser, files } = streamPatch(padding + midFilePatch, 30);
    try {
      const file = files.find((candidate) => candidate.path === 'app.ts')!;
      expect(file.body.resident()).toBe(false);
      const headings = await readHunkHeadings(file.body);
      expect([...headings]).toEqual([[3, ' function first()'], [9, ' function second()']]);

      const expanded = applyContextExpansion(file, { ...state(range(14, 16)), headings });
      const expected = applyContextExpansion(fileOf(midFilePatch), state(range(14, 16)));
      expect(await expanded.body.whenResident(() => contents(expanded))).toEqual(contents(expected));
      expect(expanded.body.text(3)).toBe('@@ -10,5 +10,7 @@ function first()');
    } finally {
      parser.store.dispose();
    }
  });
});
