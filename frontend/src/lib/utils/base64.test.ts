import { describe, expect, it } from 'vitest';
import { base64ToBytes } from './base64';

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
