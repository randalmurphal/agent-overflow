import { describe, expect, it } from 'vitest';
import { unpackText } from './packedText';
import { packText } from '../../test/helpers/streamedPatch';

const decoder = new TextDecoder('utf-8', { ignoreBOM: true });

function packedOf(bytes: readonly number[]): string {
  return String.fromCharCode(...bytes);
}

// Text with every UTF-8 length, an astral character (a surrogate pair in
// UTF-16), U+FFFD, a BOM and line ends of both kinds.
const sample = '﻿ascii é ✓ 😀 � end\r\nnext line ü\n\nlast 𝄞 line';

describe('unpackText', () => {
  it('decodes packed UTF-8 to the text, a BOM and astral characters included', () => {
    for (const text of [sample, '', 'plain ascii', '😀', '﻿', 'x'.repeat(70_000) + 'é']) {
      expect(unpackText(packText(text))).toBe(text);
    }
  });

  it('decodes invalid sequences as TextDecoder does', () => {
    for (const bytes of [[0xff], [0xc3], [0xe2, 0x9c], [0xed, 0xa0, 0x80], [0xf0, 0x9f, 0x98], [0x80, 0x41], [0xc0, 0xaf]]) {
      expect(unpackText(packedOf(bytes))).toBe(decoder.decode(Uint8Array.from(bytes)));
    }
  });
});
