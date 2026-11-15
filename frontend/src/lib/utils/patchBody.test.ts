import { afterEach, describe, expect, it, vi } from 'vitest';
import { PatchBody, type BodySegment } from './patchBody';
import { setPatchTextBudgetForTest } from './patchMemory.svelte';
import { parseReviewFiles } from './patchParser';
import { LINE_ADD, LINE_CONTEXT, PatchStore } from './patchStore';
import { streamPatch } from '../../test/helpers/streamedPatch';
import { UNICODE_PATCH } from '../../test/helpers/patchFixtures';

const disposers: (() => void)[] = [];

afterEach(() => {
  for (const dispose of disposers.splice(0)) dispose();
  setPatchTextBudgetForTest(null);
});

describe('PatchBody', () => {
  it('reads lines one segment at a time', () => {
    const [changed] = parseReviewFiles([
      'diff --git a/t b/t', 'deleted file mode 100644', '--- a/t', '+++ /dev/null', '@@ -1,1 +0,0 @@', '-gone',
      'diff --git a/t b/t', 'new file mode 120000', '--- /dev/null', '+++ b/t', '@@ -0,0 +1,1 @@', '+came', '',
    ].join('\n'));
    const body = changed.body;
    expect(body.segments).toHaveLength(2);
    const first = body.segmentEnd(0);
    expect(first).toBe(6);
    expect(body.segmentEnd(first)).toBe(body.lineCount);
    expect(body.segmentText(first - 1, first)).toBe('-gone');
    expect(body.segmentText(first, body.lineCount)).toBe('@@ -0,0 +1,1 @@\n+came');
    expect(() => body.segmentText(first - 1, first + 1)).toThrow(RangeError);
    expect(() => body.segmentEnd(body.lineCount)).toThrow(RangeError);
    expect(() => body.segmentEnd(-1)).toThrow(RangeError);
  });

  // Positions are bytes of the segment's UTF-8, the patch's own offsets.
  it('places each line at its UTF-8 bytes in its segment', () => {
    const encoder = new TextEncoder();
    for (const size of [7, 300, 16 * 1024]) {
      const streamed = streamPatch(UNICODE_PATCH, size);
      disposers.push(() => streamed.parser.store.dispose());
      const patch = encoder.encode(UNICODE_PATCH);
      for (const file of streamed.files) {
        const body = file.body;
        const text = body.segmentText(0, body.segmentEnd(0));
        expect(encoder.encode(text).length, `${size} ${file.path}`).toBe(body.storedLineEnd(body.segmentEnd(0) - 1) - body.storedLineStart(0));
        for (let index = 0; index < body.segmentEnd(0); index += 1) {
          const line = patch.subarray(body.storedLineStart(index), body.storedLineEnd(index));
          expect(new TextDecoder('utf-8', { ignoreBOM: true }).decode(line), `${size} ${file.path} ${index}`).toBe(body.text(index));
          expect(body.lineSize(index)).toBe(line.length);
        }
        expect(body.textSize, `${size} ${file.path}`).toBe(encoder.encode(body.patchText()).length);
      }
    }
  });

  // A body joined from several stores, as context expansion builds one:
  // each question about a range is the same as asking each line's store.
  it('answers residency and segment ends across segments of several stores', () => {
    setPatchTextBudgetForTest(400);
    const lines = Array.from({ length: 60 }, (_, index) => `+line ${String(index).padStart(3, '0')} ${'.'.repeat(20)}`);
    const streamed = streamPatch(['diff --git a/x b/x', '--- a/x', '+++ b/x', '@@ -0,0 +1,60 @@', ...lines, ''].join('\n'), 120);
    disposers.push(() => streamed.parser.store.dispose());
    const evictable = streamed.parser.store;
    const side = PatchStore.fromLines([
      { content: '@@ -3,2 +4,2 @@ head', kind: { oldStart: 3, newStart: 4 } },
      { content: ' kept', kind: LINE_CONTEXT },
      { content: '+added', kind: LINE_ADD },
    ]);
    const segments: BodySegment[] = [
      { store: side, start: 0, end: 2 },
      { store: evictable, start: 4, end: 20 },
      { store: side, start: 2, end: 3 },
      { store: evictable, start: 40, end: 64 },
      { store: side, start: 1, end: 3 },
    ];
    const body = new PatchBody(segments);
    // Each line's store and line, by walking the segments.
    const placed = segments.flatMap((segment) => Array.from({ length: segment.end - segment.start }, (_, offset) => ({ segment, line: segment.start + offset })));
    expect(body.lineCount).toBe(placed.length);
    const lineResident = placed.map(({ segment, line }) => segment.store.resident(line, line + 1));
    expect(lineResident).toContain(true);
    expect(lineResident).toContain(false);
    for (let start = 0; start <= body.lineCount; start += 1) {
      for (let end = start; end <= body.lineCount; end += 1) {
        expect(body.resident(start, end), `${start}-${end}`).toBe(lineResident.slice(start, end).every(Boolean));
      }
      if (start < body.lineCount) {
        const segment = placed[start].segment;
        let end = start;
        while (end < placed.length && placed[end].segment === segment) end += 1;
        expect(body.segmentEnd(start), `${start}`).toBe(end);
      }
    }
    expect(body.resident()).toBe(false);
    expect(body.kind(0)).toBe(side.kind(0));
    expect(body.hunkNewStart(0)).toBe(4);
  });

  it('pins a range in each store it spans, and releases each pin once however often its release runs', () => {
    const first = PatchStore.fromLines([{ content: ' a', kind: LINE_CONTEXT }, { content: '+b', kind: LINE_ADD }]);
    const second = PatchStore.fromLines([{ content: '+c', kind: LINE_ADD }, { content: '+d', kind: LINE_ADD }]);
    const body = new PatchBody([{ store: first, start: 0, end: 2 }, { store: second, start: 0, end: 2 }]);
    const real = PatchStore.prototype.pin;
    const pinned: [PatchStore, number, number][] = [];
    let releases = 0;
    const pin = vi.spyOn(PatchStore.prototype, 'pin').mockImplementation(function (this: PatchStore, start: number, end: number) {
      pinned.push([this, start, end]);
      const release = real.call(this, start, end);
      return () => {
        releases += 1;
        release();
      };
    });
    try {
      const release = body.pin(1, 3);
      expect(pinned).toEqual([[first, 1, 2], [second, 0, 1]]);
      release();
      release();
      expect(releases).toBe(2);
    } finally {
      pin.mockRestore();
    }
  });

  it('loads only what is evicted, and keeps what is pinned', async () => {
    setPatchTextBudgetForTest(300);
    const lines = Array.from({ length: 60 }, (_, index) => `+line ${String(index).padStart(3, '0')} ${'.'.repeat(20)}`);
    const streamed = streamPatch(['diff --git a/x b/x', '--- a/x', '+++ b/x', '@@ -0,0 +1,60 @@', ...lines, ''].join('\n'), 120);
    disposers.push(() => streamed.parser.store.dispose());
    const body = streamed.files[0].body;
    expect(body.resident(4, 20)).toBe(false);
    const release = body.pin(4, 20);
    await body.load(4, 20);
    expect(body.resident(4, 20)).toBe(true);
    expect(body.load(4, 20)).toBeNull();
    // Reading elsewhere would evict the range unless it is pinned.
    await body.whenResident(() => undefined, 40, 64);
    expect(body.resident(4, 20)).toBe(true);
    release();
    expect(body.resident(4, 20)).toBe(false);
  });

  it('keys the highlighter as fnv1a.contentKey does: `<length>:<hash base36>`', () => {
    const [file] = parseReviewFiles('diff --git a/x b/x\n+é ✓');
    expect(file.body.patchText()).toBe('diff --git a/x b/x\n+é ✓');
    expect(file.body.contentKey()).toBe('23:1ftrdto');
  });
});
