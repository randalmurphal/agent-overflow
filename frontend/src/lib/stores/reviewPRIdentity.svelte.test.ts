// The one owner of "the PR under a review pane changed": the key follows
// the PR synchronously, a moment without a PR is not a change, the first
// PR adopts what was there before it, and per-PR values stay with their PR
// and are not kept once back to their initial value.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { flushSync } from 'svelte';
import { createReviewPRIdentity, type ReviewPRIdentity } from './reviewPRIdentity.svelte';

let pr = $state<string | null>(null);
let identity: ReviewPRIdentity;
let changes: [string | null, string][];

beforeEach(() => {
  pr = 'A';
  changes = [];
  identity = createReviewPRIdentity(() => pr);
  identity.onChange((from, to) => { changes.push([from, to]); });
});

afterEach(() => identity.dispose());

describe('the PR identity', () => {
  it('follows the PR at once, holds through a moment without one, and changes only for another', () => {
    expect(identity.key).toBe('A');
    pr = null;
    expect(identity.live).toBeNull();
    expect(identity.key).toBe('A');
    flushSync();
    pr = 'A';
    flushSync();
    expect(changes).toEqual([]);
    expect(identity.epoch).toBe(0);
    pr = 'B';
    // Read before any effect has run: already the new PR.
    expect(identity.key).toBe('B');
    flushSync();
    expect(changes).toEqual([['A', 'B']]);
    expect(identity.epoch).toBe(1);
    pr = null;
    flushSync();
    pr = 'A';
    flushSync();
    expect(changes).toEqual([['A', 'B'], ['B', 'A']]);
    expect(identity.epoch).toBe(2);
  });

  it('names the first PR of a pane that had none, without counting it as a change', () => {
    identity.dispose();
    pr = null;
    changes = [];
    identity = createReviewPRIdentity(() => pr);
    identity.onChange((from, to) => { changes.push([from, to]); });
    expect(identity.key).toBeNull();
    pr = 'A';
    flushSync();
    expect(changes).toEqual([[null, 'A']]);
    expect(identity.epoch).toBe(0);
  });

  it('stops telling a removed listener', () => {
    const heard: string[] = [];
    const stop = identity.onChange((_from, to) => { heard.push(to); });
    pr = 'B';
    flushSync();
    stop();
    pr = 'C';
    flushSync();
    expect(heard).toEqual(['B']);
  });
});

describe('per-PR values', () => {
  it('keep each PR\'s own and give it back on return', () => {
    const notes = identity.keyed({ text: '', open: false });
    notes.set(identity.key, { text: 'for A', open: true });
    pr = 'B';
    expect(notes.current).toEqual({ text: '', open: false });
    notes.set(identity.key, { text: 'for B' });
    pr = 'A';
    expect(notes.current).toEqual({ text: 'for A', open: true });
    expect(notes.of('B')).toEqual({ text: 'for B', open: false });
  });

  it('carry what was there before the first PR to it', () => {
    identity.dispose();
    pr = null;
    identity = createReviewPRIdentity(() => pr);
    const notes = identity.keyed({ text: '' });
    notes.set(identity.key, { text: 'before' });
    pr = 'A';
    flushSync();
    expect(notes.current.text).toBe('before');
    expect(notes.of(null).text).toBe('');
  });

  it('keep nothing for a PR back at its initial value', () => {
    const initial = { text: '' };
    const notes = identity.keyed(initial);
    notes.set('A', { text: 'x' });
    expect(notes.of('A')).not.toBe(notes.of('Z'));
    notes.set('A', { text: '' });
    expect(notes.of('A')).toBe(notes.of('Z'));
  });
});
