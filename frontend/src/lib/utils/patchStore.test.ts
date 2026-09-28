import { describe, expect, it } from 'vitest';
import { conflictPatchFile } from './conflictFile';
import { diffSourceKey } from './diffSourceKey';
import { contentKey } from './fnv1a';
import {
  buildPatchDisplayRows,
  buildSplitDisplayRows,
  parsePatchFiles,
  type PatchDisplayRow,
  type PatchFile,
} from './patchFiles';
import {
  anchorRow,
  displayRowCount,
  FILE_START,
  materializeRows,
  nearestLineRow,
  RowWalker,
  rowStartAt,
  splitRowCount,
} from './patchRows';
import { PatchParser, parseReviewFiles, reviewFileFromPatchFile, type ReviewFile } from './patchStore';

// The compact store must describe every patch exactly as parsePatchFiles +
// buildPatchDisplayRows do: same files, same lines, same rows, same row ids,
// same intraline pairing, same highlight keys — at any chunking.

const mixed = [
  'preamble line before any file',
  'diff --git a/src/app.ts b/src/app.ts',
  'index 1111111..2222222 100644',
  '--- a/src/app.ts',
  '+++ b/src/app.ts',
  '@@ -10,6 +10,7 @@ export function app() {',
  ' const a = 1;',
  '-const b = 2;',
  '-const c = 3;',
  '+const b = 20;',
  '+const c = 30;',
  '+const d = 40;',
  ' const e = 5;',
  '@@ -40,3 +41,3 @@',
  ' tail();',
  '--- deleted sql comment reads as meta',
  '+++ added line that reads as meta',
  '-old();',
  '+new();',
  '\\ No newline at end of file',
  'diff --git a/docs/readme.md b/docs/renamed.md',
  'similarity index 90%',
  'rename from docs/readme.md',
  'rename to docs/renamed.md',
  'index 3333333..4444444 100644',
  '--- a/docs/readme.md',
  '+++ b/docs/renamed.md',
  '@@ -1,3 +1,3 @@',
  ' # Title',
  '-héllo wörld',
  '+hello world ✓',
  ' end',
  'diff --git a/img.png b/img.png',
  'index 5555555..6666666 100644',
  'Binary files a/img.png and b/img.png differ',
  'diff --git a/new.txt b/new.txt',
  'new file mode 100644',
  'index 0000000..7777777',
  '--- /dev/null',
  '+++ b/new.txt',
  '@@ -0,0 +1,2 @@',
  '+one\r',
  '+two',
  'diff --git a/gone.txt b/gone.txt',
  'deleted file mode 100644',
  'index 8888888..0000000',
  '--- a/gone.txt',
  '+++ /dev/null',
  '@@ -1,2 +0,0 @@',
  '-first',
  '-second',
  'diff --git a/link b/link',
  'deleted file mode 100644',
  'index 9999999..0000000',
  '--- a/link',
  '+++ /dev/null',
  '@@ -1 +0,0 @@',
  '-target',
  '\\ No newline at end of file',
  'diff --git a/link b/link',
  'new file mode 120000',
  'index 0000000..aaaaaaa',
  '--- /dev/null',
  '+++ b/link',
  '@@ -0,0 +1 @@',
  '+other-target',
  'diff --git a/mode.sh b/mode.sh',
  'old mode 100644',
  'new mode 100755',
  'diff --git a/z.go b/z.go',
  '--- a/z.go',
  '+++ b/z.go',
  '@@ -1,2 +1,2 @@',
  '-package z',
  '+package zz',
  ' ',
  '',
  '@@ -20,2 +20,3 @@ func Z() {',
  ' 	return',
  '+	// more',
  ' }',
].join('\n') + '\n';

