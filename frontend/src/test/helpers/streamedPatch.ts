import { vi, type Mock } from 'vitest';
import { appendFNV1a32, fnv1a32 } from '../../lib/utils/fnv1a';
import { PatchParser, type PatchChunkSpan, type PatchTextKey, type PatchTextSource, type ReviewFile } from '../../lib/utils/patchStore';

// A patch streamed into compact storage the way ReviewDiffSource streams
// it: chunk by chunk into a store that can read an evicted chunk again
// from its source. Set the budget with setPatchTextBudgetForTest before
// streaming, and dispose the store.
//
// The form says how chunks arrive. `plain`: every chunk is text and
// offsets are characters. `packed`: every chunk is packed
// (ReadReviewDiffPacked: the UTF-8 bytes, one character each, and the
// wide-line marks), offsets are bytes, and a cut can fall inside a
// character. `plain-first`: the first chunk is text, cut back to a
// character boundary, and the rest packed.

export type PatchForm = 'plain' | 'packed' | 'plain-first';

export interface StreamedChunk extends PatchChunkSpan {
  data: string;
}

export interface StreamedPatch {
  parser: PatchParser;
  files: ReviewFile[];
  source: PatchTextSource & { read: Mock<PatchTextSource['read']>; release: Mock<PatchTextSource['release']> };
  spans: StreamedChunk[];
}

/** UTF-8 bytes of `text`, one character per byte. */
export function packText(text: string): string {
  let out = '';
  for (const byte of new TextEncoder().encode(text)) out += String.fromCharCode(byte);
  return out;
}

/** For each line of packed `data` (a run between newlines) holding a byte
 * past ASCII, the index of the first such byte. */
export function wideMarks(data: string): number[] {
  const marks: number[] = [];
  let marked = false;
  for (let index = 0; index < data.length; index += 1) {
    const code = data.charCodeAt(index);
    if (code === 10) marked = false;
    else if (code >= 0x80 && !marked) {
      marks.push(index);
      marked = true;
    }
  }
  return marks;
}

/** The source key the backend answers on a chunk: the key of `text`, the
 * patch text from offset 0 to the chunk's end. */
export function textKey(text: string): PatchTextKey {
  return { hash: fnv1a32(text), units: text.length };
}

/** The chunks a patch arrives as, in `form`, of `size` characters or
 * bytes each, each with its source key. A packed chunk cut inside a
 * character, which the backend never answers, is keyed by its bytes'
 * prefix decoded, so the last chunk's key is the whole patch's. */
export function patchChunks(patch: string, size: number | ((index: number) => number), form: PatchForm = 'plain'): StreamedChunk[] {
  const sizeAt = (index: number) => Math.max(1, typeof size === 'number' ? size : size(index));
  const chunks: StreamedChunk[] = [];
  if (form === 'plain') {
    let hash = fnv1a32('');
    for (let offset = 0, index = 0; offset < patch.length; index += 1) {
      const next = Math.min(patch.length, offset + sizeAt(index));
      const data = patch.slice(offset, next);
      hash = appendFNV1a32(hash, data);
      chunks.push({ data, offset, nextOffset: next, key: { hash, units: next } });
      offset = next;
    }
    return chunks;
  }
  const encoded = new TextEncoder().encode(patch);
  const prefixKey = (end: number) => textKey(new TextDecoder('utf-8', { ignoreBOM: true }).decode(encoded.subarray(0, end)));
  const bytes = packText(patch);
  let offset = 0;
  let index = 0;
  if (form === 'plain-first' && bytes.length > 0) {
    let next = Math.min(bytes.length, sizeAt(0));
    while (next < bytes.length && (bytes.charCodeAt(next) & 0xc0) === 0x80) next += 1;
    const raw = Uint8Array.from(bytes.slice(0, next), (char) => char.charCodeAt(0));
    chunks.push({ data: new TextDecoder('utf-8', { ignoreBOM: true }).decode(raw), offset: 0, nextOffset: next, key: prefixKey(next) });
    offset = next;
    index = 1;
  }
  for (; offset < bytes.length; index += 1) {
    const next = Math.min(bytes.length, offset + sizeAt(index));
    const data = bytes.slice(offset, next);
    chunks.push({ data, offset, nextOffset: next, wide: wideMarks(data), key: prefixKey(next) });
    offset = next;
  }
  return chunks;
}

/** The chunk as a backend older than the key answers it. */
export function unkeyed(chunk: StreamedChunk): StreamedChunk {
  const { key: _key, ...rest } = chunk;
  return rest;
}

export function streamPatch(patch: string, size: number | ((index: number) => number), form: PatchForm = 'plain'): StreamedPatch {
  const spans = patchChunks(patch, size, form);
  const source = {
    // Serves a chunk again in the form it first had; asking another form
    // is a bug in the reader.
    read: vi.fn<PatchTextSource['read']>(async (offset, nextOffset, packed) => {
      const span = spans.find((candidate) => candidate.offset === offset);
      if (!span || span.nextOffset !== nextOffset) throw new Error(`unexpected read ${offset}-${nextOffset}`);
      if (packed !== (span.wide !== undefined)) throw new Error(`unexpected form of read ${offset}-${nextOffset}`);
      return { data: span.data, nextOffset, packed: span.wide !== undefined };
    }),
    release: vi.fn<PatchTextSource['release']>(),
  };
  const parser = new PatchParser();
  parser.store.attachSource(source);
  for (const span of spans) parser.append(span.data, span);
  parser.end();
  return { parser, files: parser.files, source, spans };
}
