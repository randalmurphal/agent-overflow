import { beforeEach, describe, expect, it } from 'vitest';
import { flushSync } from 'svelte';
import {
  applyDiscussionDefinitionsChanged,
  discussionDefinitionsRevision,
  resetDiscussionDefinitionsForTest,
} from './discussionDefinitions.svelte';

// Create, rename, edit and delete all persisted and answered their own caller,
// so a definition written on one device never appeared on another until that
// screen was reopened. The counter is the whole signal: a rename moves a
// definition between names, so a row-carrying frame could not say what any
// reader's list now holds.

beforeEach(() => {
  resetDiscussionDefinitionsForTest();
});

describe('discussionDefinitions', () => {
  it('starts settled', () => {
    expect(discussionDefinitionsRevision()).toBe(0);
  });

  it('moves once per write, so a reader re-runs once per write', () => {
    applyDiscussionDefinitionsChanged();
    expect(discussionDefinitionsRevision()).toBe(1);
    applyDiscussionDefinitionsChanged();
    expect(discussionDefinitionsRevision()).toBe(2);
  });

  it('wakes a reader that is watching it', () => {
    let seen = -1;
    let runs = 0;
    const stop = $effect.root(() => {
      $effect(() => {
        seen = discussionDefinitionsRevision();
        runs += 1;
      });
    });
    flushSync();
    expect(runs).toBe(1);
    expect(seen).toBe(0);

    applyDiscussionDefinitionsChanged();
    flushSync();

    expect(runs).toBe(2);
    expect(seen).toBe(1);
    stop();
  });
});
