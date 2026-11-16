import { describe, expect, it } from 'vitest';
import { mergeQueueNoun, mergeQueuePlace } from './reviewReadiness';

describe('merge queue wording', () => {
  it("names GitLab's queue a merge train", () => {
    expect(mergeQueueNoun('gitlab')).toBe('merge train');
    expect(mergeQueueNoun('github')).toBe('merge queue');
    expect(mergeQueueNoun(undefined)).toBe('merge queue');
  });

  it('says the place it knows', () => {
    expect(mergeQueuePlace(2, 3)).toBe('2 of 3');
    expect(mergeQueuePlace(2, undefined)).toBe('position 2');
    expect(mergeQueuePlace(undefined, 3)).toBe('');
  });
});
