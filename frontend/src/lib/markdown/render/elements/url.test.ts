// parseUrl reads the scheme after the trim WHATWG parsing applies: leading
// and trailing C0 controls and spaces go, inner ones stay.

import { describe, expect, it } from 'vitest';
import { parseUrl } from './url';

const C0_AND_SPACE = Array.from({ length: 0x21 }, (_, code) => String.fromCharCode(code)).join('');

describe('parseUrl trimming', () => {
  it('drops every leading and trailing C0 control and space', () => {
    expect(parseUrl(`${C0_AND_SPACE}https://a.example/x${C0_AND_SPACE}`)?.href).toBe('https://a.example/x');
  });

  it('reads a protocol-relative URL behind leading controls', () => {
    expect(parseUrl('\t\n //a.example/x')?.href).toBe('https://a.example/x');
  });

  it('keeps characters above the space', () => {
    expect(parseUrl('!https://a.example/x')).toBeNull();
  });

  it('answers null for input that is only controls and spaces', () => {
    expect(parseUrl(C0_AND_SPACE)).toBeNull();
    expect(parseUrl('')).toBeNull();
  });
});