function randomPatch(seed: number): string {
  let state = seed;
  const rand = (n: number) => {
    state = (Math.imul(state, 1103515245) + 12345) >>> 0;
    return state % n;
  };
  const out: string[] = [];
  const fileCount = 1 + rand(6);
  for (let file = 0; file < fileCount; file += 1) {
    const path = `dir${rand(3)}/file${file}.${['ts', 'go', 'md'][rand(3)]}`;
    out.push(`diff --git a/${path} b/${path}`, `--- a/${path}`, `+++ b/${path}`);
    let oldLine = 1 + rand(5);
    let newLine = oldLine;
    const hunks = 1 + rand(4);
    for (let hunk = 0; hunk < hunks; hunk += 1) {
      const body: string[] = [];
      let oldCount = 0;
      let newCount = 0;
      const lines = 1 + rand(40);
      for (let line = 0; line < lines; line += 1) {
        const pick = rand(4);
        const text = `line ${rand(1000)} ${'x'.repeat(rand(30))}`;
        if (pick === 0) {
          body.push(`-${text}`);
          oldCount += 1;
        } else if (pick === 1) {
          body.push(`+${text}`);
          newCount += 1;
        } else {
          body.push(` ${text}`);
          oldCount += 1;
          newCount += 1;
        }
      }
      out.push(`@@ -${oldLine},${oldCount} +${newLine},${newCount} @@`, ...body);
      const between = rand(8);
      oldLine += oldCount + between;
      newLine += newCount + between;
    }
  }
  return out.join('\n') + (rand(2) === 0 ? '\n' : '');
}

function patchTextOf(file: PatchFile): string {
  return file.lines
    .map((line) => (line.type === 'marker' || line.fold !== undefined ? '\\ marker' : line.content))
    .join('\n');
}

function withoutLineIndex(rows: PatchDisplayRow[]): PatchDisplayRow[] {
  return rows.map(({ lineIndex: _lineIndex, ...row }) => row);
}

function expectSameFile(review: ReviewFile, parsed: PatchFile): void {
  expect({
    path: review.path,
    kind: review.kind,
    additions: review.additions,
    deletions: review.deletions,
    suppressGaps: review.suppressGaps === true,
  }).toEqual({
    path: parsed.path,
    kind: parsed.kind,
    additions: parsed.additions,
    deletions: parsed.deletions,
    suppressGaps: parsed.suppressGaps === true,
  });
  expect(review.body.toPatchLines()).toEqual(parsed.lines);
  expect(review.body.patchText()).toBe(patchTextOf(parsed));
  expect(review.body.contentKey()).toBe(contentKey(patchTextOf(parsed)));

  for (const newSideTotal of [undefined, 0, 3, 50, 10_000]) {
    for (const suppressGaps of [false, true]) {
      const reviewVariant: ReviewFile = { ...review, newSideTotal, suppressGaps: suppressGaps || review.suppressGaps };
      const expected = buildPatchDisplayRows(parsed.lines, newSideTotal, suppressGaps || parsed.suppressGaps === true);
      expect(displayRowCount(reviewVariant)).toBe(expected.length);
      expect(withoutLineIndex(materializeRows(reviewVariant, FILE_START, expected.length + 5))).toEqual(expected);
      for (let start = 0; start < expected.length; start += 7) {
        const count = 11;
        const rows = materializeRows(reviewVariant, rowStartAt(reviewVariant, start), count);
        expect(withoutLineIndex(rows)).toEqual(expected.slice(start, start + count));
        expect(splitRowCount(reviewVariant, rowStartAt(reviewVariant, start), count))
          .toBe(buildSplitDisplayRows(expected.slice(start, start + count)).length);
      }
    }
  }
}

function expectParity(patch: string, reviewFiles: ReviewFile[]): void {
  const parsed = parsePatchFiles(patch);
  expect(reviewFiles.map((file) => file.path)).toEqual(parsed.map((file) => file.path));
  reviewFiles.forEach((file, index) => expectSameFile(file, parsed[index]));
}

function parseInChunks(patch: string, sizes: (index: number) => number): { parser: PatchParser; files: ReviewFile[] } {
  const parser = new PatchParser();
  let pos = 0;
  for (let index = 0; pos < patch.length; index += 1) {
    const size = Math.max(1, sizes(index));
    parser.append(patch.slice(pos, pos + size));
    pos += size;
  }
  parser.end();
  return { parser, files: parser.files };
}

