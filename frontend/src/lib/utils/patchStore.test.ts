import { afterEach, describe, expect, it, vi } from 'vitest';
import { diffSourceKey } from './diffSourceKey';
import { contentKey } from './fnv1a';
import { patchTextHeldBytes } from './patchMemory.svelte';
import { PatchParser, sourceHash, type ReviewFile } from './patchParser';
import { LINE_ADD, LINE_CONTEXT, LINE_HUNK, LINE_MARKER, PatchStore } from './patchStore';
import { MIXED_PATCH, parseInChunks, randomPatch, sparselyWide, UNICODE_PATCH } from '../../test/helpers/patchFixtures';
import { packText, patchChunks, textKey, type PatchForm, type StreamedChunk } from '../../test/helpers/streamedPatch';

// Whatever form the chunks arrive in and wherever they are cut, the store
// reads exactly as a store of the whole plain text.

const disposers: (() => void)[] = [];

afterEach(() => {
  for (const dispose of disposers.splice(0)) dispose();
});

// Every chunk carries its key, so the parser hashes none of the text.
function streamed(chunks: readonly StreamedChunk[]): PatchParser {
  const append = vi.spyOn(sourceHash, 'append');
  try {
    const parser = new PatchParser();
    disposers.push(() => parser.store.dispose());
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
  disposers.push(() => reference.store.dispose());
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

describe('PatchStore', () => {
  it('reads plain lines as stored at any chunking', () => {
    for (const patch of [MIXED_PATCH, UNICODE_PATCH]) {
      for (const size of [1, 7, 64, 4096]) {
        const store = streamed(patchChunks(patch, size)).store;
        for (let start = 0; start < store.lineCount; start += 3) {
          for (const end of [start, start + 1, start + 4, store.lineCount]) {
            const to = Math.min(end, store.lineCount);
            expect(store.storedRange(start, to), `${size} ${start}-${to}`).toBe(store.textRange(start, to));
          }
        }
      }
    }
  });

  it('reads as the plain store does at any chunking and form', () => {
    for (const patch of [UNICODE_PATCH, `${UNICODE_PATCH}\n`, MIXED_PATCH]) {
      for (const form of ['plain', 'packed'] as const satisfies readonly PatchForm[]) {
        for (const size of [1, 2, 3, 5, 7, 64, 100, 10_000]) {
          expectSameAsPlain(patch, streamed(patchChunks(patch, size, form)), `${form} ${size}`);
        }
        expectSameAsPlain(patch, streamed(patchChunks(patch, (index) => [61, 2, 173, 3][index % 4], form)), `${form} varied`);
      }
    }
  });

  // Packing needs wide lines to be sparse in their chunk, so these
  // patches hold ASCII files between the wide ones.
  it('reads packed chunks as the plain store does', () => {
    for (const base of [UNICODE_PATCH, `${UNICODE_PATCH}\n`, MIXED_PATCH]) {
      const patch = sparselyWide(base, 40);
      for (const size of [4096, 9000, 16 * 1024, (index: number) => [6100, 2, 9000, 3][index % 4]]) {
        const chunks = patchChunks(patch, size, 'packed');
        const label = `${base.slice(0, 12)} ${typeof size === 'number' ? size : 'varied'}`;
        expect(chunks.filter((chunk) => chunk.wide !== undefined).length, label).toBeGreaterThan(0);
        expectSameAsPlain(patch, streamed(chunks), label);
      }
    }
  });

  // A line longer than the read arrives in plain pieces (each is all of
  // one wide line), and its end can arrive packed with the lines after it.
  it('reads a wide line whose pieces arrive plain and whose end arrives packed', () => {
    const long = 'ü✓😀'.repeat(230);
    const tail = Array.from({ length: 120 }, (_, index) => `+ascii line ${index}`);
    const header = 'diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1,121 @@\n';
    const patch = `${header}-${long}\n${tail.join('\n')}\n`;
    const lineStart = new TextEncoder().encode(header).length;
    const lineEnd = lineStart + new TextEncoder().encode(`-${long}`).length;
    // Windows whose last piece of the line is short.
    for (const size of [690, 1010, 1020]) {
      const chunks = patchChunks(patch, size, 'packed');
      const pieces = chunks.filter((chunk) => chunk.nextOffset > lineStart && chunk.offset < lineEnd);
      expect(pieces.length, `${size}`).toBeGreaterThan(1);
      expect(pieces.slice(0, -1).every((chunk) => chunk.wide === undefined), `${size}`).toBe(true);
      expect(pieces[pieces.length - 1].wide, `${size}`).toBeDefined();
      expectSameAsPlain(patch, streamed(chunks), `packed ${size}`);
    }
  });

  it('marks wide lines past the first block of line kinds', () => {
    const lines = Array.from({ length: 3000 }, (_, index) => (index % 50 === 0 ? `+ligne ${index} ✓` : `+line ${index}`));
    const patch = `diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -0,0 +1,3000 @@\n${lines.join('\n')}\n`;
    const chunks = patchChunks(patch, 4096, 'packed');
    expect(chunks.filter((chunk) => chunk.wide !== undefined).length).toBeGreaterThan(3);
    expectSameAsPlain(patch, streamed(chunks), 'packed');
  });

  it('decodes only the lines the chunk marks', () => {
    const parser = new PatchParser();
    const data = packText('+é\n+ü\n');
    parser.append(data, { offset: 0, nextOffset: data.length, wide: [5], key: textKey('+é\n+ü\n') });
    parser.end();
    expect([parser.store.text(0), parser.store.text(1)]).toEqual([data.slice(0, 3), '+ü']);
  });

  it('records the kinds and hunk starts of explicitly typed lines', () => {
    const store = PatchStore.fromLines([
      { content: '@@ -3,1 +4,2 @@ not parsed', kind: { oldStart: 30, newStart: 40 } },
      { content: ' same', kind: LINE_CONTEXT },
      { content: '+new', kind: LINE_ADD },
      { content: '+++++++ fold', kind: LINE_MARKER, fold: { id: 1, lines: 3 } },
    ]);
    expect([0, 1, 2, 3].map((line) => store.kind(line))).toEqual([LINE_HUNK, LINE_CONTEXT, LINE_ADD, LINE_MARKER]);
    // The starts are the ones given, not the header text's.
    expect([store.hunkOldStart(0), store.hunkNewStart(0)]).toEqual([30, 40]);
    expect(store.fold(3)).toEqual({ id: 1, lines: 3 });
    expect(() => store.hunkOldStart(1)).toThrow('not a hunk header');
  });

  // A pin holds whole chunks: pinnedEnd extends a range through the
  // lines that end in the chunk its last line ends in, and no further.
  it('answers the lines a pin holds anyway, lines spanning chunks included', () => {
    const lengths = [5, 40, 3, 3, 90, 7, 2, 2, 2, 150, 4, 60, 1];
    const lines = ['diff --git a/x b/x', '--- a/x', '+++ b/x', `@@ -0,0 +1,${lengths.length} @@`, ...lengths.map((n, i) => `+${String.fromCharCode(97 + i).repeat(n)}`)];
    const patch = lines.join('\n') + '\n';
    // Chunks as the backend cuts them, which end after a newline when they
    // hold one, and cut every `n` characters, which need not.
    const cuts: [string, { offset: number; nextOffset: number; data: string }[], PatchParser][] = [];
    for (const window of [16, 25, 64]) {
      const chunks = patchChunks(patch, window);
      cuts.push([`window ${window}`, chunks, streamed(chunks)]);
    }
    for (const every of [7, 11, 30]) {
      const parser = new PatchParser();
      disposers.push(() => parser.store.dispose());
      const chunks = [];
      for (let offset = 0; offset < patch.length; offset += every) {
        const data = patch.slice(offset, offset + every);
        chunks.push({ offset, nextOffset: offset + data.length, data });
        parser.append(data);
      }
      parser.end();
      cuts.push([`every ${every}`, chunks, parser]);
    }
    for (const [label, chunks, parser] of cuts) {
      const store = parser.store;
      // The chunk each line's newline is in.
      const ends: number[] = [];
      for (let line = 0, at = 0; line < lines.length; line += 1) {
        at += lines[line].length;
        ends.push(chunks.findIndex((chunk) => chunk.offset <= at && at < chunk.nextOffset));
        at += 1;
      }
      for (let start = 0; start < lines.length; start += 1) {
        for (let end = start + 1; end <= lines.length; end += 1) {
          let want = end;
          while (want < lines.length && ends[want] <= ends[end - 1]) want += 1;
          expect(store.pinnedEnd(start, end), `${label}: ${start}..${end}`).toBe(want);
        }
      }
    }
  });

  it('answers its line count and size read-only', () => {
    const store = PatchStore.fromLines([{ content: '+a', kind: LINE_ADD }]);
    // @ts-expect-error lineCount is read-only.
    expect(() => { store.lineCount = 5; }).toThrow(TypeError);
    // @ts-expect-error storedSize is read-only.
    expect(() => { store.storedSize = 5; }).toThrow(TypeError);
    expect([store.lineCount, store.storedSize]).toEqual([1, 3]);
  });

  // The line-start buffer every store shares grows for a chunk of many
  // lines. However a parse stops, the next parse that needs it grown
  // grows it again from its first size.
  it('gives up the grown line-start buffer however a parse stops', () => {
    const many = Array.from({ length: 5000 }, (_, index) => `+line ${index}`).join('\n') + '\n';
    const stops: [string, (parser: PatchParser) => void][] = [
      ['end', (parser) => parser.end()],
      ['dispose mid-stream', (parser) => parser.store.dispose()],
      ['dispose after a refused chunk', (parser) => {
        expect(() => parser.append('+b\n', { offset: 0, nextOffset: 3, wide: [1] })).toThrow('without its key');
        parser.store.dispose();
      }],
    ];
    for (const [name, stop] of stops) {
      const stopped = new PatchParser();
      stopped.append(many);
      stop(stopped);
      const sizes: number[] = [];
      vi.stubGlobal('Uint32Array', class extends Uint32Array {
        constructor(...args: ConstructorParameters<typeof Uint32Array>) {
          super(...args);
          sizes.push(this.length);
        }
      });
      try {
        const next = new PatchParser();
        next.append(many);
        next.store.dispose();
      } finally {
        vi.unstubAllGlobals();
      }
      expect(sizes, name).toContain(8192);
    }
  });

  it('holds one copy of the text', () => {
    const base = patchTextHeldBytes();
    const patch = randomPatch(7).repeat(50);
    const { parser } = parseInChunks(patch, () => 4096);
    disposers.push(() => parser.store.dispose());
    expect(patchTextHeldBytes() - base).toBe(patch.length);
  });
});
