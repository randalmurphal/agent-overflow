import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  patchMemory,
  patchTextEvictableChunks,
  patchTextHeldBytes,
  patchTextVersion,
  setPatchTextBudgetForTest,
} from './patchMemory.svelte';
import { parsePatchFiles } from './patchFiles';
import { whenResident } from './patchBody';
import { PatchParser, parseReviewFiles, type ReviewFile } from './patchParser';
import { PatchTextLost, PatchTextNotResident } from './patchChunks';
import { LINE_ADD, PatchStore } from './patchStore';
import { packText, streamPatch, textKey, type PatchForm, type StreamedPatch } from '../../test/helpers/streamedPatch';

// Patch text past the memory budget: what is evicted first, how it is read
// again, and what a reader can rely on while it is not resident.

const disposers: (() => void)[] = [];

afterEach(() => {
  for (const dispose of disposers.splice(0)) dispose();
  setPatchTextBudgetForTest(null);
});

/** A patch of `files` files with `lines` added lines each; every line is
 * 16 characters, so a 160-character chunk holds ten. */
function patchOf(files: number, lines: number): string {
  const out: string[] = [];
  for (let file = 0; file < files; file += 1) {
    const name = `f${String(file).padStart(3, '0')}.ts`;
    out.push(`diff --git a/${name} b/${name}`);
    out.push(`--- a/${name}`.padEnd(15, ' '));
    out.push(`+++ b/${name}`.padEnd(15, ' '));
    out.push('@@ -1,0 +1,9 @@'.padEnd(15, ' '));
    for (let line = 0; line < lines; line += 1) out.push(`+${file}:${line}`.padEnd(15, '.'));
  }
  return out.join('\n') + '\n';
}

function stream(patch: string, size: number | ((index: number) => number), form?: PatchForm): StreamedPatch {
  const streamed = streamPatch(patch, size, form);
  disposers.push(() => streamed.parser.store.dispose());
  return streamed;
}

function resident(file: ReviewFile): boolean {
  return file.body.resident();
}

/** Reads the first `lines` lines of a file again (all by default), as an
 * exact reader does. */
function load(file: ReviewFile, lines = file.body.lineCount): Promise<void> {
  return file.body.whenResident(() => undefined, 0, lines);
}

