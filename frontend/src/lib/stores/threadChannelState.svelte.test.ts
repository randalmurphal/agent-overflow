import { afterEach, describe, expect, it } from 'vitest';
import type { ChannelMessage, ChannelStatePayload } from '../types/discussion';
import { createThreadChannelState } from './threadChannelState.svelte';
import {
  clearAllDiscussionLiveTail,
  lookupDiscussionLiveTail,
} from './discussionLiveTail';

afterEach(() => {
  // Every test in this file that calls applyState() registers roster ids
  // in the module-level live-tail registry. Clear between tests so a
  // registration from one test can't leak into the next's lookups.
  clearAllDiscussionLiveTail();
});

function makeMessage(overrides: Partial<ChannelMessage> = {}): ChannelMessage {
  return {
    id: 'message-' + (overrides.sequence ?? 1),
    channelId: 'channel-1',
    sequence: 1,
    fromType: 'agent',
    fromId: 'agent-1',
    fromRole: 'advocate',
    content: 'hello',
    createdAt: 0,
    ...overrides,
  };
}

function makeStatePayload(overrides: Partial<ChannelStatePayload> = {}): ChannelStatePayload {
  return {
    channelId: 'channel-1',
    threadId: 'parent-thread',
    status: 'open',
    turnCount: 0,
    maxTurns: 8,
    awaitingResponse: false,
    currentSpeakerThreadId: 'advocate-thread',
    currentSpeakerRole: 'advocate',
    participants: [
      { threadId: 'advocate-thread', role: 'advocate', provider: 'claude', model: 'claude-sonnet-4-6' },
      { threadId: 'critic-thread', role: 'critic', provider: 'claude', model: 'claude-sonnet-4-6' },
    ],
    ...overrides,
  };
}

