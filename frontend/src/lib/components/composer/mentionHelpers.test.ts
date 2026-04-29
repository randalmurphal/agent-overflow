import { describe, expect, it } from 'vitest';
import { detectMentionTrigger } from './mentionHelpers';

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
