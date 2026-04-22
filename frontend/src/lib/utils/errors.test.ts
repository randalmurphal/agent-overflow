import { describe, expect, it } from 'vitest';
import { errString } from './errors';

describe('errString', () => {
  it('returns .message for Error instances (without the "Error:" prefix)', () => {
    expect(errString(new Error('boom'))).toBe('boom');
  });

  it('returns .message for TypeError / subclasses', () => {
    expect(errString(new TypeError('bad'))).toBe('bad');
  });

  it('returns a string value verbatim', () => {
    expect(errString('database offline')).toBe('database offline');
  });

  it('returns the empty string verbatim (no fallback)', () => {
    expect(errString('')).toBe('');
  });

  it('returns `.message` from duck-typed { message: string } objects', () => {
    expect(errString({ message: 'custom', code: 42 })).toBe('custom');
  });

  it('falls back to String() for { message: non-string }', () => {
    // Some providers return `{ message: null }` which shouldn't be used.
    expect(errString({ message: null })).toBe('[object Object]');
  });

  it('falls back to String() for plain objects without message', () => {
    expect(errString({ status: 500 })).toBe('[object Object]');
  });

  it('stringifies number / boolean primitives', () => {
    expect(errString(42)).toBe('42');
    expect(errString(false)).toBe('false');
  });

  it('handles null and undefined', () => {
    expect(errString(null)).toBe('null');
    expect(errString(undefined)).toBe('undefined');
  });

  it('skips empty-string .message and falls back', () => {
    // An object with `message: ""` intentionally falls through to
    // String() so we don't produce an empty toast from a malformed error.
    expect(errString({ message: '' })).toBe('[object Object]');
  });
});
