// UTF-8 positions in JS strings. A streaming row's wire positions (a delta's
// `offset`, a row's `streamEnd`) count the UTF-8 bytes of the text the
// backend persisted, which is valid UTF-8, so every position falls on a
// character boundary.

/**
 * The UTF-8 byte length of `text`. A surrogate pair is one four-byte
 * character, counted at its high half, so a string split inside a pair
 * still sums to the whole.
 */
export function utf8Length(text: string): number {
  let bytes = 0;
  for (let index = 0; index < text.length; index += 1) {
    const unit = text.charCodeAt(index);
    if (unit < 0x80) bytes += 1;
    else if (unit < 0x800) bytes += 2;
    else if (unit >= 0xd800 && unit <= 0xdbff) bytes += 4;
    else if (unit < 0xdc00 || unit > 0xdfff) bytes += 3;
  }
  return bytes;
}

/**
 * The code-unit index at which the first `bytes` UTF-8 bytes of `text`
 * end, or null when that position is inside a character or past the end.
 */
export function utf16IndexAtUtf8(text: string, bytes: number): number | null {
  let remaining = bytes;
  let index = 0;
  while (remaining > 0 && index < text.length) {
    const unit = text.charCodeAt(index);
    if (unit >= 0xd800 && unit <= 0xdbff) {
      remaining -= 4;
      index += 2;
    } else {
      remaining -= unit < 0x80 ? 1 : unit < 0x800 ? 2 : 3;
      index += 1;
    }
  }
  return remaining === 0 && index <= text.length ? index : null;
}

/**
 * The part of a streamed chunk at `offset` that lies past `end`: the whole
 * chunk when it starts at `end`, its unseen suffix when it overlaps `end`,
 * and '' when `end` already covers it. Null when the chunk starts past
 * `end`, which is text this holder never received, or when `end` falls
 * inside one of its characters.
 */
export function streamedTextPast(chunk: string, offset: number, end: number): string | null {
  if (offset === end) return chunk;
  if (offset > end) return null;
  const covered = end - offset;
  const bytes = utf8Length(chunk);
  if (covered >= bytes) return '';
  const index = utf16IndexAtUtf8(chunk, covered);
  return index === null ? null : chunk.slice(index);
}
