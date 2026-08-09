import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setupEventListeners } from './events';
import { resetPanesForTest } from './panes.svelte';
import { setBindingMock, resetBindingMocks } from '../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';
import { transportGapChannel } from '../transport/wsClient';
import { buildPane, makeThread } from '../../test/helpers/chat';
import { makeChannelMessage, makeChannelStatePayload } from '../../test/helpers/discussion';
import { refreshDiscussionChannel } from './eventsDiscussion';
import { clearAllDiscussionLiveTail } from './discussionLiveTail';
import type { ChannelMessage, ChannelParticipantState, ChannelStatePayload } from '../types/discussion';
import type { Thread } from '../types/models';

// Exercises the push-driven discussion wiring added alongside the
// ChannelView rewrite: discussion:message / discussion:state routing
// (eventsDiscussion.ts), the provider:item_event live-tail side-channel
// for discussion child threads (eventsItemStream.ts's seam into
// discussionLiveTail.ts), the shared resync helper's fetch shape, and
// the new transport-gap cases. See docs/architecture/discussion-
// deliberation.md for the wire contract this drives against.

function discussionThread(overrides: Partial<Thread> = {}): Thread {
  return makeThread({
    id: 'parent-thread',
    title: 'Deliberation',
    mode: 'discussion',
    discussionId: 'channel-1',
    ...overrides,
  });
}

// This suite's historical defaults: short 'm<seq>' ids, an in-flight
// turn (awaitingResponse true), and a single-participant roster.
function makeMessage(overrides: Partial<ChannelMessage> = {}): ChannelMessage {
  return makeChannelMessage({ id: 'm' + (overrides.sequence ?? 1), ...overrides });
}

function makeStatePayload(overrides: Partial<ChannelStatePayload> = {}): ChannelStatePayload {
  return makeChannelStatePayload({
    awaitingResponse: true,
    participants: [
      { threadId: 'advocate-thread', role: 'advocate', provider: 'claude', model: 'claude-sonnet-4-6', proposedConclusion: false },
    ],
    ...overrides,
  });
}

let cleanup: () => void;

beforeEach(() => {
  resetBindingMocks();
  resetPanesForTest();
  clearAllDiscussionLiveTail();
  setBindingMock('ListThreads', async () => []);
  setBindingMock('ListProjects', async () => []);
  cleanup = setupEventListeners();
});

afterEach(() => {
  cleanup();
  resetPanesForTest();
  clearAllDiscussionLiveTail();
});

