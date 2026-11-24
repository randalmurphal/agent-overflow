import { describe, expect, it } from 'vitest';
import { errString } from './errors';

describe('errString', () => {
  it.each([
    ['returns .message for Error instances (without the "Error:" prefix)', new Error('boom'), 'boom'],
    ['returns .message for TypeError / subclasses', new TypeError('bad'), 'bad'],
    ['returns a string value verbatim', 'database offline', 'database offline'],
    ['returns the empty string verbatim (no fallback)', '', ''],
    ['returns `.message` from duck-typed { message: string } objects', { message: 'custom', code: 42 }, 'custom'],
    // Some providers return `{ message: null }` which shouldn't be used.
    ['falls back to String() for { message: non-string }', { message: null }, '[object Object]'],
    ['falls back to String() for plain objects without message', { status: 500 }, '[object Object]'],
    ['stringifies a number primitive', 42, '42'],
    ['stringifies a boolean primitive', false, 'false'],
    ['handles null', null, 'null'],
    ['handles undefined', undefined, 'undefined'],
    // An object with `message: ""` intentionally falls through to
    // String() so we don't produce an empty toast from a malformed error.
    ['skips empty-string .message and falls back', { message: '' }, '[object Object]'],
  ])('%s', (_name, input: unknown, want) => {
    expect(errString(input)).toBe(want);
  });
});
