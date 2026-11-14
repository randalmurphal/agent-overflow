import { describe, expect, it, vi } from 'vitest';
import { conflictPatchFile } from './conflictFile';
import { diffSourceKey } from './diffSourceKey';
import { contentKey, fnv1a32 } from './fnv1a';
import {
  buildPatchDisplayRows,
  buildSplitDisplayRows,
  parsePatchFiles,
  type PatchDisplayRow,
  type PatchFile,
} from './patchFiles';
import {
  anchorLines,
  anchorRows,
  completeAnchor,
  anchorLinesMatch,
  anchorRow,
  displayRowCount,
  FILE_START,
  materializeRows,
  nearestLineRow,
  RowWalker,
  rowStartAt,
  splitRowCount,
} from './patchRows';
import { keptText, PatchParser, parseReviewFiles, reviewFileFromPatchFile, sourceHash, type PatchChunkSpan, type ReviewFile } from './patchStore';
import { packText, patchChunks, textKey, unkeyed, wideMarks, type PatchForm, type StreamedChunk } from '../../test/helpers/streamedPatch';
import packedVectors from '../../test/fixtures/packedTextVectors.json';

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
  '--- removed "-- note" reads like a header',
  '+++ added "++i" reads like a header',
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
        // Some lines' text starts with "--" or "++", so the line reads
        // like a file header with its prefix.
        const lead = ['', '', '', '--', '++'][rand(5)];
        const text = `${lead}line ${rand(1000)} ${'x'.repeat(rand(30))}`;
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
      expect(withoutLineIndex(materializeRows(reviewVariant, FILE_START, expected.length + 5).rows)).toEqual(expected);
      for (let start = 0; start < expected.length; start += 7) {
        const count = 11;
        const rows = materializeRows(reviewVariant, rowStartAt(reviewVariant, start), count).rows;
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

  it('reads header lookalikes inside a hunk as the lines they are', () => {
    const [file] = parseReviewFiles(mixed);
    const lines = file.body.toPatchLines();
    const removed = lines.findIndex((line) => line.content.startsWith('--- removed'));
    const added = lines.findIndex((line) => line.content.startsWith('+++ added'));
    expect(lines[removed].type).toBe('del');
    expect(lines[added].type).toBe('add');
    expect(file.path).toBe('src/app.ts');
    expect([file.additions, file.deletions]).toEqual([5, 4]);
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
    const rows = materializeRows(files[0], FILE_START, 10).rows;
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

  it('records what each header says as it parses, at every chunking, and the same as a PatchFile of it', () => {
    const patch = [
      'diff --git a/src/old.ts b/src/new.ts',
      'similarity index 90%',
      'rename from src/old.ts',
      'rename to src/new.ts',
      'index 1111111..2222222 100644',
      '--- a/src/old.ts',
      '+++ b/src/new.ts',
      '@@ -1,1 +1,1 @@',
      '-rename from not/a/header',
      '+index 3333333..4444444 100644',
      'diff --git a/lib b/lib',
      'index abcdef0..1234567 160000',
      '--- a/lib',
      '+++ b/lib',
      '@@ -1 +1 @@',
      '-Subproject commit abcdef0',
      '+Subproject commit 1234567',
      'diff --git a/added.ts b/added.ts',
      'new file mode 100644',
      'index 0000000..5555555',
      '--- /dev/null',
      '+++ b/added.ts',
      '@@ -0,0 +1 @@',
      '+x',
      'diff --git a/logo.png b/logo.png',
      'index 6666666..7777777 100644',
      'Binary files a/logo.png and b/logo.png differ',
    ].join('\n');
    const expected = [
      { sections: 1, oldPath: 'src/old.ts', indexed: true, oldBlob: '1111111', newBlob: '2222222', submodule: false, binary: false },
      { sections: 1, oldPath: null, indexed: true, oldBlob: 'abcdef0', newBlob: '1234567', submodule: true, binary: false },
      { sections: 1, oldPath: null, indexed: true, oldBlob: null, newBlob: '5555555', submodule: false, binary: false },
      { sections: 1, oldPath: null, indexed: true, oldBlob: '6666666', newBlob: '7777777', submodule: false, binary: true },
    ];
    for (const size of [1, 7, 64, 10_000]) expect(parseInChunks(patch, () => size).files.map((file) => file.header)).toEqual(expected);
    expect(parsePatchFiles(patch).map((file) => reviewFileFromPatchFile(file).header)).toEqual(expected);
  });

  // A tripwire: JS cannot see whether a string is a slice holding its
  // chunk alive, only whether it went through the copy.
  it('copies every string a file keeps from the patch\'s chunks', () => {
    const own = vi.spyOn(keptText, 'own').mockImplementation((text) => `owned:${text}`);
    try {
      const files = parseReviewFiles([
        'diff --git a/src/old.ts b/src/new.ts',
        'rename from src/old.ts',
        'rename to src/new.ts',
        'index 1111111..2222222 100644',
        '--- a/src/old.ts',
        '+++ b/src/new.ts',
        '@@ -1 +1 @@',
        '-a',
        '+b',
        'diff --git a/link b/link',
        'deleted file mode 120000',
        'index 3333333..0000000',
        '--- a/link',
        '+++ /dev/null',
        '@@ -1 +0,0 @@',
        '-target',
        'diff --git a/link b/link',
        'new file mode 100644',
        'index 0000000..4444444',
        '--- /dev/null',
        '+++ b/link',
        '@@ -0,0 +1 @@',
        '+file',
      ].join('\n'));
      expect(files.map((file) => [file.path, file.header?.oldPath ?? null, file.header?.oldBlob ?? null, file.header?.newBlob ?? null])).toEqual([
        ['owned:src/new.ts', 'owned:src/old.ts', 'owned:1111111', 'owned:2222222'],
        // A type change: one file, its deleted section's header.
        ['owned:link', null, 'owned:3333333', null],
      ]);
    } finally {
      own.mockRestore();
    }
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

describe('split-view halves of a context row', () => {
  // Old 1..3, new 1..3: 1 and 3 unchanged, 2 replaced.
  const file = parseReviewFiles([
    'diff --git a/src/app.ts b/src/app.ts',
    'index 1111111..2222222 100644',
    '--- a/src/app.ts',
    '+++ b/src/app.ts',
    '@@ -1,3 +1,4 @@',
    ' alpha',
    '-beta',
    '+beta two',
    '+inserted',
    ' gamma',
  ].join('\n'))[0];

  it('names the context row from either half with both numbers; one number names it only for a forge thread', () => {
    const gamma = anchorRow(file, { side: 'context', oldLine: 3, newLine: 4 });
    expect(gamma).toBeGreaterThan(0);
    expect(anchorRow(file, { side: 'old', oldLine: 3, newLine: 4 })).toBe(gamma);
    expect(anchorRow(file, { side: 'new', oldLine: 3, newLine: 4 })).toBe(gamma);
    // A saved draft with one number on an unchanged line names no row.
    expect(anchorRow(file, { side: 'old', oldLine: 3 })).toBe(-1);
    expect(anchorRow(file, { side: 'new', newLine: 4 })).toBe(-1);
    expect(anchorRow(file, { side: 'old', oldLine: 3 }, true)).toBe(gamma);
    expect(anchorRow(file, { side: 'new', newLine: 4 }, true)).toBe(gamma);
    expect(anchorRow(file, { side: 'old', oldLine: 2 })).toBeGreaterThan(0);
    expect(anchorRow(file, { side: 'old', oldLine: 4 }, true)).toBe(-1);
  });

  it('completes a half to both numbers, and leaves a changed line as it is', () => {
    expect(completeAnchor(file, { side: 'old' as const, oldLine: 3 })).toEqual({ side: 'old', oldLine: 3, newLine: 4 });
    expect(completeAnchor(file, { side: 'new' as const, newLine: 4 })).toEqual({ side: 'new', oldLine: 3, newLine: 4 });
    expect(completeAnchor(file, { side: 'old' as const, oldLine: 2 })).toEqual({ side: 'old', oldLine: 2 });
    expect(completeAnchor(file, { side: 'new' as const, newLine: 9 })).toEqual({ side: 'new', newLine: 9 });
  });

  it('agrees with the membership check, strict and loose', () => {
    const lines = anchorLines(file);
    for (const anchor of [
      { side: 'old' as const, oldLine: 3 },
      { side: 'new' as const, newLine: 4 },
      { side: 'old' as const, oldLine: 3, newLine: 4 },
      { side: 'new' as const, oldLine: 3, newLine: 4 },
      { side: 'old' as const, oldLine: 2 },
      { side: 'new' as const, newLine: 3 },
      { side: 'context' as const, oldLine: 1, newLine: 1 },
      { side: 'old' as const, oldLine: 4 },
      { side: 'new' as const, newLine: 5 },
      { side: 'context' as const, oldLine: 3, newLine: 3 },
    ]) {
      for (const loose of [false, true]) {
        expect(anchorLinesMatch(lines, anchor, loose), `${JSON.stringify(anchor)} ${loose}`).toBe(anchorRow(file, anchor, loose) >= 0);
      }
    }
  });

  it('counts the rows an anchor names and the place of the one at a line', () => {
    expect(anchorRows(file, { side: 'old', oldLine: 3, newLine: 4 })).toMatchObject({ count: 1, occurrence: 0, context: true });
    expect(anchorRows(file, { side: 'new', newLine: 9 })).toMatchObject({ count: 0, row: -1 });
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

// A chunk after the open can arrive packed (ReadReviewDiffPacked). Whatever
// the form and the cuts, the store reads exactly as the all-plain store.
describe('packed chunks', () => {
  const long = `${'é'.repeat(40)}x😀${'✓'.repeat(30)}`;
  const unicode = [
    '﻿preamble with a BOM',
    'diff --git a/docs/à côté.md b/docs/à côté.md',
    'index 1111111..2222222 100644',
    '--- a/docs/à côté.md',
    '+++ b/docs/à côté.md',
    '@@ -1,6 +1,7 @@ fn ünïcode() {',
    ' ascii context',
    '-héllo wörld\r',
    '+hello world ✓\r',
    '+astral 😀 𝄞 and �',
    '',
    `-${long}`,
    `+${long}y`,
    ' end',
    'diff --git a/naïve.txt b/日本.txt',
    'similarity index 90%',
    'rename from naïve.txt',
    'rename to 日本.txt',
    'index 3333333..4444444 100644',
    '--- a/naïve.txt',
    '+++ b/日本.txt',
    '@@ -1 +1 @@',
    '-a',
    '+ü',
    // Its path is only in the header line; `à` packs to C3 A0, and A0
    // is a space to a regex.
    'diff --git a/à.png b/à.png',
    'index 5555555..6666666 100644',
    'Binary files a/à.png and b/à.png differ',
    // A pure rename: its path is only in `rename to`.
    'diff --git a/x y.txt b/ü z.txt',
    'similarity index 100%',
    'rename from x y.txt',
    'rename to ü z.txt',
    'diff --git a/plain.go b/plain.go',
    '--- a/plain.go',
    '+++ b/plain.go',
    '@@ -1,2 +1,2 @@',
    '-package a',
    '+package b',
    ' 😀',
  ].join('\n');

  // Every chunk carries its key, so the parser hashes none of the text.
  function streamed(patch: string, size: number | ((index: number) => number), form: PatchForm): PatchParser {
    const chunks = patchChunks(patch, size, form);
    const append = vi.spyOn(sourceHash, 'append');
    try {
      const parser = new PatchParser();
      for (const chunk of chunks) parser.append(chunk.data, chunk);
      parser.end();
      expect(append).not.toHaveBeenCalled();
      return parser;
    } finally {
      append.mockRestore();
    }
  }

  function expectSameAsPlain(patch: string, parser: PatchParser, label: string): void {
    const reference = new PatchParser();
    reference.append(patch);
    reference.end();
    const store = parser.store;
    expect(store.lineCount, label).toBe(reference.store.lineCount);
    for (let line = 0; line < store.lineCount; line += 1) expect(store.text(line), `${label} line ${line}`).toBe(reference.store.text(line));
    for (let start = 0; start < store.lineCount; start += 3) {
      for (const end of [start + 1, start + 4, store.lineCount]) {
        const to = Math.min(end, store.lineCount);
        expect(store.textRange(start, to), `${label} range ${start}-${to}`).toBe(reference.store.textRange(start, to));
      }
    }
    expect(parser.sourceKey(), label).toBe(diffSourceKey(patch));
    const summary = (file: ReviewFile) => ({
      path: file.path,
      kind: file.kind,
      additions: file.additions,
      deletions: file.deletions,
      header: file.header,
      lines: file.body.toPatchLines(),
      text: file.body.patchText(),
      key: file.body.contentKey(),
    });
    expect(parser.files.map(summary), label).toEqual(reference.files.map(summary));
    for (const file of parser.files) expect(file.body.contentKey(), label).toBe(contentKey(file.body.patchText()));
  }

  it('reads as the plain store does at any chunking and form, cuts inside characters included', () => {
    for (const patch of [unicode, `${unicode}\n`, mixed]) {
      for (const form of ['plain', 'packed', 'plain-first'] as const) {
        for (const size of [1, 2, 3, 5, 7, 64, 100, 10_000]) {
          expectSameAsPlain(patch, streamed(patch, size, form), `${form} ${size}`);
        }
        expectSameAsPlain(patch, streamed(patch, (index) => [61, 2, 173, 3][index % 4], form), `${form} varied`);
      }
    }
  });

  it('reads a line longer than its chunks, cut inside its characters, after a plain piece', () => {
    const patch = `diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-${'ü✓😀'.repeat(400)}\n+${'a'.repeat(300)}${'é'.repeat(300)}\n`;
    const longStart = packText(patch).indexOf('-');
    for (const size of [7, 50, 61, 97]) {
      expectSameAsPlain(patch, streamed(patch, size, 'packed'), `packed ${size}`);
      // The first chunk ends inside the long line, so the line has a
      // plain piece and packed pieces.
      expectSameAsPlain(patch, streamed(patch, (index) => (index === 0 ? longStart + 20 : size), 'plain-first'), `plain-first ${size}`);
    }
  });

  it('marks wide lines past the first block of line kinds', () => {
    const lines = Array.from({ length: 3000 }, (_, index) => (index % 7 === 0 ? `+ligne ${index} é` : `+line ${index}`));
    const patch = `diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -0,0 +1,3000 @@\n${lines.join('\n')}\n`;
    expectSameAsPlain(patch, streamed(patch, 4096, 'packed'), 'packed');
  });

  // What the Go side answers for a chunk's bytes (internal/gitdiff
  // TestPackTextMatchesTheSharedVectors holds PackText to the same file).
  it('reads every chunk the backend answers as the text a plain read delivers, under the key it answers', () => {
    expect(packedVectors.some((vector) => vector.packed) && packedVectors.some((vector) => !vector.packed)).toBe(true);
    for (const vector of packedVectors) {
      // The Go fold of the chunk's bytes (internal/gitdiff) keys the text.
      expect(vector.key, vector.name).toEqual({ hash: fnv1a32(vector.text), units: vector.text.length });
      const bytes = vector.rawHex.length / 2;
      const span: PatchChunkSpan = { offset: 0, nextOffset: bytes, key: vector.key };
      if (vector.packed) span.wide = vector.wide;
      const parser = new PatchParser();
      parser.append(vector.data, span);
      parser.end();
      const lines = vector.text === '' ? [] : vector.text.split('\n');
      if (vector.text.endsWith('\n')) lines.pop();
      const store = parser.store;
      expect(Array.from({ length: store.lineCount }, (_, line) => store.text(line)), vector.name).toEqual(lines);
      expect(store.textRange(0, store.lineCount), vector.name).toBe(lines.join('\n'));
      // An empty patch has no key.
      expect(parser.sourceKey(), vector.name).toBe(vector.text === '' ? '' : diffSourceKey(vector.text));
      const reference = parseReviewFiles(vector.text);
      expect(parser.files.map((file) => [file.path, file.body.patchText()]), vector.name)
        .toEqual(reference.map((file) => [file.path, file.body.patchText()]));
      // The helper these tests pack with agrees with the backend.
      if (vector.packed) expect([packText(vector.text), wideMarks(vector.data)], vector.name).toEqual([vector.data, vector.wide]);
    }
  });

  it('refuses a mark that is not at a byte past ASCII, out of order, or outside the chunk', () => {
    const data = packText('+é\n+ü\n');
    for (const wide of [[0], [4, 1], [1, 1], [1, 99], [-1], [1.5]]) {
      const parser = new PatchParser();
      expect(() => parser.append(data, { offset: 0, nextOffset: data.length, wide, key: textKey('+é\n+ü\n') }), JSON.stringify(wide)).toThrow('wide mark');
      expect(parser.store.lineCount).toBe(0);
    }
  });

  it('decodes only the lines the chunk marks', () => {
    const parser = new PatchParser();
    const data = packText('+é\n+ü\n');
    parser.append(data, { offset: 0, nextOffset: data.length, wide: [5], key: textKey('+é\n+ü\n') });
    parser.end();
    expect([parser.store.text(0), parser.store.text(1)]).toEqual([data.slice(0, 3), '+ü']);
  });
});

// The backend keys each chunk with the source key of the text through its
// end (PatchChunkSpan.key); a backend older than that keys none.
describe('source keys', () => {
  const patch = `${mixed}\n+astral 😀 é ✓\n`;

  function keyed(chunks: readonly StreamedChunk[]): { parser: PatchParser; hashed: number } {
    const append = vi.spyOn(sourceHash, 'append');
    try {
      const parser = new PatchParser();
      for (const chunk of chunks) parser.append(chunk.data, chunk);
      parser.end();
      return { parser, hashed: append.mock.calls.length };
    } finally {
      append.mockRestore();
    }
  }

  it('takes the last chunk\'s key as the patch\'s, hashing nothing', () => {
    const chunks = patchChunks(patch, 37);
    const last = chunks[chunks.length - 1];
    // A key the text does not hash to is still the key: it is not checked
    // against the text, only taken.
    last.key = { hash: 0x1234, units: last.key!.units };
    const { parser, hashed } = keyed(chunks);
    expect(parser.sourceKey()).toBe(`fnv1a:00001234:${patch.length}`);
    expect(hashed).toBe(0);
  });

  it('hashes chunks without a key, at any chunking', () => {
    for (const size of [1, 5, 64, 10_000]) {
      const chunks = patchChunks(patch, size).map(unkeyed);
      const { parser, hashed } = keyed(chunks);
      expect(parser.sourceKey(), `${size}`).toBe(diffSourceKey(patch));
      expect(hashed, `${size}`).toBe(chunks.length);
    }
  });

  it('continues from unkeyed chunks with keyed ones, and from keyed with unkeyed', () => {
    for (const form of ['plain', 'plain-first'] as const) {
      const chunks = patchChunks(patch, 41, form);
      expect(chunks.length).toBeGreaterThan(3);
      // A packed chunk always has its key, so only plain chunks drop theirs.
      const mixedKeys = chunks.map((chunk, index) => (chunk.wide === undefined && index % 2 === 0 ? unkeyed(chunk) : chunk));
      const { parser, hashed } = keyed(mixedKeys);
      expect(parser.sourceKey(), form).toBe(diffSourceKey(patch));
      expect(hashed, form).toBe(mixedKeys.filter((chunk) => chunk.key === undefined).length);
      // The last chunk without a key: hashed from the key before it.
      const tail = chunks.map((chunk, index) => (index === chunks.length - 1 && chunk.wide === undefined ? unkeyed(chunk) : chunk));
      expect(keyed(tail).parser.sourceKey(), form).toBe(diffSourceKey(patch));
    }
  });

  it('refuses a packed chunk without its key, and stores none of it', () => {
    const data = packText('+é\n');
    const parser = new PatchParser();
    expect(() => parser.append(data, { offset: 0, nextOffset: data.length, wide: [1] })).toThrow('without its key');
    expect(parser.store.lineCount).toBe(0);
  });

  it('refuses a key that cannot be its text\'s, and stores none of the chunk', () => {
    const first = '+a\n';
    const plain = '+bc\n';
    const packed = packText('+é\n');
    const cases: { name: string; data: string; key: unknown; wide?: number[] }[] = [
      { name: 'null', data: plain, key: null },
      { name: 'not an object', data: plain, key: 7 },
      { name: 'hash missing', data: plain, key: { units: 7 } },
      { name: 'hash negative', data: plain, key: { hash: -1, units: 7 } },
      { name: 'hash 2^32', data: plain, key: { hash: 0x1_0000_0000, units: 7 } },
      { name: 'hash fractional', data: plain, key: { hash: 1.5, units: 7 } },
      { name: 'units negative', data: plain, key: { hash: 1, units: -1 } },
      { name: 'units fractional', data: plain, key: { hash: 1, units: 6.5 } },
      { name: 'units unsafe', data: plain, key: { hash: 1, units: 2 ** 53 } },
      { name: 'units a string', data: plain, key: { hash: 1, units: '7' } },
      { name: 'plain units short of its text', data: plain, key: { hash: 1, units: 6 } },
      { name: 'plain units past its text', data: plain, key: { hash: 1, units: 8 } },
      { name: 'packed units behind the count', data: packed, key: { hash: 1, units: 2 }, wide: [1] },
      { name: 'packed units past its bytes', data: packed, key: { hash: 1, units: 8 }, wide: [1] },
      { name: 'packed units fractional', data: packed, key: { hash: 1, units: 3.5 }, wide: [1] },
      { name: 'packed units unsafe', data: packed, key: { hash: 1, units: 2 ** 53 }, wide: [1] },
      { name: 'packed units infinite', data: packed, key: { hash: 1, units: Infinity }, wide: [1] },
      { name: 'packed units a string', data: packed, key: { hash: 1, units: '9' }, wide: [1] },
    ];
    for (const { name, data, key, wide } of cases) {
      const parser = new PatchParser();
      parser.append(first, { offset: 0, nextOffset: 3, key: textKey(first) });
      const span = { offset: 3, nextOffset: 3 + data.length, key, ...(wide ? { wide } : {}) } as PatchChunkSpan;
      expect(() => parser.append(data, span), name).toThrow('key that cannot be');
      expect(parser.store.lineCount, name).toBe(1);
    }
    // The bounds themselves are keys.
    for (const { data, key, wide } of [
      { data: plain, key: { hash: 0, units: 7 } },
      { data: plain, key: { hash: 0xffff_ffff, units: 7 } },
      { data: packed, key: { hash: 1, units: 3 }, wide: [1] },
      { data: packed, key: { hash: 1, units: 7 }, wide: [1] },
    ]) {
      const parser = new PatchParser();
      parser.append(first, { offset: 0, nextOffset: 3, key: textKey(first) });
      parser.append(data, { offset: 3, nextOffset: 3 + data.length, key, ...(wide ? { wide } : {}) });
      parser.end();
      expect(parser.sourceKey()).toBe(`fnv1a:${key.hash.toString(16).padStart(8, '0')}:${key.units}`);
    }
  });
});
