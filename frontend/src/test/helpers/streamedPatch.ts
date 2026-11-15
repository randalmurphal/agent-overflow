import { vi, type Mock } from 'vitest';
import { appendFNV1a32, FNV_OFFSET_BASIS_32, fnv1a32 } from '../../lib/utils/fnv1a';
import type { PatchTextSource } from '../../lib/utils/patchChunks';
import { PatchParser, type ReviewFile } from '../../lib/utils/patchParser';
import type { PatchChunkSpan, PatchTextKey } from '../../lib/utils/patchStore';

// The one fake review diff backend: a patch served as the backend serves
// it (internal/gitdiff, internal/jstext), so a test reads chunks only of
// the shapes production receives.
//
// - Offsets are bytes of the patch's UTF-8. A read of `maxBytes` from an
//   offset takes the rest when it fits, else ends after the window's last
//   newline, or, inside a line longer than the window, between characters
//   (gitdiff.ChunkCut). The backend's window is never shorter than a
//   character (MinChunkBytes); one here takes the whole character.
// - Text is what a JSON client receives: each byte that does not begin a
//   valid UTF-8 sequence is one U+FFFD.
// - Every chunk carries the key of the text through its end, unless the
//   backend predates keys.

export type StreamedChunk = PatchChunkSpan & { data: string; eof: boolean };

export interface StreamedPatch {
  parser: PatchParser;
  files: ReviewFile[];
  source: PatchTextSource & { read: Mock<PatchTextSource['read']>; release: Mock<PatchTextSource['release']> };
  spans: StreamedChunk[];
}

/** The key the backend answers for `text`, the patch text from offset 0
 * to a chunk's end. */
export function textKey(text: string): PatchTextKey {
  return { hash: fnv1a32(text), units: text.length };
}

/** The chunk as a backend older than the key answers it. */
export function unkeyed(chunk: StreamedChunk): StreamedChunk {
  const { key: _key, ...rest } = chunk;
  return rest;
}

// A lead byte's sequence length and the range of the byte after it
// (Unicode table 3-7); length 0 for a byte no sequence starts with.
function leadShape(lead: number): [length: number, low: number, high: number] {
  if (lead < 0x80) return [1, 0, 0];
  if (lead >= 0xc2 && lead <= 0xdf) return [2, 0x80, 0xbf];
  if (lead === 0xe0) return [3, 0xa0, 0xbf];
  if (lead === 0xed) return [3, 0x80, 0x9f];
  if (lead >= 0xe1 && lead <= 0xef) return [3, 0x80, 0xbf];
  if (lead === 0xf0) return [4, 0x90, 0xbf];
  if (lead >= 0xf1 && lead <= 0xf3) return [4, 0x80, 0xbf];
  if (lead === 0xf4) return [4, 0x80, 0x8f];
  return [0, 0, 0];
}

// How many bytes from `at`, below `end`, follow the sequence at `at` as
// a valid one would.
function validRun(bytes: Uint8Array, at: number, end: number): number {
  const [length, low, high] = leadShape(bytes[at]);
  let next = at + 1;
  for (; next < Math.min(end, at + length); next += 1) {
    const byte = bytes[next];
    if (next === at + 1 ? byte < low || byte > high : (byte & 0xc0) !== 0x80) break;
  }
  return next - at;
}

// The length of the valid UTF-8 sequence at `at`, or 0 when the byte
// there does not begin one (Go's utf8.DecodeRune answering width 1).
function sequenceAt(bytes: Uint8Array, at: number): number {
  const length = leadShape(bytes[at])[0];
  return length > 0 && validRun(bytes, at, bytes.length) === length ? length : 0;
}

// Whether bytes [at, end) are a proper prefix of a valid sequence: Go's
// utf8.FullRune answering false.
function unfinished(bytes: Uint8Array, at: number, end: number): boolean {
  const length = leadShape(bytes[at])[0];
  return end - at < length && validRun(bytes, at, end) === end - at;
}

/** Where a read of `maxBytes` from `offset` ends, as the backend's
 * stream cuts it: the rest when it fits in the window, else
 * gitdiff.ChunkCut of the window. The vectors in chunkCutVectors.json
 * hold Go's cut to the same answers. */
