import { describe, expect, it } from 'vitest';
import { sha1Hex, sha256Hex } from './hashes';

// FIPS 180 vectors (the empty string, "abc", the 448-bit two-block
// message, a million "a"s), and paths checked with `printf '%s' <path> |
// sha256sum` and `sha1sum`, a non-ASCII one hashed as UTF-8.
const VECTORS = [
  { text: '', sha256: 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855', sha1: 'da39a3ee5e6b4b0d3255bfef95601890afd80709' },
  { text: 'abc', sha256: 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad', sha1: 'a9993e364706816aba3e25717850c26c9cd0d89d' },
  { text: 'abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq', sha256: '248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1', sha1: '84983e441c3bd26ebaae4aa1f95129e5e54670f1' },
  { text: 'a'.repeat(1_000_000), sha256: 'cdc76e5c9914fb9281a1c7e284d73e67f1809a48a497200e046d39ccc7112cd0', sha1: '34aa973cd4c4daa4f61eeb2bdbad27316534016f' },
  { text: 'README.md', sha256: 'b335630551682c19a781afebcf4d07bf978fb1f8ac04c6bf87428ed5106870f5', sha1: '8ec9a00bfd09b3190ac6b22251dbb1aa95a0579d' },
  { text: 'a b/ünï.txt', sha256: 'f1db545a5d36e6ba1355f94da081f0ef6b1b04621d85e00c54f2e2819f7dbc17', sha1: '7ca6a06256cb97f9d2b07b69c4c505514a1a1518' },
];

describe('sha1Hex and sha256Hex', () => {
  it.each(VECTORS)('hash $text.length bytes of text to the standard digests', ({ text, sha256, sha1 }) => {
    expect(sha256Hex(text)).toBe(sha256);
    expect(sha1Hex(text)).toBe(sha1);
  });
});