describe('patch text past the memory budget', () => {
  it('keeps the beginning of a diff and gives up the text that arrived last', () => {
    const base = patchTextHeldBytes();
    setPatchTextBudgetForTest(1600);
    const { files, source } = stream(patchOf(20, 6), 160);

    expect(patchTextHeldBytes() - base).toBeLessThanOrEqual(1600);
    // It keeps the source to read the evicted text again.
    expect(source.release).not.toHaveBeenCalled();
    expect(resident(files[0])).toBe(true);
    expect(resident(files[1])).toBe(true);
    expect(resident(files[19])).toBe(false);
    expect(() => files[19].body.text(4)).toThrow(PatchTextNotResident);
    // Everything but text stays answerable.
    expect(files.map((file) => file.path)).toEqual(parsePatchFiles(patchOf(20, 6)).map((file) => file.path));
    expect(files[19].additions).toBe(6);
  });

  it('reads evicted text again at the range it first arrived in', async () => {
    const base = patchTextHeldBytes();
    setPatchTextBudgetForTest(1600);
    const patch = patchOf(20, 6);
    const { files, source, spans } = stream(patch, 160);
    const reference = parseReviewFiles(patch);
    disposers.push(() => reference[0].body.segments[0].store.dispose());

    const last = files[19];
    const text = await last.body.whenResident(() => last.body.patchText());
    expect(text).toBe(reference[19].body.patchText());
    const lastSpans = spans.filter((span) => span.nextOffset > patch.indexOf('diff --git a/f019.ts'));
    expect(source.read.mock.calls).toEqual(lastSpans.map((span) => [span.offset, span.nextOffset, false]));
    // The text it read counts, and the budget holds once nothing pins it.
    expect(patchTextHeldBytes() - base).toBeLessThanOrEqual(1600 + reference[0].body.segments[0].store.storedSize);
  });

  it('gives up text nobody read before text that was read, least recently read first', async () => {
    setPatchTextBudgetForTest(1600);
    const { files } = stream(patchOf(20, 6), 160);
    // Ten chunks fit; the first holds f000 and part of f001.
    expect(resident(files[0])).toBe(true);
    const lastResident = files.map(resident).lastIndexOf(true);
    expect(lastResident).toBeGreaterThan(1);

    files[0].body.text(2);
    await load(files[19]);

    expect(resident(files[0])).toBe(true);
    expect(resident(files[19])).toBe(true);
    // Room for the reread came from the newest text nobody read.
    expect(resident(files[lastResident])).toBe(false);
  });

  it('gives up the least recently read text once no unread text is left', async () => {
    const patch = patchOf(20, 6);
    // Room for all but the last file, so the store keeps its source.
    setPatchTextBudgetForTest(19 * 175);
    const { files } = stream(patch, 175);
    expect(resident(files[19])).toBe(false);
    // Every file is one 175-character chunk; reading them makes each one
    // read text, in this order.
    files[15].body.text(4);
    files[17].body.text(4);
    await load(files[19]);
    setPatchTextBudgetForTest(3 * 175);
    patchMemory.enforce();
    expect([15, 17, 19].map((index) => resident(files[index]))).toEqual([true, true, true]);
    expect(files.filter(resident)).toHaveLength(3);

    files[15].body.text(4);
    setPatchTextBudgetForTest(2 * 175);
    patchMemory.enforce();
    expect([15, 17, 19].map((index) => resident(files[index]))).toEqual([true, false, true]);
  });

  it('reads a chunk once for concurrent readers, and one chunk at a time in the order asked', async () => {
    setPatchTextBudgetForTest(160);
    const { files, source } = stream(patchOf(20, 6), 160);
    const releases: (() => void)[] = [];
    const inner = source.read.getMockImplementation()!;
    source.read.mockImplementation((offset, nextOffset, packed) =>
      new Promise((resolve) => { releases.push(() => resolve(inner(offset, nextOffset, packed))); }));

    const file = files[10];
    const first = load(file);
    const again = load(file);
    await Promise.resolve();
    await Promise.resolve();
    expect(source.read).toHaveBeenCalledTimes(1);
    while (releases.length > 0) {
      releases.shift()!();
      await new Promise((resolve) => setTimeout(resolve, 0));
    }
    await Promise.all([first, again]);
    const offsets = source.read.mock.calls.map((call) => call[0] as number);
    expect(offsets).toEqual([...offsets].sort((a, b) => a - b));
    expect(new Set(offsets).size).toBe(offsets.length);
  });

  it('classifies a line longer than its chunks whole, whatever the budget', () => {
    setPatchTextBudgetForTest(1);
    const path = `src/${'deep/'.repeat(400)}file.ts`;
    const patch = [
      `diff --git a/${path} b/${path}`,
      `--- a/${path}`,
      `+++ b/${path}`,
      '@@ -1,1 +1,1 @@',
      `-${'x'.repeat(3000)}`,
      `+${'y'.repeat(3000)}`,
    ].join('\n') + '\n';
    const { files } = stream(patch, 97);

    expect(files.map((file) => file.path)).toEqual([path]);
    expect(files[0].additions).toBe(1);
    expect(files[0].deletions).toBe(1);
  });

  it('matches a resident parse file by file once each file is read again', async () => {
    setPatchTextBudgetForTest(512);
    const patch = patchOf(12, 9);
    const { files } = stream(patch, (index) => (index % 3 === 0 ? 61 : 173));
    const reference = parsePatchFiles(patch);
    expect(files).toHaveLength(reference.length);
    for (let index = 0; index < files.length; index += 1) {
      const file = files[index];
      const lines = await file.body.whenResident(() => file.body.toPatchLines());
      expect(lines).toEqual(reference[index].lines);
    }
  });

  it('pins a range larger than the budget while its reader runs', async () => {
    const base = patchTextHeldBytes();
    setPatchTextBudgetForTest(160);
    const patch = patchOf(4, 30);
    const { files } = stream(patch, 160);
    const reference = parseReviewFiles(patch);
    disposers.push(() => reference[0].body.segments[0].store.dispose());

    const whole = await whenResident(files.flatMap((file) => file.body.segments), () =>
      files.map((file) => file.body.patchText()));
    expect(whole).toEqual(reference.map((file) => file.body.patchText()));
    // Unpinned, the text goes back under the budget.
    expect(patchTextHeldBytes() - base - reference[0].body.segments[0].store.storedSize).toBeLessThanOrEqual(160);
  });

  it('reports text it can no longer read once, and stops asking for it', async () => {
    setPatchTextBudgetForTest(160);
    const { files, source } = stream(patchOf(20, 6), 160);
    source.read.mockRejectedValue(new Error('review diff: not open'));
    const lost = vi.fn();
    files[0].body.segments[0].store.whenLost(lost);

    await expect(load(files[19], 1)).rejects.toBeInstanceOf(PatchTextLost);
    await expect(load(files[18], 1)).rejects.toBeInstanceOf(PatchTextLost);
    expect(lost).toHaveBeenCalledTimes(1);
    expect(lost.mock.calls[0][0].message).toContain('review diff: not open');
    expect(source.read).toHaveBeenCalledTimes(1);
    const late = vi.fn();
    files[0].body.segments[0].store.whenLost(late);
    expect(late).toHaveBeenCalledTimes(1);
  });

  it('treats a reread that answers another chunk as lost', async () => {
    setPatchTextBudgetForTest(160);
    const { files, source } = stream(patchOf(20, 6), 160);
    source.read.mockImplementation(async (_offset: number, nextOffset: number) => ({ data: 'x'.repeat(160), nextOffset: nextOffset + 1, packed: false }));
    await expect(load(files[19], 1)).rejects.toThrow('the diff no longer serves the same chunk');
  });

  it('bumps the text version when evicted text is back', async () => {
    setPatchTextBudgetForTest(160);
    const { files } = stream(patchOf(20, 6), 160);
    const before = patchTextVersion();
    await load(files[19], 1);
    expect(patchTextVersion()).toBeGreaterThan(before);
  });

  it('dispose releases the source once and stops counting the text', () => {
    const base = patchTextHeldBytes();
    setPatchTextBudgetForTest(1600);
    const { parser, source } = stream(patchOf(20, 6), 160);
    expect(patchTextHeldBytes()).toBeGreaterThan(base);

    parser.store.dispose();
    parser.store.dispose();
    expect(source.release).toHaveBeenCalledTimes(1);
    expect(patchTextHeldBytes()).toBe(base);
  });

  it('keeps no chunk of a store that was disposed or ended with all of its text, read or not', () => {
    const base = patchTextEvictableChunks();
    for (let round = 0; round < 3; round += 1) {
      setPatchTextBudgetForTest(1600);
      const closed = stream(patchOf(20, 6), 160);
      closed.files[0].body.text(2);
      expect(patchTextEvictableChunks()).toBeGreaterThan(base);
      closed.parser.store.dispose();
      expect(patchTextEvictableChunks()).toBe(base);

      setPatchTextBudgetForTest(null);
      const finished = stream(patchOf(20, 6), 160);
      finished.files[3].body.text(2);
      expect(patchTextEvictableChunks()).toBe(base);
      // Its text counts against the next round's budget until then.
      finished.parser.store.dispose();
    }
  });

  it('keeps one entry for a chunk however often it is read or offered again', async () => {
    const base = patchTextEvictableChunks();
    setPatchTextBudgetForTest(1600);
    const { files } = stream(patchOf(20, 6), 160);
    const held = patchTextEvictableChunks() - base;
    for (let round = 0; round < 3; round += 1) {
      for (const file of files) await load(file);
    }
    expect(patchTextEvictableChunks() - base).toBeLessThanOrEqual(held);
  });

  it('a disposed store reads nothing again', async () => {
    setPatchTextBudgetForTest(160);
    const { parser, files, source } = stream(patchOf(20, 6), 160);
    parser.store.dispose();
    await expect(load(files[19], 1)).rejects.toBeInstanceOf(PatchTextLost);
    expect(source.read).not.toHaveBeenCalled();
  });

  it('releases the source when the patch ends with all of its text, and keeps that text whatever the budget does', () => {
    const { parser, source } = stream(patchOf(4, 6), 160);
    expect(source.release).toHaveBeenCalledTimes(1);

    setPatchTextBudgetForTest(1);
    stream(patchOf(4, 6), 160);
    expect(parser.files.every(resident)).toBe(true);
    parser.store.dispose();
    expect(source.release).toHaveBeenCalledTimes(1);
  });

  it('keeps the source of a patch that ended with text evicted until it is disposed', () => {
    setPatchTextBudgetForTest(1600);
    const { parser, source } = stream(patchOf(20, 6), 160);
    expect(source.release).not.toHaveBeenCalled();
    parser.store.dispose();
    expect(source.release).toHaveBeenCalledTimes(1);
  });

  // Pinned text the store does not hold is not counted: the budget is
  // met from held text whether or not a reader waits for evicted text.
  it('counts no pin of text that is evicted while a reader waits for it', async () => {
    const base = patchTextHeldBytes();
    setPatchTextBudgetForTest(1600);
    const waiting = stream(patchOf(20, 6), 160);
    const last = waiting.files[19];
    expect(last.body.resident(0, 1)).toBe(false);
    // The text the reader waits for never arrives during the test.
    waiting.source.read.mockImplementation(() => new Promise(() => {}));
    void last.body.whenResident(() => undefined);
    await Promise.resolve();
    expect(waiting.source.read).toHaveBeenCalled();
    const held = patchTextHeldBytes() - base;

    // Another diff arrives: the held text stays within the budget.
    stream(patchOf(20, 6), 160);
    expect(patchTextHeldBytes() - base).toBeLessThanOrEqual(Math.max(held, 1600));
  });

  // A chunk of a line still arriving that a reader also pins is counted
  // once, so the budget is not raised past what is pinned.
  it('counts a chunk pinned both by the line still arriving and by a reader once', () => {
    const base = patchTextHeldBytes();
    setPatchTextBudgetForTest(160);
    const arriving = new PatchParser();
    disposers.push(() => arriving.store.dispose());
    arriving.store.attachSource({ read: vi.fn(), release: vi.fn() });
    const line = `+${'z'.repeat(5000)}`;
    arriving.append(line.slice(0, 2500), { offset: 0, nextOffset: 2500 });
    arriving.append(line.slice(2500), { offset: 2500, nextOffset: 5001 });
    // The line still arriving, read by a reader that pins it.
    const release = arriving.store.pin(0, 1);
    disposers.push(release);

    stream(patchOf(40, 6), 160);
    // Pinned text and the budget's worth of the rest.
    expect(patchTextHeldBytes() - base).toBeLessThanOrEqual(5001 + 160);
  });

  it('gives back the pin of a line still arriving when its store is disposed', () => {
    const base = patchTextHeldBytes();
    setPatchTextBudgetForTest(160);
    const abandoned = new PatchParser();
    abandoned.store.attachSource({ read: vi.fn(), release: vi.fn() });
    abandoned.append(`+${'z'.repeat(5000)}`, { offset: 0, nextOffset: 5001 });
    abandoned.store.dispose();

    stream(patchOf(20, 6), 160);
    expect(patchTextHeldBytes() - base).toBeLessThanOrEqual(160);
  });

  // Text built in memory (expanded context, conflict files) is not
  // counted, so pinning it does not raise the limit the rest is held to.
  it('meets the budget from read text while text it never counted is pinned', async () => {
    const base = patchTextHeldBytes();
    setPatchTextBudgetForTest(1600);
    const built = PatchStore.fromLines(Array.from({ length: 200 }, (_, index) => ({ content: `+built ${index}`.padEnd(40, '.'), kind: LINE_ADD })));
    disposers.push(() => built.dispose());
    expect(patchTextHeldBytes()).toBe(base);
    const release = built.pin(0, built.lineCount);
    disposers.push(release);

    const { files } = stream(patchOf(20, 6), 160);
    for (const file of files) await load(file);
    expect(patchTextHeldBytes() - base).toBeLessThanOrEqual(1600);
  });

  it('counts text past Latin-1 at two bytes a character', () => {
    const base = patchTextHeldBytes();
    const parser = new PatchParser();
    disposers.push(() => parser.store.dispose());
    parser.append('+✓\n');
    expect(patchTextHeldBytes() - base).toBe(6);
    parser.append('+é\n');
    expect(patchTextHeldBytes() - base).toBe(9);
  });
});


