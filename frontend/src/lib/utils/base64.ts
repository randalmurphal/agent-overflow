/**
 * Decode a base64 string into its raw bytes.
 *
 * `atob` yields a "binary string" where each UTF-16 code unit holds one
 * decoded byte (0–255); copying those into a `Uint8Array` recovers the
 * original bytes exactly. We go byte-wise rather than through `TextDecoder`
 * because the input is arbitrary binary — terminal PTY output, image data —
 * not necessarily valid UTF-8, and a text decoder would corrupt it.
 *
 * Returns the precise `Uint8Array<ArrayBuffer>` (not the wider
 * `ArrayBufferLike` default) so callers that need an `ArrayBuffer`-backed
 * view — e.g. `new Blob([...])`, which rejects `SharedArrayBuffer` — type
 * cleanly without a cast.
 */
export function base64ToBytes(b64: string): Uint8Array<ArrayBuffer> {
  const binary = atob(b64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) {
    bytes[i] = binary.charCodeAt(i);
  }
  return bytes;
}

/** Encode bytes as base64, a block at a time so a large buffer does not
 * overflow the argument list of String.fromCharCode. */
export function bytesToBase64(bytes: Uint8Array): string {
  const BLOCK = 0x8000;
  let binary = '';
  for (let i = 0; i < bytes.length; i += BLOCK) {
    binary += String.fromCharCode(...bytes.subarray(i, i + BLOCK));
  }
  return btoa(binary);
}
