import { describe, expect, it } from 'vitest';
import { decodeTerminalOutput, encodeTerminalInput } from './terminal';

describe('terminal encoding helpers', () => {
  it('decodes base64 output preserving every byte', () => {
    const bytes = new Uint8Array([0x48, 0x69, 0x1b, 0x5b, 0x30, 0x6d]);
    // Hi + ANSI reset
    const b64 = btoa(String.fromCharCode(...bytes));
    const decoded = decodeTerminalOutput(b64);
    expect(decoded).toBeInstanceOf(Uint8Array);
    expect(Array.from(decoded)).toEqual(Array.from(bytes));
  });

  it('returns the multi-byte UTF-8 bytes intact, not a Latin-1 string', () => {
    // Regression for the TUI-renders-as-mojibake bug: `─` (U+2500) is the
    // 3-byte UTF-8 sequence 0xE2 0x94 0x80. The decoder must hand xterm those
    // exact bytes so xterm's own UTF-8 decoder composes the glyph. Decoding to
    // a string would split it into three Latin-1 code points (`â`, `”`, ``).
    const boxDrawing = new Uint8Array([0xe2, 0x94, 0x80]);
    const b64 = btoa(String.fromCharCode(...boxDrawing));
    const decoded = decodeTerminalOutput(b64);
    expect(Array.from(decoded)).toEqual([0xe2, 0x94, 0x80]);
  });

  it('returns an empty byte array for empty input', () => {
    expect(decodeTerminalOutput('')).toEqual(new Uint8Array(0));
  });

  it('encodes string input as base64 UTF-8', () => {
    const encoded = encodeTerminalInput('hi');
    expect(atob(encoded)).toBe('hi');
  });

  it('round-trips multi-byte UTF-8 characters losslessly', () => {
    const emoji = '😀';
    const encoded = encodeTerminalInput(emoji);
    // Decode bytes and assert identical content.
    const binary = atob(encoded);
    const bytes = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i++) {
      bytes[i] = binary.charCodeAt(i);
    }
    const roundTripped = new TextDecoder().decode(bytes);
    expect(roundTripped).toBe(emoji);
  });
});