describe('packed patch text past the memory budget', () => {
  // patchOf with a character of every UTF-8 length on every 30th added
  // line: sparse enough for a 2000-byte chunk to be packed.
  function widePatchOf(files: number, lines: number): string {
    return patchOf(files, lines).replace(/^\+(\d+):(\d+)\./gm, (line, file: string, at: string) => (Number(at) % 30 === 0 ? `+${file}:${at}é✓😀` : line));
  }
  const SIZE = 2000;
  const BUDGET = 2500;

  it('counts a packed chunk at its length', () => {
    const base = patchTextHeldBytes();
    const parser = new PatchParser();
    disposers.push(() => parser.store.dispose());
    const check = packText('+✓\n');
    parser.append(check, { offset: 0, nextOffset: check.length, wide: [1], key: textKey('+✓\n') });
    expect(patchTextHeldBytes() - base).toBe(5);
    const accent = packText('+é\n');
    parser.append(accent, { offset: 5, nextOffset: 5 + accent.length, wide: [1], key: textKey('+✓\n+é\n') });
    expect(patchTextHeldBytes() - base).toBe(9);
  });

  it('reads each evicted chunk again in the form it arrived in', async () => {
    setPatchTextBudgetForTest(BUDGET);
    const patch = widePatchOf(12, 60);
    const { files, source, spans } = stream(patch, SIZE, 'packed');
    const reference = parsePatchFiles(patch);
    expect(files.map((file) => file.path)).toEqual(reference.map((file) => file.path));
    expect(files.some((file) => !resident(file))).toBe(true);
    expect(spans.filter((span) => span.wide !== undefined).length).toBeGreaterThan(2);

    // Last to first, so the first chunk is evicted and read again too.
    for (let index = files.length - 1; index >= 0; index -= 1) {
      const file = files[index];
      expect(await file.body.whenResident(() => file.body.toPatchLines()), `${index}`).toEqual(reference[index].lines);
    }
    for (let index = 0; index < files.length; index += 1) {
      const file = files[index];
      expect(await file.body.whenResident(() => file.body.toPatchLines()), `${index}`).toEqual(reference[index].lines);
    }
    const forms = source.read.mock.calls.map(([offset, , packed]) => [packed, spans.find((span) => span.offset === offset)!.wide !== undefined]);
    expect(forms.every(([asked, arrived]) => asked === arrived)).toBe(true);
    expect(new Set(forms.map(([asked]) => asked))).toEqual(new Set([true, false]));
  });

  it('treats a packed reread of another length as lost', async () => {
    setPatchTextBudgetForTest(BUDGET);
    const { files, source } = stream(widePatchOf(20, 60), SIZE, 'packed');
    source.read.mockImplementation(async (_offset, nextOffset, packed) => ({ data: 'x', nextOffset, packed }));
    await expect(load(files[19])).rejects.toThrow('the diff no longer serves the same chunk');
  });

  it('treats a packed chunk read again as text as lost', async () => {
    setPatchTextBudgetForTest(BUDGET);
    const { files, source, spans } = stream(widePatchOf(20, 60), SIZE, 'packed');
    expect(spans[spans.length - 1].wide).toBeDefined();
    const inner = source.read.getMockImplementation()!;
    source.read.mockImplementation(async (offset, nextOffset, packed) => ({ ...(await inner(offset, nextOffset, packed)), packed: false }));
    await expect(load(files[19])).rejects.toThrow('the diff no longer serves the same chunk');
  });
});
