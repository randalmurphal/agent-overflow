import { describe, expect, it } from 'vitest';
import { createThreadPane, type ThreadPane } from '../lib/stores/thread.svelte';
import { makeThread } from './helpers/chat';

// The shared teardown clears a pane a test leaves open, as closing it does in
// the app, so its pending work (the replica write-back) ends with the test.
describe('panes a test leaves open', () => {
  let leftOpen: ThreadPane | null = null;

  it('a test opens a pane and does not close it', () => {
    leftOpen = createThreadPane({ paneId: 'left-open' });
    leftOpen.replaceThread(makeThread({ id: 'thread-left-open' }));
    expect(leftOpen.thread?.id).toBe('thread-left-open');
  });

  it('is cleared before the next test runs', () => {
    expect(leftOpen).not.toBeNull();
    expect(leftOpen!.thread).toBeNull();
  });
});
