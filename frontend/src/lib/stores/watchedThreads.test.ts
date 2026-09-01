// The composition half: which threads end up in the set the transport
// sends. The wire behavior itself is lib/transport/watchedThreads.test.ts.
//
// Spies on the real transport singleton rather than mocking the module:
// src/test/setup.ts already holds a live reference to it, so a module mock
// would leave this suite asserting against a different instance than the
// one the code under test calls.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { wsClient } from '../transport/wsClient';
import {
  refreshWatchedThreads,
  registerWatchedThreadSource,
  resetWatchedThreadSourcesForTest,
  watchThreadsBeforeMount,
} from './watchedThreads';

/** Every set pushed to the transport, in order. */
let pushed: string[][];

/** The ids of the most recent push, sorted so assertions read by set. */
function lastSent(): string[] {
  return [...(pushed.at(-1) ?? [])].sort();
}

describe('watched-thread composition', () => {
  beforeEach(() => {
    resetWatchedThreadSourcesForTest();
    pushed = [];
    vi.spyOn(wsClient, 'setWatchedThreads').mockImplementation((ids) => {
      pushed.push([...ids]);
    });
  });

  afterEach(() => {
    resetWatchedThreadSourcesForTest();
    vi.restoreAllMocks();
  });

  it('unions every registered source', () => {
    registerWatchedThreadSource(() => ['pane-thread']);
    registerWatchedThreadSource(() => ['live-tail-thread']);

    refreshWatchedThreads();
    expect(lastSent()).toEqual(['live-tail-thread', 'pane-thread']);
  });

  it('drops a source when it unregisters', () => {
    const release = registerWatchedThreadSource(() => ['pane-thread']);
    registerWatchedThreadSource(() => ['live-tail-thread']);

    release();
    expect(lastSent()).toEqual(['live-tail-thread']);
  });

  it('sends the composed set plus the ids about to mount', () => {
    registerWatchedThreadSource(() => ['already-open']);

    watchThreadsBeforeMount(['opening-a', 'opening-b']);
    // The opening threads are watched BEFORE the registry can see them,
    // which is the whole reason this entry point exists: their history and
    // window loads go out on the same socket immediately afterwards.
    expect(lastSent()).toEqual(['already-open', 'opening-a', 'opening-b']);
  });

  it('sends an empty set when nothing is open', () => {
    registerWatchedThreadSource(() => []);

    refreshWatchedThreads();
    // Not "skip the push": a client with every pane closed says so, and
    // the transport is what decides whether that differs from the last set.
    expect(lastSent()).toEqual([]);
    expect(pushed).not.toHaveLength(0);
  });

  it('ignores empty ids from a source mid-mount', () => {
    // A registered pane with no thread yet is the normal state of an empty
    // pane; it must contribute nothing rather than an empty id the backend
    // would refuse the whole frame for.
    registerWatchedThreadSource(() => ['', 'real-thread']);

    refreshWatchedThreads();
    expect(lastSent()).toEqual(['real-thread']);
  });
});
