import { describe, expect, it } from 'vitest';
import { streamedTextPast, utf16IndexAtUtf8, utf8Length } from './utf8Offsets';

const encoder = new TextEncoder();

describe('utf8Length', () => {
  it('counts the UTF-8 bytes of every character width', () => {
    for (const text of ['', 'plain', 'café', 'naïve résumé', '日本語', 'a😀b', '😀😀', 'mixed é日😀 end']) {
      expect(utf8Length(text)).toBe(encoder.encode(text).length);
    }
  });

  it('sums a surrogate pair split across two strings to the whole character', () => {
    const pair = '😀';
    expect(utf8Length(pair.slice(0, 1)) + utf8Length(pair.slice(1))).toBe(4);
  });
});

describe('utf16IndexAtUtf8', () => {
  it('maps a byte offset on a character boundary to its code-unit index', () => {
    const text = 'aé日😀z';
    expect(utf16IndexAtUtf8(text, 0)).toBe(0);
    expect(utf16IndexAtUtf8(text, 1)).toBe(1);
    expect(utf16IndexAtUtf8(text, 3)).toBe(2);
    expect(utf16IndexAtUtf8(text, 6)).toBe(3);
    expect(utf16IndexAtUtf8(text, 10)).toBe(5);
    expect(utf16IndexAtUtf8(text, 11)).toBe(6);
  });

  it('refuses an offset inside a character or past the end', () => {
    const text = 'aé日😀';
    expect(utf16IndexAtUtf8(text, 2)).toBeNull();
    expect(utf16IndexAtUtf8(text, 4)).toBeNull();
    expect(utf16IndexAtUtf8(text, 8)).toBeNull();
    expect(utf16IndexAtUtf8(text, 11)).toBeNull();
  });
});

describe('streamedTextPast', () => {
  it('keeps a chunk that starts at the end', () => {
    expect(streamedTextPast('next ', 10, 10)).toBe('next ');
  });

  it('keeps only the part of an overlapping chunk past the end', () => {
    expect(streamedTextPast('abcdef', 10, 13)).toBe('def');
    // 'é' is two bytes: the end falls after it.
    expect(streamedTextPast('xé日z', 0, 3)).toBe('日z');
  });

  it('drops a chunk the end already covers', () => {
    expect(streamedTextPast('abc', 10, 13)).toBe('');
    expect(streamedTextPast('abc', 10, 20)).toBe('');
  });

  it('refuses a chunk that starts past the end', () => {
    expect(streamedTextPast('abc', 11, 10)).toBeNull();
  });

  it('refuses an end inside one of the chunk characters', () => {
    expect(streamedTextPast('日本', 0, 1)).toBeNull();
  });
});