describe('PatchParser', () => {
  it('describes a patch exactly as parsePatchFiles and buildPatchDisplayRows do', () => {
    expectParity(mixed, parseReviewFiles(mixed));
  });

  it('describes random patches exactly', () => {
    for (let seed = 1; seed <= 40; seed += 1) {
      const patch = randomPatch(seed);
      expectParity(patch, parseReviewFiles(patch));
    }
  });

  it('gives the same result at every chunking, including mid-line cuts', () => {
    const whole = parseReviewFiles(mixed);
    for (const size of [1, 2, 3, 5, 17, 64, 1000]) {
      const { parser, files } = parseInChunks(mixed, () => size);
      expect(parser.sourceKey()).toBe(diffSourceKey(mixed));
      expectParity(mixed, files);
      expect(files.map((file) => file.body.contentKey())).toEqual(whole.map((file) => file.body.contentKey()));
    }
  });

  it('reads a line longer than several chunks', () => {
    const long = 'x'.repeat(5000);
    const patch = `diff --git a/min.js b/min.js\n--- a/min.js\n+++ b/min.js\n@@ -1 +1 @@\n-${long}\n+${long}y\n`;
    const { files } = parseInChunks(patch, (index) => (index % 2 === 0 ? 700 : 1300));
    expectParity(patch, files);
    const rows = materializeRows(files[0], FILE_START, 10);
    expect(rows.filter((row) => !row.gap).map((row) => row.line.content.length)).toEqual([5001, 5002]);
  });

  it('publishes a file only once the next section shows it is final', () => {
    const parser = new PatchParser();
    const lines = mixed.split('\n');
    const deletedLink = lines.indexOf('diff --git a/link b/link');
    parser.append(lines.slice(0, deletedLink + 6).join('\n') + '\n');
    const before = parser.files.map((file) => file.path);
    expect(before).not.toContain('link');
    parser.append(lines.slice(deletedLink + 6).join('\n'));
    parser.end();
    const link = parser.files.filter((file) => file.path === 'link');
    expect(link).toHaveLength(1);
    expect(link[0].kind).toBe('modified');
    expect(parser.files.slice(0, before.length).map((file) => file.path)).toEqual(before);
  });

  it('keys an empty patch as nothing', () => {
    const parser = new PatchParser();
    parser.end();
    expect(parser.files).toEqual([]);
    expect(parser.sourceKey()).toBe('');
  });

  it('holds one copy of the text and a few bytes per line', () => {
    const patch = randomPatch(7).repeat(50);
    const { parser } = parseInChunks(patch, () => 4096);
    const lines = patch.split('\n').length;
    expect(parser.store.heldBytes()).toBeLessThan(patch.length + lines * 6 + 4096);
  });
});

describe('conflict pseudo-files', () => {
  it('rows match the PatchFile rows, folds and markers included', () => {
    const content = [
      'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h',
      '<<<<<<< ours', 'mine', '=======', 'theirs', '>>>>>>> theirs',
      'i', 'j', 'k', 'l', 'm', 'n', 'o', 'p',
    ].join('\n');
    for (const expandedFolds of [undefined, new Set([0]), new Set([0, 1])]) {
      const parsed = conflictPatchFile('c.txt', content, { baseLabel: 'main', headLabel: 'pr', expandedFolds, notes: ['CONFLICT (content): c.txt'] });
      expectSameFile(reviewFileFromPatchFile(parsed), parsed);
    }
  });
});

describe('row lookups', () => {
  const file = parseReviewFiles(mixed)[0];
  const rows = buildPatchDisplayRows(parsePatchFiles(mixed)[0].lines);

  it('finds the row a comment anchor names', () => {
    rows.forEach((row, index) => {
      if (row.gap) return;
      const side = row.side;
      const expected = rows.findIndex((candidate) => !candidate.gap && candidate.side === side
        && (side === 'old' ? candidate.oldLine === row.oldLine
          : side === 'new' ? candidate.newLine === row.newLine
            : candidate.oldLine === row.oldLine && candidate.newLine === row.newLine));
      expect(anchorRow(file, { side, oldLine: row.oldLine, newLine: row.newLine })).toBe(expected);
      expect(expected).toBeLessThanOrEqual(index);
    });
    expect(anchorRow(file, { side: 'new', newLine: 9999 })).toBe(-1);
  });

  it('finds the nearest row on a side', () => {
    expect(nearestLineRow(file, 'new', 12)).toBe(rows.findIndex((row) => row.newLine === 12));
    expect(nearestLineRow(file, 'old', 11)).toBe(rows.findIndex((row) => row.oldLine === 11 && row.newLine === 0));
    expect(nearestLineRow(file, 'new', 1)).toBe(rows.findIndex((row) => row.newLine > 0));
  });

  it('walks rows with the ids buildPatchDisplayRows assigns', () => {
    const walker = new RowWalker(file);
    const ids: string[] = [];
    while (walker.next()) ids.push(walker.rowId());
    expect(ids).toEqual(rows.map((row) => row.id));
  });
});
