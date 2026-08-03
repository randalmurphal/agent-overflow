import { beforeEach, describe, expect, it } from 'vitest';
import { applyCommandLifecycle } from './eventsQueue';
import {
  getFlushedForThread,
  markItemsFlushed,
  resetForTest,
} from './sendQueue.svelte';

function seedFlushed(): void {
  markItemsFlushed('t1', [
    { queueItemId: 'q1', userItemId: 'user:0:flush:1', message: 'steer me' },
  ]);
}

describe('applyCommandLifecycle', () => {
  beforeEach(() => {
    resetForTest();
  });

  // The baseline every consumer must render correctly: an older Claude
  // CLI (or a Codex thread) emits no lifecycle frames at all.
  it('leaves the entry untouched when no ack ever arrives', () => {
    seedFlushed();
    expect(getFlushedForThread('t1')[0].lifecycle).toBeUndefined();
  });

  it('stamps the queued ack onto its entry', () => {
    seedFlushed();
    applyCommandLifecycle({
      threadId: 't1',
      commandUuid: 'uuid-1',
      userItemId: 'user:0:flush:1',
      state: 'queued',
    });
    expect(getFlushedForThread('t1')[0].lifecycle).toEqual({
      state: 'queued',
      delivery: undefined,
    });
  });

  it('carries the delivery classification on started', () => {
    seedFlushed();
    applyCommandLifecycle({
      threadId: 't1',
      commandUuid: 'uuid-1',
      userItemId: 'user:0:flush:1',
      state: 'started',
      delivery: 'mid_turn',
    });
    expect(getFlushedForThread('t1')[0].lifecycle).toEqual({
      state: 'started',
      delivery: 'mid_turn',
    });
  });

  it('advances through the full queued → started → completed sequence', () => {
    seedFlushed();
    const base = { threadId: 't1', commandUuid: 'uuid-1', userItemId: 'user:0:flush:1' } as const;
    applyCommandLifecycle({ ...base, state: 'queued' });
    applyCommandLifecycle({ ...base, state: 'started', delivery: 'new_turn' });
    applyCommandLifecycle({ ...base, state: 'completed' });
    expect(getFlushedForThread('t1')[0].lifecycle?.state).toBe('completed');
    // The delivery classification does not stick around past the state it
    // described — a completed message is no longer "starting".
    expect(getFlushedForThread('t1')[0].lifecycle?.delivery).toBeUndefined();
  });

  it('records a cancelled message', () => {
    seedFlushed();
    applyCommandLifecycle({
      threadId: 't1',
      commandUuid: 'uuid-1',
      userItemId: 'user:0:flush:1',
      state: 'cancelled',
    });
    expect(getFlushedForThread('t1')[0].lifecycle?.state).toBe('cancelled');
  });

  // An ack whose row the backend could not correlate has nothing to
  // attach to. Attaching it anywhere would label the wrong message.
  it('ignores an ack with no resolved row', () => {
    seedFlushed();
    applyCommandLifecycle({ threadId: 't1', commandUuid: 'uuid-1', state: 'started' });
    expect(getFlushedForThread('t1')[0].lifecycle).toBeUndefined();
  });

  // The echo can clear Zone 2 before a late ack lands; re-adding state
  // would resurrect a marker the user already watched disappear.
  it('does not resurrect an entry that is no longer pending', () => {
    applyCommandLifecycle({
      threadId: 't1',
      commandUuid: 'uuid-1',
      userItemId: 'user:0:flush:1',
      state: 'completed',
    });
    expect(getFlushedForThread('t1')).toHaveLength(0);
  });

  it('ignores malformed frames', () => {
    seedFlushed();
    applyCommandLifecycle(undefined);
    expect(getFlushedForThread('t1')[0].lifecycle).toBeUndefined();
  });
});
