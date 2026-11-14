import { describe, expect, it } from 'vitest';
import { base64ToBytes, bytesToBase64 } from './base64';

describe('base64ToBytes', () => {
  it('decodes ASCII base64 to its bytes', () => {
    // btoa('Hi') === 'SGk='
    expect(Array.from(base64ToBytes('SGk='))).toEqual([0x48, 0x69]);
  });

  it('preserves arbitrary non-UTF-8 bytes (raw binary, not text)', () => {
    // 0xFF and a lone 0x80 continuation byte would corrupt under TextDecoder;
    // the byte-wise copy must hand them back untouched.
    const bytes = new Uint8Array([0x00, 0xff, 0xe2, 0x94, 0x80, 0x1b]);
    const b64 = btoa(String.fromCharCode(...bytes));
    expect(Array.from(base64ToBytes(b64))).toEqual(Array.from(bytes));
  });

  it('returns an empty array for empty input', () => {
    expect(base64ToBytes('')).toEqual(new Uint8Array(0));
  });
});

describe('bytesToBase64', () => {
  it('round-trips every byte value across block boundaries', () => {
    const bytes = new Uint8Array(0x8000 * 2 + 3);
    for (let i = 0; i < bytes.length; i += 1) bytes[i] = (i * 7) & 0xff;
    const encoded = bytesToBase64(bytes);
    expect(encoded).toBe(btoa(Array.from(bytes, (byte) => String.fromCharCode(byte)).join('')));
    expect(base64ToBytes(encoded)).toEqual(bytes);
  });

  it('encodes nothing as the empty string', () => {
    expect(bytesToBase64(new Uint8Array(0))).toBe('');
  });
});