describe('discussion:message / discussion:state routing', () => {
  it('routes discussion:message only to the pane showing the event PARENT threadId', async () => {
    const paneA = await buildPane(discussionThread({ id: 'parent-a', discussionId: 'channel-a' }), [], 'a');
    const paneB = await buildPane(discussionThread({ id: 'parent-b', discussionId: 'channel-b' }), [], 'b');

    emitWailsEvent('discussion:message', {
      channelId: 'channel-a',
      threadId: 'parent-a',
      message: makeMessage({ channelId: 'channel-a' }),
    });

    expect(paneA.channelMessages).toHaveLength(1);
    expect(paneB.channelMessages).toHaveLength(0);
  });

  it('routes discussion:state only to the matching pane and populates the FSM snapshot', async () => {
    const paneA = await buildPane(discussionThread({ id: 'parent-a', discussionId: 'channel-a' }), [], 'a');
    const paneB = await buildPane(discussionThread({ id: 'parent-b', discussionId: 'channel-b' }), [], 'b');

    emitWailsEvent('discussion:state', makeStatePayload({ channelId: 'channel-a', threadId: 'parent-a' }));

    expect(paneA.channelStatus).toBe('open');
    expect(paneA.channelTurnCount).toBe(0);
    expect(paneA.channelMaxTurns).toBe(8);
    expect(paneA.channelAwaitingResponse).toBe(true);
    expect(paneA.channelCurrentSpeakerRole).toBe('advocate');
    expect(paneB.channelStatus).toBeNull();
  });

  it('ignores a discussion:message payload missing required fields', async () => {
    const pane = await buildPane(discussionThread(), [], 'a');
    emitWailsEvent('discussion:message', { channelId: 'channel-1' });
    expect(pane.channelMessages).toHaveLength(0);
  });

  it('drops a discussion:message whose fields fail boundary validation', async () => {
    // Same shared transport as the item stream — remote peers can reach
    // this handler, so malformed payloads must be rejected before they
    // touch reactive state (content flows into ChatMarkdown).
    const pane = await buildPane(discussionThread(), [], 'a');
    const base = { channelId: 'channel-1', threadId: 'parent-thread' };

    // Non-finite sequence.
    emitWailsEvent('discussion:message', {
      ...base,
      message: makeMessage({ sequence: Number.NaN }),
    });
    // Non-string content.
    emitWailsEvent('discussion:message', {
      ...base,
      message: { ...makeMessage({ sequence: 2 }), content: 12345 },
    });
    // Oversized content (above the shared 2M text cap).
    emitWailsEvent('discussion:message', {
      ...base,
      message: makeMessage({ sequence: 3, content: 'x'.repeat(2_000_001) }),
    });
    // Oversized fromRole (label fields cap at 128).
    emitWailsEvent('discussion:message', {
      ...base,
      message: makeMessage({ sequence: 4, fromRole: 'r'.repeat(129) }),
    });
    // Empty id.
    emitWailsEvent('discussion:message', {
      ...base,
      message: makeMessage({ sequence: 5, id: '  ' }),
    });

    expect(pane.channelMessages).toHaveLength(0);

    // A valid message still routes after the rejects.
    emitWailsEvent('discussion:message', {
      ...base,
      message: makeMessage({ sequence: 6, content: 'valid after rejects' }),
    });
    expect(pane.channelMessages).toHaveLength(1);
    expect(pane.channelMessages[0].content).toBe('valid after rejects');
  });

  it('drops a discussion:state whose fields fail boundary validation', async () => {
    const pane = await buildPane(discussionThread(), [], 'a');

    // Non-finite turn counters.
    emitWailsEvent('discussion:state', makeStatePayload({ turnCount: Number.POSITIVE_INFINITY }));
    emitWailsEvent('discussion:state', makeStatePayload({ maxTurns: Number.NaN }));
    // Non-boolean awaitingResponse.
    emitWailsEvent('discussion:state', {
      ...makeStatePayload(),
      awaitingResponse: 'yes',
    });
    // participants not an array.
    emitWailsEvent('discussion:state', {
      ...makeStatePayload(),
      participants: 'nope',
    });
    // Participant entry with an oversized role label.
    emitWailsEvent('discussion:state', makeStatePayload({
      participants: [
        { threadId: 'advocate-thread', role: 'r'.repeat(129), provider: 'claude', model: 'claude-sonnet-4-6', proposedConclusion: false },
      ],
    }));
    // Non-string currentSpeakerThreadId.
    emitWailsEvent('discussion:state', {
      ...makeStatePayload(),
      currentSpeakerThreadId: 42,
    });

    expect(pane.channelStatus).toBeNull();
    expect(pane.channelTurnCount).toBe(0);

    // A valid snapshot still routes after the rejects.
    emitWailsEvent('discussion:state', makeStatePayload({ turnCount: 2 }));
    expect(pane.channelStatus).toBe('open');
    expect(pane.channelTurnCount).toBe(2);
  });

  it('accepts a participant proposedConclusion field that is true, false, or absent (stale backend), but rejects a wrong-typed value', async () => {
    const pane = await buildPane(discussionThread(), [], 'a');

    // Present and true.
    emitWailsEvent('discussion:state', makeStatePayload({
      turnCount: 1,
      participants: [
        { threadId: 'advocate-thread', role: 'advocate', provider: 'claude', model: 'claude-sonnet-4-6', proposedConclusion: true },
      ],
    }));
    expect(pane.channelTurnCount).toBe(1);

    // Present and false.
    emitWailsEvent('discussion:state', makeStatePayload({
      turnCount: 2,
      participants: [
        { threadId: 'advocate-thread', role: 'advocate', provider: 'claude', model: 'claude-sonnet-4-6', proposedConclusion: false },
      ],
    }));
    expect(pane.channelTurnCount).toBe(2);

    // Absent entirely — a stale backend predating this field must still validate.
    const withoutField = { threadId: 'advocate-thread', role: 'advocate', provider: 'claude', model: 'claude-sonnet-4-6' };
    emitWailsEvent('discussion:state', makeStatePayload({
      turnCount: 3,
      participants: [withoutField as unknown as ChannelParticipantState],
    }));
    expect(pane.channelTurnCount).toBe(3);

    // Wrong-typed value is rejected, same as any other boundary-validated field.
    emitWailsEvent('discussion:state', makeStatePayload({
      turnCount: 4,
      participants: [
        { threadId: 'advocate-thread', role: 'advocate', provider: 'claude', model: 'claude-sonnet-4-6', proposedConclusion: 'yes' as unknown as boolean },
      ],
    }));
    expect(pane.channelTurnCount).toBe(3);
  });
});