export function chunkCut(bytes: Uint8Array, offset: number, maxBytes: number): number {
  if (bytes.length - offset <= maxBytes) return bytes.length;
  const end = offset + maxBytes;
  const newline = bytes.subarray(offset, end).lastIndexOf(10);
  if (newline >= 0) return offset + newline + 1;
  // stringsx.CompleteRunePrefix: before a trailing unfinished sequence,
  // unless the window is that sequence.
  for (let back = 1; back < 4 && back <= maxBytes; back += 1) {
    const start = end - back;
    if ((bytes[start] & 0xc0) === 0x80) continue;
    if (start > offset && unfinished(bytes, start, end)) return start;
    break;
  }
  return end;
}

const decoder = new TextDecoder('utf-8', { ignoreBOM: true });

// The text a JSON client receives for bytes [from, to).
function jsonText(bytes: Uint8Array, from: number, to: number): string {
  let out = '';
  let run = from;
  for (let at = from; at < to;) {
    const length = sequenceAt(bytes, at);
    if (length > 0) {
      at += length;
      continue;
    }
    out += decoder.decode(bytes.subarray(run, at)) + '\ufffd';
    at += 1;
    run = at;
  }
  return out + decoder.decode(bytes.subarray(run, to));
}

/** A patch served by byte offset as the backend serves it. */
export interface FakeDiff {
  /** Its bytes. */
  readonly bytes: Uint8Array;
  /** ReadReviewDiff from `offset`. `keyed` false is a backend older than
   * the key. */
  read(offset: number, maxBytes: number, keyed?: boolean): StreamedChunk;
}

/** A fake backend serving `patch`, text or raw bytes (which may hold
 * invalid UTF-8). */
export function fakeDiff(patch: string | Uint8Array): FakeDiff {
  const bytes = typeof patch === 'string' ? new TextEncoder().encode(patch) : patch;
  // The key at each boundary read so far.
  const keys = new Map<number, PatchTextKey>([[0, { hash: FNV_OFFSET_BASIS_32, units: 0 }]]);
  return {
    bytes,
    read(offset, maxBytes, keyed = true) {
      // The backend's window is never shorter than a character; one here
      // is widened to the character at the offset.
      const next = chunkCut(bytes, offset, Math.max(1, maxBytes, sequenceAt(bytes, offset)));
      const text = jsonText(bytes, offset, next);
      const before = keys.get(offset) ?? textKey(jsonText(bytes, 0, offset));
      const key = { hash: appendFNV1a32(before.hash, text), units: before.units + text.length };
      keys.set(next, key);
      const eof = next === bytes.length;
      return keyed ? { data: text, key, offset, nextOffset: next, eof } : { data: text, offset, nextOffset: next, eof };
    },
  };
}

/** The chunks a patch streams as, each read with a window of `size`
 * bytes. */
export function patchChunks(patch: string, size: number | ((index: number) => number)): StreamedChunk[] {
  const diff = fakeDiff(patch);
  const chunks: StreamedChunk[] = [];
  for (let offset = 0, index = 0; offset < diff.bytes.length; index += 1) {
    const chunk = diff.read(offset, typeof size === 'number' ? size : size(index));
    chunks.push(chunk);
    offset = chunk.nextOffset;
  }
  return chunks;
}

/** Streams a patch into a parser whose store reads evicted chunks again
 * from the fake backend, as ReviewDiffSource does. Set the budget with
 * setPatchTextBudgetForTest before streaming, and dispose the store. */
export function streamPatch(patch: string, size: number | ((index: number) => number)): StreamedPatch {
  const diff = fakeDiff(patch);
  const spans = patchChunks(patch, size);
  const source = {
    // The same offset and a window no larger than the first read's cut
    // the same chunk.
    read: vi.fn<PatchTextSource['read']>(async (offset, nextOffset) => {
      const chunk = diff.read(offset, nextOffset - offset);
      return { data: chunk.data, nextOffset: chunk.nextOffset };
    }),
    release: vi.fn<PatchTextSource['release']>(),
  };
  const parser = new PatchParser();
  parser.store.attachSource(source);
  for (const span of spans) parser.append(span.data, span);
  parser.end();
  return { parser, files: parser.files, source, spans };
}
