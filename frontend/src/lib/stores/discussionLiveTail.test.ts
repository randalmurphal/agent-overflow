import { describe, expect, it, vi } from 'vitest';
import {
  clearAllDiscussionLiveTail,
  lookupDiscussionLiveTail,
  registerDiscussionLiveTail,
  unregisterDiscussionLiveTail,
  type DiscussionLiveTailHandler,
} from './discussionLiveTail';

function makeHandler(): DiscussionLiveTailHandler {
  return {
    applyTailUpsert: vi.fn(),
    applyTailDelta: vi.fn(),
  };
}

describe('discussionLiveTail registry', () => {
  it('returns undefined for a threadId with no registrations', () => {
    expect(lookupDiscussionLiveTail('nobody-registered')).toBeUndefined();
  });

  it('registers a handler and makes it reachable via lookup', () => {
    const handler = makeHandler();
    registerDiscussionLiveTail('child-1', handler);

    const handlers = lookupDiscussionLiveTail('child-1');
    expect(handlers).toBeDefined();
    expect(handlers?.has(handler)).toBe(true);

    clearAllDiscussionLiveTail();
  });

  it('supports multiple handlers registered under the same threadId', () => {
    // Two panes could show the same parent discussion thread — each
    // pane's channel-state instance registers its own handler under the
    // same participant thread id.
    const handlerA = makeHandler();
    const handlerB = makeHandler();
    registerDiscussionLiveTail('child-1', handlerA);
    registerDiscussionLiveTail('child-1', handlerB);

    const handlers = lookupDiscussionLiveTail('child-1');
    expect(handlers?.size).toBe(2);
    expect(handlers?.has(handlerA)).toBe(true);
    expect(handlers?.has(handlerB)).toBe(true);

    clearAllDiscussionLiveTail();
  });

  it('supports one handler registered under multiple threadIds (a full roster)', () => {
    const handler = makeHandler();
    registerDiscussionLiveTail('advocate-thread', handler);
    registerDiscussionLiveTail('critic-thread', handler);

    expect(lookupDiscussionLiveTail('advocate-thread')?.has(handler)).toBe(true);
    expect(lookupDiscussionLiveTail('critic-thread')?.has(handler)).toBe(true);

    clearAllDiscussionLiveTail();
  });

  it('unregister removes only the targeted handler and drops the empty entry', () => {
    const handlerA = makeHandler();
    const handlerB = makeHandler();
    registerDiscussionLiveTail('child-1', handlerA);
    registerDiscussionLiveTail('child-1', handlerB);

    unregisterDiscussionLiveTail('child-1', handlerA);
    const handlers = lookupDiscussionLiveTail('child-1');
    expect(handlers?.has(handlerA)).toBe(false);
    expect(handlers?.has(handlerB)).toBe(true);

    unregisterDiscussionLiveTail('child-1', handlerB);
    // The Set emptied out — the registry drops the key entirely rather
    // than leaking an empty Set entry per stale thread id.
    expect(lookupDiscussionLiveTail('child-1')).toBeUndefined();
  });

  it('unregister is a no-op for an unknown threadId or an unregistered handler', () => {
    const handler = makeHandler();
    expect(() => unregisterDiscussionLiveTail('never-registered', handler)).not.toThrow();

    registerDiscussionLiveTail('child-1', handler);
    const other = makeHandler();
    unregisterDiscussionLiveTail('child-1', other);
    expect(lookupDiscussionLiveTail('child-1')?.has(handler)).toBe(true);

    clearAllDiscussionLiveTail();
  });

  it('clearAll drops every registration', () => {
    const handler = makeHandler();
    registerDiscussionLiveTail('child-1', handler);
    registerDiscussionLiveTail('child-2', handler);

    clearAllDiscussionLiveTail();

    expect(lookupDiscussionLiveTail('child-1')).toBeUndefined();
    expect(lookupDiscussionLiveTail('child-2')).toBeUndefined();
  });

  it('registering with an empty threadId is a no-op', () => {
    const handler = makeHandler();
    registerDiscussionLiveTail('', handler);
    expect(lookupDiscussionLiveTail('')).toBeUndefined();
  });
});