describe('provider:item_event live-tail seam for discussion child threads', () => {
  it('feeds a registered child thread assistant_text upsert into the channel live tail', async () => {
    const pane = await buildPane(discussionThread(), [], 'a');
    // Registers 'advocate-thread' in the discussionLiveTail registry via
    // the roster in the state snapshot.
    emitWailsEvent('discussion:state', makeStatePayload());

    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'advocate-thread',
      item: {
        id: 'item-1',
        threadId: 'advocate-thread',
        turnIndex: 0,
        itemIndex: 0,
        kind: 'assistant_text',
        role: 'assistant',
        status: 'streaming',
        summary: 'partial reply',
        createdAt: 0,
        updatedAt: 0,
      },
    });
    await new Promise((resolve) => requestAnimationFrame(() => resolve(undefined)));

    expect(pane.channelLiveTail).toEqual({
      threadId: 'advocate-thread',
      itemId: 'item-1',
      text: 'partial reply',
    });
  });

  it('feeds a registered child thread assistant_text delta into the channel live tail', async () => {
    const pane = await buildPane(discussionThread(), [], 'a');
    emitWailsEvent('discussion:state', makeStatePayload());

    emitWailsEvent('provider:item_event', {
      action: 'delta',
      threadId: 'advocate-thread',
      itemId: 'item-1',
      kind: 'assistant_text',
      delta: 'Hello',
      updatedAt: 1,
    });
    emitWailsEvent('provider:item_event', {
      action: 'delta',
      threadId: 'advocate-thread',
      itemId: 'item-1',
      kind: 'assistant_text',
      delta: ', world',
      updatedAt: 2,
    });
    await new Promise((resolve) => requestAnimationFrame(() => resolve(undefined)));

    expect(pane.channelLiveTail?.text).toBe('Hello, world');
  });

  it('drops traffic for a threadId with no registered live-tail handler', async () => {
    const pane = await buildPane(discussionThread(), [], 'a');
    emitWailsEvent('discussion:state', makeStatePayload());

    // 'unregistered-thread' isn't in the roster the state snapshot
    // seeded, so it has no discussionLiveTail registration — this must
    // not throw, and must not populate anyone's tail.
    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'unregistered-thread',
      item: {
        id: 'item-1',
        threadId: 'unregistered-thread',
        turnIndex: 0,
        itemIndex: 0,
        kind: 'assistant_text',
        role: 'assistant',
        status: 'streaming',
        summary: 'nobody is listening',
        createdAt: 0,
        updatedAt: 0,
      },
    });
    await new Promise((resolve) => requestAnimationFrame(() => resolve(undefined)));

    expect(pane.channelLiveTail).toBeNull();
  });

  it('ignores non-assistant_text kinds even for a registered child thread', async () => {
    const pane = await buildPane(discussionThread(), [], 'a');
    emitWailsEvent('discussion:state', makeStatePayload());

    emitWailsEvent('provider:item_event', {
      action: 'upsert',
      threadId: 'advocate-thread',
      item: {
        id: 'tool-1',
        threadId: 'advocate-thread',
        turnIndex: 0,
        itemIndex: 0,
        kind: 'tool_call',
        role: 'assistant',
        status: 'running',
        summary: 'Bash: ls',
        createdAt: 0,
        updatedAt: 0,
      },
    });
    emitWailsEvent('provider:item_event', {
      action: 'delta',
      threadId: 'advocate-thread',
      itemId: 'tool-1',
      kind: 'tool_call',
      delta: 'ignored',
      updatedAt: 1,
    });
    await new Promise((resolve) => requestAnimationFrame(() => resolve(undefined)));

    expect(pane.channelLiveTail).toBeNull();
  });
});

describe('refreshDiscussionChannel fetch shape', () => {
  it('fetches with afterSeq -1 for a pane with no loaded messages (initial load)', async () => {
    const pane = await buildPane(discussionThread(), [], 'a');
    const stateMock = setBindingMock('GetChannelState', async () => makeStatePayload());
    const messagesMock = setBindingMock('GetChannelMessages', async () => [makeMessage({ sequence: 0 })]);

    await refreshDiscussionChannel(pane);

    expect(stateMock).toHaveBeenCalledWith('channel-1');
    expect(messagesMock).toHaveBeenCalledWith('channel-1', -1, 500);
    // Sequence 0 (the channel's first message) must render — this is the
    // regression the -1 cursor fixes over the old afterSeq=0 seeding.
    expect(pane.channelMessages.map((m) => m.sequence)).toEqual([0]);
  });

  it('fetches with the pane\'s highest loaded sequence once messages are loaded', async () => {
    const pane = await buildPane(discussionThread(), [], 'a');
    setBindingMock('GetChannelState', async () => makeStatePayload());
    setBindingMock('GetChannelMessages', async () => [makeMessage({ sequence: 0 })]);
    await refreshDiscussionChannel(pane);

    const messagesMock = setBindingMock('GetChannelMessages', async () => [makeMessage({ sequence: 1 })]);
    await refreshDiscussionChannel(pane);

    expect(messagesMock).toHaveBeenCalledWith('channel-1', 0, 500);
    expect(pane.channelMessages.map((m) => m.sequence)).toEqual([0, 1]);
  });

  it('is a no-op for a pane with no discussionId', async () => {
    const pane = await buildPane(makeThread({ id: 'plain-thread', discussionId: undefined }), [], 'a');
    const stateMock = setBindingMock('GetChannelState', async () => makeStatePayload());

    const result = await refreshDiscussionChannel(pane);

    expect(result).toEqual([]);
    expect(stateMock).not.toHaveBeenCalled();
  });
});

