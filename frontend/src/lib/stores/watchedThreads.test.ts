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
  registerWatchedScopeSource,
  registerWatchedThreadSource,
  resetWatchedThreadSourcesForTest,
  watchThreadsBeforeMount,
} from './watchedThreads';

/** Every set pushed to the transport, in order. */
let pushed: string[][];
/** The scope half of every push, as `thread/root`. */
let pushedScopes: string[][];

/** The ids of the most recent push, sorted so assertions read by set. */
function lastSent(): string[] {
  return [...(pushed.at(-1) ?? [])].sort();
}

function lastScopes(): string[] {
  return [...(pushedScopes.at(-1) ?? [])].sort();
}

describe('watched-thread composition', () => {
  beforeEach(() => {
    resetWatchedThreadSourcesForTest();
    pushed = [];
    pushedScopes = [];
    vi.spyOn(wsClient, 'setWatchedThreads').mockImplementation((ids, scopes) => {
      pushed.push([...ids]);
      pushedScopes.push(scopes.map((scope) => `${scope.threadId}/${scope.scopeRootId}`));
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

  it('sends the union of scope sources beside the threads, and drops one on release', () => {
    registerWatchedThreadSource(() => ['pane-thread']);
    const agentView = registerWatchedScopeSource(() => [{ threadId: 'pane-thread', scopeRootId: 'agent-1' }]);
    registerWatchedScopeSource(() => [{ threadId: 'pane-thread', scopeRootId: 'agent-2' }]);

    expect(lastSent()).toEqual(['pane-thread']);
    expect(lastScopes()).toEqual(['pane-thread/agent-1', 'pane-thread/agent-2']);

    agentView();
    expect(lastScopes()).toEqual(['pane-thread/agent-2']);
    expect(lastSent()).toEqual(['pane-thread']);
  });

  it('ignores a scope with an empty id', () => {
    registerWatchedScopeSource(() => [
      { threadId: '', scopeRootId: 'agent-1' },
      { threadId: 'pane-thread', scopeRootId: '' },
      { threadId: 'pane-thread', scopeRootId: 'agent-2' },
    ]);
    expect(lastScopes()).toEqual(['pane-thread/agent-2']);
  });

  it('keeps the composed scopes when threads are about to mount', () => {
    // Opening another pane must not drop an open agent view's scope for
    // the moment before the mount recomposes.
    registerWatchedScopeSource(() => [{ threadId: 'already-open', scopeRootId: 'agent-1' }]);

    watchThreadsBeforeMount(['opening']);
    expect(lastSent()).toEqual(['opening']);
    expect(lastScopes()).toEqual(['already-open/agent-1']);
  });
});