describe('createThreadChannelState', () => {
  it('merges messages by sequence while preserving timeline order', () => {
    const state = createThreadChannelState();

    state.applyMessageBatch([
      makeMessage({ id: 'message-3', sequence: 3, content: 'third' }),
      makeMessage({ id: 'message-1', sequence: 1, content: 'first' }),
    ]);
    state.applyMessageBatch([
      makeMessage({ id: 'message-2', sequence: 2, content: 'second' }),
      makeMessage({ id: 'duplicate-3', sequence: 3, content: 'duplicate third' }),
    ]);

    expect(state.messages.map((message) => message.sequence)).toEqual([1, 2, 3]);
    expect(state.messages.map((message) => message.content)).toEqual(['first', 'second', 'third']);
  });

  it('keeps the messages array reference when incoming messages are duplicates', () => {
    const state = createThreadChannelState();
    state.applyMessageBatch([makeMessage({ id: 'message-1', sequence: 1 })]);
    const before = state.messages;

    state.applyMessageBatch([makeMessage({ id: 'duplicate-1', sequence: 1, content: 'ignored' })]);

    expect(state.messages).toBe(before);
    expect(state.messages.map((message) => message.content)).toEqual(['hello']);
  });

  it('applyMessage dedupes a single push against the same sequence', () => {
    const state = createThreadChannelState();
    state.applyMessage(makeMessage({ id: 'a', sequence: 1, content: 'first' }));
    state.applyMessage(makeMessage({ id: 'a-echo', sequence: 1, content: 'echoed' }));

    expect(state.messages).toHaveLength(1);
    expect(state.messages[0].content).toBe('first');
  });

  it('applyMessage keeps sort order for out-of-order sequences', () => {
    const state = createThreadChannelState();
    state.applyMessage(makeMessage({ id: 'b', sequence: 2, content: 'second' }));
    state.applyMessage(makeMessage({ id: 'a', sequence: 1, content: 'first' }));

    expect(state.messages.map((m) => m.sequence)).toEqual([1, 2]);
  });

  it('stamps lastLiveContentAt only for genuinely-new messages', () => {
    const state = createThreadChannelState();
    expect(state.lastLiveContentAt).toBe(0);

    const before = performance.now();
    state.applyMessage(makeMessage({ id: 'a', sequence: 1 }));
    const after = performance.now();

    expect(state.lastLiveContentAt).toBeGreaterThanOrEqual(before);
    expect(state.lastLiveContentAt).toBeLessThanOrEqual(after);

    // A duplicate re-application must not re-stamp.
    const stampAfterFirst = state.lastLiveContentAt;
    state.applyMessage(makeMessage({ id: 'a-dup', sequence: 1, content: 'dup' }));
    expect(state.lastLiveContentAt).toBe(stampAfterFirst);
  });

  it('applyMessageBatch stamps lastLiveContentAt when the batch adds any new message', () => {
    const state = createThreadChannelState();
    expect(state.lastLiveContentAt).toBe(0);

    state.applyMessageBatch([makeMessage({ id: 'a', sequence: 1 })]);
    expect(state.lastLiveContentAt).toBeGreaterThan(0);
  });

  describe('live tail', () => {
    it('applyState registers the roster and a tail upsert reaches the channel state', () => {
      const state = createThreadChannelState();
      state.applyState(makeStatePayload());

      const handlers = lookupDiscussionLiveTail('advocate-thread');
      expect(handlers?.size).toBe(1);
      for (const handler of handlers ?? []) {
        handler.applyTailUpsert('advocate-thread', 'item-1', 'partial text');
      }

      expect(state.liveTail).toEqual({
        threadId: 'advocate-thread',
        itemId: 'item-1',
        text: 'partial text',
      });
    });

    it('applyTailUpsert replaces text wholesale (self-repairs a missed-delta mount)', () => {
      const state = createThreadChannelState();
      state.applyState(makeStatePayload());
      const handler = [...(lookupDiscussionLiveTail('advocate-thread') ?? [])][0];

      handler.applyTailUpsert('advocate-thread', 'item-1', 'first chunk');
      handler.applyTailUpsert('advocate-thread', 'item-1', 'first chunk plus more, all at once');

      expect(state.liveTail?.text).toBe('first chunk plus more, all at once');
    });

    it('applyTailDelta appends to the current tail for the same item', () => {
      const state = createThreadChannelState();
      state.applyState(makeStatePayload());
      const handler = [...(lookupDiscussionLiveTail('advocate-thread') ?? [])][0];

      handler.applyTailDelta('advocate-thread', 'item-1', 'Hello');
      handler.applyTailDelta('advocate-thread', 'item-1', ', world');

      expect(state.liveTail?.text).toBe('Hello, world');
    });

    it('a new assistant_text item id supersedes the previous tail instead of appending', () => {
      const state = createThreadChannelState();
      state.applyState(makeStatePayload());
      const handler = [...(lookupDiscussionLiveTail('advocate-thread') ?? [])][0];

      handler.applyTailDelta('advocate-thread', 'item-1', 'old item text');
      handler.applyTailDelta('advocate-thread', 'item-2', 'new item text');

      expect(state.liveTail).toEqual({
        threadId: 'advocate-thread',
        itemId: 'item-2',
        text: 'new item text',
      });
    });

    it('an AGENT message landing clears the live tail for that fromId', () => {
      const state = createThreadChannelState();
      state.applyState(makeStatePayload());
      const handler = [...(lookupDiscussionLiveTail('advocate-thread') ?? [])][0];
      handler.applyTailDelta('advocate-thread', 'item-1', 'streaming...');
      expect(state.liveTail).not.toBeNull();

      state.applyMessage(makeMessage({
        id: 'm1', sequence: 1, fromType: 'agent', fromId: 'advocate-thread', content: 'streaming... done',
      }));

      expect(state.liveTail).toBeNull();
    });

    it('a HUMAN message landing does not clear an unrelated live tail', () => {
      const state = createThreadChannelState();
      state.applyState(makeStatePayload());
      const handler = [...(lookupDiscussionLiveTail('advocate-thread') ?? [])][0];
      handler.applyTailDelta('advocate-thread', 'item-1', 'streaming...');

      state.applyMessage(makeMessage({ id: 'm1', sequence: 1, fromType: 'human', fromId: 'user', content: 'hi' }));

      expect(state.liveTail).not.toBeNull();
    });

    it('an agent message from a DIFFERENT fromId does not clear the tail', () => {
      const state = createThreadChannelState();
      state.applyState(makeStatePayload());
      const handler = [...(lookupDiscussionLiveTail('advocate-thread') ?? [])][0];
      handler.applyTailDelta('advocate-thread', 'item-1', 'streaming...');

      state.applyMessage(makeMessage({
        id: 'm1', sequence: 1, fromType: 'agent', fromId: 'critic-thread', content: 'unrelated',
      }));

      expect(state.liveTail).not.toBeNull();
    });

    it('applyState drops a stale tail whose thread is no longer the awaited current speaker', () => {
      // A tool-only turn advances CurrentSpeaker without ever posting an
      // agent message, so clearTailIfSuperseded never fires for the old
      // speaker — the next discussion:state snapshot must clean it up.
      const state = createThreadChannelState();
      state.applyState(makeStatePayload({ currentSpeakerThreadId: 'advocate-thread' }));
      const handler = [...(lookupDiscussionLiveTail('advocate-thread') ?? [])][0];
      handler.applyTailDelta('advocate-thread', 'item-1', 'partial, never finished');

      state.applyState(makeStatePayload({ currentSpeakerThreadId: 'critic-thread' }));

      expect(state.liveTail).toBeNull();
    });

    it('applyState keeps a live tail that still matches the current speaker', () => {
      const state = createThreadChannelState();
      state.applyState(makeStatePayload({ currentSpeakerThreadId: 'advocate-thread', awaitingResponse: true }));
      const handler = [...(lookupDiscussionLiveTail('advocate-thread') ?? [])][0];
      handler.applyTailDelta('advocate-thread', 'item-1', 'still going');

      // A second state push mid-turn (e.g. re-emitted after some
      // unrelated advance) with the same current speaker must not wipe
      // the in-flight tail.
      state.applyState(makeStatePayload({ currentSpeakerThreadId: 'advocate-thread', awaitingResponse: true, turnCount: 1 }));

      expect(state.liveTail?.text).toBe('still going');
    });

    it('applyState unregisters a participant dropped from the roster', () => {
      const state = createThreadChannelState();
      state.applyState(makeStatePayload());
      expect(lookupDiscussionLiveTail('critic-thread')).toBeDefined();

      state.applyState(makeStatePayload({
        participants: [
          { threadId: 'advocate-thread', role: 'advocate', provider: 'claude', model: 'claude-sonnet-4-6' },
        ],
      }));

      expect(lookupDiscussionLiveTail('critic-thread')).toBeUndefined();
      expect(lookupDiscussionLiveTail('advocate-thread')).toBeDefined();
    });

    it('a tail upsert/delta stamps lastLiveContentAt', () => {
      const state = createThreadChannelState();
      state.applyState(makeStatePayload());
      const handler = [...(lookupDiscussionLiveTail('advocate-thread') ?? [])][0];
      expect(state.lastLiveContentAt).toBe(0);

      handler.applyTailDelta('advocate-thread', 'item-1', 'chunk');

      expect(state.lastLiveContentAt).toBeGreaterThan(0);
    });

    it('clear() resets every field and unregisters the roster from the live-tail registry', () => {
      const state = createThreadChannelState();
      state.applyState(makeStatePayload());
      state.applyMessage(makeMessage({ id: 'a', sequence: 1 }));
      const handler = [...(lookupDiscussionLiveTail('advocate-thread') ?? [])][0];
      handler.applyTailDelta('advocate-thread', 'item-1', 'chunk');
      expect(lookupDiscussionLiveTail('advocate-thread')).toBeDefined();

      state.clear();

      expect(state.messages).toEqual([]);
      expect(state.status).toBeNull();
      expect(state.turnCount).toBe(0);
      expect(state.maxTurns).toBe(0);
      expect(state.awaitingResponse).toBe(false);
      expect(state.currentSpeakerThreadId).toBeNull();
      expect(state.currentSpeakerRole).toBeNull();
      expect(state.participants).toEqual([]);
      expect(state.liveTail).toBeNull();
      expect(state.lastLiveContentAt).toBe(0);
      expect(lookupDiscussionLiveTail('advocate-thread')).toBeUndefined();
      expect(lookupDiscussionLiveTail('critic-thread')).toBeUndefined();
    });
  });

  it('applyState applies a full deliberation-FSM snapshot', () => {
    const state = createThreadChannelState();

    state.applyState(makeStatePayload({
      status: 'open',
      turnCount: 3,
      maxTurns: 8,
      awaitingResponse: true,
      currentSpeakerThreadId: 'critic-thread',
      currentSpeakerRole: 'critic',
    }));

    expect(state.status).toBe('open');
    expect(state.turnCount).toBe(3);
    expect(state.maxTurns).toBe(8);
    expect(state.awaitingResponse).toBe(true);
    expect(state.currentSpeakerThreadId).toBe('critic-thread');
    expect(state.currentSpeakerRole).toBe('critic');
    expect(state.participants).toHaveLength(2);
  });

  it('clears messages and status for thread switches', () => {
    const state = createThreadChannelState();
    state.applyMessageBatch([makeMessage()]);
    state.applyState(makeStatePayload({ status: 'closed' }));

    state.clear();

    expect(state.messages).toEqual([]);
    expect(state.status).toBeNull();
  });
});
