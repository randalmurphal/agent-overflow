// Packed patch text (`ReadReviewDiffPacked`): a chunk's UTF-8 bytes as a
// string of one character per byte (U+0000..U+00FF), which an engine
// holds at one byte per character. An ASCII byte is its own character, so
// an ASCII line reads as it is. A line holding a byte past ASCII (a wide
// line) is decoded when it is read. The backend names each wide line of a
// chunk by the index of its first such byte (`wide`, ascending), so
// nothing here scans for them.

const decoder = new TextDecoder('utf-8', { ignoreBOM: true });

// Lines up to this many bytes decode through one shared buffer; a longer
// one gets its own, so a long minified line is not kept after its read.
const SCRATCH_BYTES = 64 * 1024;
const scratch = new Uint8Array(SCRATCH_BYTES);

/** The text packed UTF-8 bytes encode. Each invalid sequence decodes to
 * U+FFFD as TextDecoder replaces it; a leading BOM is kept. */
export function unpackText(packed: string): string {
  const length = packed.length;
  const bytes = length <= SCRATCH_BYTES ? scratch : new Uint8Array(length);
  for (let index = 0; index < length; index += 1) bytes[index] = packed.charCodeAt(index);
  return decoder.decode(bytes.subarray(0, length));
}

/** Throws unless `wide` can be a packed chunk's marks: ascending indexes
 * of bytes past ASCII in `data`. A mark that is not would index the wrong
 * lines as wide. */
export function checkWideMarks(data: string, wide: readonly number[]): void {
  let last = -1;
  for (let mark = 0; mark < wide.length; mark += 1) {
    const at = wide[mark];
    // charCodeAt answers NaN outside the text.
    if (!Number.isInteger(at) || at <= last || !(data.charCodeAt(at) >= 0x80)) {
      throw new Error('patch text: a packed chunk has a wide mark that is not at a byte past ASCII');
    }
    last = at;
  }
}
