import { describe, expect, it } from 'vitest';
import { decodeTerminalOutput, encodeTerminalInput } from './terminal';

describe('terminal encoding helpers', () => {
  it('decodes base64 output preserving every byte', () => {
    const bytes = new Uint8Array([0x48, 0x69, 0x1b, 0x5b, 0x30, 0x6d]);
    // Hi + ANSI reset
    const b64 = btoa(String.fromCharCode(...bytes));
    const decoded = decodeTerminalOutput(b64);
    expect(decoded.length).toBe(bytes.length);
    for (let i = 0; i < bytes.length; i++) {
      expect(decoded.charCodeAt(i)).toBe(bytes[i]);
    }
  });

  it('returns an empty string for empty input', () => {
    expect(decodeTerminalOutput('')).toBe('');
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
