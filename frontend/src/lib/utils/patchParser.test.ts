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
  displayRowCount,
  FILE_START,
  materializeRows,
  rowStartAt,
  splitRowCount,
} from './patchRows';
import { PatchParser, parseReviewFiles, reviewFileFromPatchFile, sourceHash, type PatchChunkMarks, type ReviewFile } from './patchParser';
import { keptText } from './patchStore';
import { MIXED_PATCH, parseInChunks, randomPatch } from '../../test/helpers/patchFixtures';
import { fakeDiff, packText, patchChunks, textKey, unkeyed, wideMarks, type StreamedChunk } from '../../test/helpers/streamedPatch';
import packedVectors from '../../test/fixtures/packedTextVectors.json';

// The compact store must describe every patch exactly as parsePatchFiles +
// buildPatchDisplayRows do: same files, same lines, same rows, same row ids,
// same intraline pairing, same highlight keys, at any chunking.

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

describe('PatchParser', () => {
  it('describes a patch exactly as parsePatchFiles and buildPatchDisplayRows do', () => {
    expectParity(MIXED_PATCH, parseReviewFiles(MIXED_PATCH));
  });

  it('reads header lookalikes inside a hunk as the lines they are', () => {
    const [file] = parseReviewFiles(MIXED_PATCH);
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
    const whole = parseReviewFiles(MIXED_PATCH);
    for (const size of [1, 2, 3, 5, 17, 64, 1000]) {
      const { parser, files } = parseInChunks(MIXED_PATCH, () => size);
      expect(parser.sourceKey()).toBe(diffSourceKey(MIXED_PATCH));
      expectParity(MIXED_PATCH, files);
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
    const lines = MIXED_PATCH.split('\n');
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

// A chunk after the open can arrive packed (ReadReviewDiffPacked). The
// parser checks what each chunk arrives with before storing any of it.
describe('packed chunks', () => {
  // What the Go side answers for a chunk's bytes (internal/jstext
  // TestPackTextMatchesTheSharedVectors holds PackText to the same file).
  it('reads every chunk the backend answers as the text a plain read delivers, under the key it answers', () => {
    expect(packedVectors.some((vector) => vector.packed) && packedVectors.some((vector) => !vector.packed)).toBe(true);
    for (const vector of packedVectors) {
      // The Go fold of the chunk's bytes (internal/jstext) keys the text.
      expect(vector.key, vector.name).toEqual({ hash: fnv1a32(vector.text), units: vector.text.length });
      const bytes = vector.rawHex.length / 2;
      // Marks as the wire carries them: an unpacked vector's are empty.
      const parser = new PatchParser();
      parser.append(vector.data, { offset: 0, nextOffset: bytes, wide: vector.wide, key: vector.key });
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
      // The fake backend the other tests read from answers what the Go
      // backend answers.
      const raw = Uint8Array.from(vector.rawHex.match(/../g) ?? [], (pair) => Number.parseInt(pair, 16));
      const served = fakeDiff(raw).read(0, Math.max(1, bytes), true);
      expect({ data: served.data, wide: served.wide ?? [], key: served.key }, vector.name)
        .toEqual({ data: vector.data, wide: vector.wide, key: vector.key });
      expect(fakeDiff(raw).read(0, Math.max(1, bytes), false).data, vector.name).toBe(vector.text);
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

  it('stores a chunk with empty marks as plain text', () => {
    const data = packText('+é\n');
    const parser = new PatchParser();
    parser.append(data, { offset: 0, nextOffset: data.length, wide: [], key: textKey(data) });
    parser.end();
    expect(parser.store.text(0)).toBe(data.slice(0, -1));
    // Its key counts its units, as a plain chunk's does; a packed chunk's
    // may count fewer.
    const short = new PatchParser();
    expect(() => short.append(data, { offset: 0, nextOffset: data.length, wide: [], key: textKey('+é\n') })).toThrow('key that cannot be');
    expect(short.store.lineCount).toBe(0);
  });

  it('refuses wide marks that are not a list, and stores none of the chunk', () => {
    const data = packText('+é\n');
    for (const wide of [1, {}, '1']) {
      const parser = new PatchParser();
      const chunk = { offset: 0, nextOffset: data.length, wide, key: textKey('+é\n') } as unknown as PatchChunkMarks;
      expect(() => parser.append(data, chunk), JSON.stringify(wide)).toThrow('not a list');
      expect(parser.store.lineCount).toBe(0);
    }
  });

  it('refuses a packed chunk without its key, and stores none of it', () => {
    const data = packText('+é\n');
    const parser = new PatchParser();
    expect(() => parser.append(data, { offset: 0, nextOffset: data.length, wide: [1] })).toThrow('without its key');
    expect(parser.store.lineCount).toBe(0);
  });

  // The backend cuts between characters, so a packed chunk decodes alone;
  // one that does not would show U+FFFD where the character was cut.
  it('refuses a packed chunk cut inside a character, and stores none of it', () => {
    const whole = packText('+✓ é\n+😀');
    const cases: [string, string][] = [
      ['starts inside', whole.slice(2)],
      ['ends inside a three-byte character', whole.slice(0, 3)],
      ['ends inside a four-byte character', whole.slice(0, -1)],
      ['ends inside a two-byte character', packText('+é').slice(0, 2)],
    ];
    for (const [name, data] of cases) {
      const parser = new PatchParser();
      const wide = wideMarks(data);
      expect(() => parser.append(data, { offset: 0, nextOffset: data.length, wide, key: { hash: 1, units: data.length } }), name).toThrow('inside a character');
      expect(parser.store.lineCount, name).toBe(0);
    }
    for (const data of [whole, packText('😀'), packText('é\n')]) {
      const parser = new PatchParser();
      parser.append(data, { offset: 0, nextOffset: data.length, wide: wideMarks(data), key: { hash: 1, units: data.length } });
      expect(parser.store.lineCount).toBeGreaterThan(0);
    }
  });
});

// The backend keys each chunk with the source key of the text through its
// end (PatchChunkMarks.key); a backend older than that keys none.
describe('source keys', () => {
  const patch = `${MIXED_PATCH}\n+astral 😀 é ✓\n`;

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
    for (const form of ['plain', 'packed'] as const) {
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
      const chunk = { offset: 3, nextOffset: 3 + data.length, key, ...(wide ? { wide } : {}) } as PatchChunkMarks;
      expect(() => parser.append(data, chunk), name).toThrow('key that cannot be');
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
      parser.append(data, { offset: 3, nextOffset: 3 + data.length, key, wide });
      parser.end();
      expect(parser.sourceKey()).toBe(`fnv1a:${key.hash.toString(16).padStart(8, '0')}:${key.units}`);
    }
  });
});
