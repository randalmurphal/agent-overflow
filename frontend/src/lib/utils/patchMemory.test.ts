import { afterEach, describe, expect, it, vi } from 'vitest';
import { patchMemory, patchTextHeldBytes, patchTextVersion, setPatchTextBudgetForTest } from './patchMemory.svelte';
import { parsePatchFiles } from './patchFiles';
import {
  PatchParser,
  PatchTextLost,
  PatchTextNotResident,
  parseReviewFiles,
  whenResident,
  type ReviewFile,
} from './patchStore';
import { streamPatch, type StreamedPatch } from '../../test/helpers/streamedPatch';

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

function stream(patch: string, size: number | ((index: number) => number)): StreamedPatch {
  const streamed = streamPatch(patch, size);
  disposers.push(() => streamed.parser.store.dispose());
  return streamed;
}

function resident(file: ReviewFile): boolean {
  return file.body.resident();
}

describe('patch text past the memory budget', () => {
  it('keeps the beginning of a diff and gives up the text that arrived last', () => {
    const base = patchTextHeldBytes();
    setPatchTextBudgetForTest(1600);
    const { files, parser } = stream(patchOf(20, 6), 160);

    expect(patchTextHeldBytes() - base).toBeLessThanOrEqual(1600);
    expect(parser.store.evictedAny).toBe(true);
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
    expect(source.read.mock.calls).toEqual(lastSpans.map((span) => [span.offset, span.nextOffset]));
    // The text it read counts, and the budget holds once nothing pins it.
    expect(patchTextHeldBytes() - base).toBeLessThanOrEqual(1600 + reference[0].body.segments[0].store.totalChars);
  });

  it('gives up text nobody read before text that was read, least recently read first', async () => {
    setPatchTextBudgetForTest(1600);
    const { files } = stream(patchOf(20, 6), 160);
    // Ten chunks fit; the first holds f000 and part of f001.
    expect(resident(files[0])).toBe(true);
    const lastResident = files.map(resident).lastIndexOf(true);
    expect(lastResident).toBeGreaterThan(1);

    files[0].body.text(2);
    await files[19].body.loadLines(0, files[19].body.lineCount);

    expect(resident(files[0])).toBe(true);
    expect(resident(files[19])).toBe(true);
    // Room for the reread came from the newest text nobody read.
    expect(resident(files[lastResident])).toBe(false);
  });

  it('gives up the least recently read text once no unread text is left', async () => {
    const patch = patchOf(20, 6);
    setPatchTextBudgetForTest(1_000_000);
    const { files } = stream(patch, 175);
    // Every file is one 175-character chunk; reading them makes each one
    // read text, in this order.
    for (const index of [15, 17, 19]) files[index].body.text(4);
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
    source.read.mockImplementation((offset, nextOffset) =>
      new Promise((resolve) => { releases.push(() => resolve(inner(offset, nextOffset))); }));

    const file = files[10];
    const first = file.body.loadLines(0, file.body.lineCount);
    const again = file.body.loadLines(0, file.body.lineCount);
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
    expect(patchTextHeldBytes() - base - reference[0].body.segments[0].store.totalChars).toBeLessThanOrEqual(160);
  });

  it('reports text it can no longer read once, and stops asking for it', async () => {
    setPatchTextBudgetForTest(160);
    const { files, source } = stream(patchOf(20, 6), 160);
    source.read.mockRejectedValue(new Error('review diff: not open'));
    const lost = vi.fn();
    files[0].body.segments[0].store.whenLost(lost);

    await expect(files[19].body.loadLines(0, 1)).rejects.toBeInstanceOf(PatchTextLost);
    await expect(files[18].body.loadLines(0, 1)).rejects.toBeInstanceOf(PatchTextLost);
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
    source.read.mockImplementation(async (_offset: number, nextOffset: number) => ({ data: 'x'.repeat(160), nextOffset: nextOffset + 1 }));
    await expect(files[19].body.loadLines(0, 1)).rejects.toThrow('the diff no longer serves the same chunk');
  });

  it('bumps the text version when evicted text is back', async () => {
    setPatchTextBudgetForTest(160);
    const { files } = stream(patchOf(20, 6), 160);
    const before = patchTextVersion();
    await files[19].body.loadLines(0, 1);
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

  it('a disposed store reads nothing again', async () => {
    setPatchTextBudgetForTest(160);
    const { parser, files, source } = stream(patchOf(20, 6), 160);
    parser.store.dispose();
    await expect(files[19].body.loadLines(0, 1)).rejects.toBeInstanceOf(PatchTextLost);
    expect(source.read).not.toHaveBeenCalled();
  });

  it('a detached store keeps all of its text whatever the budget does', () => {
    const { parser, source } = stream(patchOf(4, 6), 160);
    expect(parser.store.evictedAny).toBe(false);
    expect(parser.store.detachSource()).toBe(source);

    setPatchTextBudgetForTest(1);
    stream(patchOf(4, 6), 160);
    expect(parser.files.every(resident)).toBe(true);
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
