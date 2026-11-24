import { describe, expect, it } from 'vitest';
import { detectMentionTrigger } from './mentionHelpers';

describe('detectMentionTrigger', () => {
  it.each([
    { case: 'returns null when no @ before the caret', value: 'hello world', caret: 5, trigger: null },
    { case: 'detects @ at start of string', value: '@foo', caret: 4, trigger: { query: 'foo', start: 0, end: 4 } },
    { case: 'detects @ after whitespace', value: 'look at @bar', caret: 12, trigger: { query: 'bar', start: 8, end: 12 } },
    { case: 'does not trigger when @ follows a non-whitespace character', value: 'foo@bar', caret: 7, trigger: null },
    { case: 'closes the trigger after a space', value: '@foo bar', caret: 8, trigger: null },
    { case: 'allows an empty query immediately after @', value: '@', caret: 1, trigger: { query: '', start: 0, end: 1 } },
    { case: 'rejects a caret past the end', value: '@foo', caret: 10, trigger: null },
    { case: 'rejects a negative caret', value: '@foo', caret: -1, trigger: null },
  ])('$case', ({ value, caret, trigger }) => {
    expect(detectMentionTrigger(value, caret)).toEqual(trigger);
  });
});
