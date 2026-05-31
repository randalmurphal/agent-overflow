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
