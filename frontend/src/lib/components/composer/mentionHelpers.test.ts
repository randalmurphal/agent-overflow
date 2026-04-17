import { describe, expect, it } from 'vitest';
import { applyMention, detectMentionTrigger } from './mentionHelpers';

describe('detectMentionTrigger', () => {
  it('returns null when no @ before the caret', () => {
    expect(detectMentionTrigger('hello world', 5)).toBeNull();
  });

  it('detects @ at start of string', () => {
    expect(detectMentionTrigger('@foo', 4)).toEqual({ query: 'foo', start: 0, end: 4 });
  });

  it('detects @ after whitespace', () => {
    const value = 'look at @bar';
    const trigger = detectMentionTrigger(value, value.length);
    expect(trigger).toEqual({ query: 'bar', start: 8, end: 12 });
  });

  it('does not trigger when @ follows a non-whitespace character', () => {
    expect(detectMentionTrigger('foo@bar', 7)).toBeNull();
  });

  it('closes the trigger after a space', () => {
    expect(detectMentionTrigger('@foo bar', 8)).toBeNull();
  });

  it('allows an empty query immediately after @', () => {
    expect(detectMentionTrigger('@', 1)).toEqual({ query: '', start: 0, end: 1 });
  });

  it('rejects invalid caret positions', () => {
    expect(detectMentionTrigger('@foo', 10)).toBeNull();
    expect(detectMentionTrigger('@foo', -1)).toBeNull();
  });
});

describe('applyMention', () => {
  it('replaces the trigger span with the file path and trailing space', () => {
    const result = applyMention('hello @foo', { query: 'foo', start: 6, end: 10 }, 'src/foo.ts');
    expect(result.value).toBe('hello @src/foo.ts ');
    expect(result.caret).toBe(result.value.length);
  });

  it('keeps content after the caret intact', () => {
    const value = '@fo and more';
    const result = applyMention(value, { query: 'fo', start: 0, end: 3 }, 'foo.ts');
    expect(result.value).toBe('@foo.ts  and more');
    // caret sits right after the inserted token
    expect(result.caret).toBe('@foo.ts '.length);
  });
});