describe('transport gap recovery for discussion channels', () => {
  it('dedupes GetChannelState/GetChannelMessages by channelId across two panes on one thread', async () => {
    const thread = discussionThread();
    const paneA = await buildPane(thread, [], 'a');
    const paneB = await buildPane(thread, [], 'b');
    const stateMock = setBindingMock('GetChannelState', async () => makeStatePayload());
    const messagesMock = setBindingMock('GetChannelMessages', async () => [makeMessage({ sequence: 0 })]);

    emitWailsEvent(transportGapChannel, { channel: 'discussion:message', seq: 1 });
    // The gap handler's fetchDiscussionChannelSnapshot chains a
    // Promise.all of two RPCs through an async function's own return,
    // then a further `.then()` that applies the result to each pane —
    // more microtask hops than a bare `await Promise.resolve()` pair
    // drains, and the mock call count alone updates a hop earlier than
    // the pane state does. Wait on the actual downstream effect (pane
    // state), not the mock invocation, so this can't pass on a stale
    // pre-apply read.
    await vi.waitFor(() => expect(paneA.channelMessages).toHaveLength(1));

    // One channel shared by both panes → exactly one fetch, not two.
    expect(stateMock).toHaveBeenCalledTimes(1);
    expect(messagesMock).toHaveBeenCalledTimes(1);
    // But BOTH panes' independent channel-state instances get the result.
    expect(paneA.channelMessages).toHaveLength(1);
    expect(paneB.channelMessages).toHaveLength(1);
  });

  it('recovers via discussion:state gaps the same way as discussion:message gaps', async () => {
    const pane = await buildPane(discussionThread(), [], 'a');
    const stateMock = setBindingMock('GetChannelState', async () => makeStatePayload({ turnCount: 4 }));
    setBindingMock('GetChannelMessages', async () => []);

    emitWailsEvent(transportGapChannel, { channel: 'discussion:state', seq: 2 });
    await vi.waitFor(() => expect(pane.channelTurnCount).toBe(4));

    expect(stateMock).toHaveBeenCalledTimes(1);
    expect(pane.channelTurnCount).toBe(4);
  });

  it('does not fetch discussion channels for panes with no discussionId', async () => {
    await buildPane(makeThread({ id: 'plain-thread', discussionId: undefined }), [], 'a');
    const stateMock = setBindingMock('GetChannelState', async () => makeStatePayload());

    emitWailsEvent(transportGapChannel, { channel: 'discussion:message', seq: 1 });
    await Promise.resolve();
    await Promise.resolve();

    expect(stateMock).not.toHaveBeenCalled();
  });

  // The counterpart to the default fallback below: channels whose very next
  // frame repairs them must NOT fall through to it. They are also the
  // highest-rate channels on the wire and therefore the likeliest to be the
  // ones a full subscriber buffer drops, so a fallthrough would turn every
  // overflow into a full sidebar + pane refetch for self-healing data.
  it('does not refresh panes for gaps on self-healing channels', async () => {
    const pane = await buildPane(makeThread({ id: 'plain-thread' }), [], 'a');
    const refreshSpy = vi.spyOn(pane, 'refreshFromBackend').mockResolvedValue(undefined);
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});

    for (const channel of ['system:stats', 'highlight:seed', 'highlight:diff_seed']) {
      emitWailsEvent(transportGapChannel, { channel, seq: 1 });
    }
    await Promise.resolve();

    expect(refreshSpy).not.toHaveBeenCalled();
    expect(warnSpy).not.toHaveBeenCalled();
    refreshSpy.mockRestore();
    warnSpy.mockRestore();
  });

  it('keeps the unknown-channel default fallback working after adding discussion cases', async () => {
    const pane = await buildPane(makeThread({ id: 'plain-thread' }), [], 'a');
    const refreshSpy = vi.spyOn(pane, 'refreshFromBackend').mockResolvedValue(undefined);
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});

    emitWailsEvent(transportGapChannel, { channel: 'some:future-channel', seq: 1 });
    await Promise.resolve();

    expect(refreshSpy).toHaveBeenCalled();
    expect(warnSpy).toHaveBeenCalled();
    refreshSpy.mockRestore();
    warnSpy.mockRestore();
  });
});
