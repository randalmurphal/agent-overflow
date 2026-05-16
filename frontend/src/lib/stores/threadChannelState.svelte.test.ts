import { describe, expect, it } from 'vitest';
import type { ChannelMessage } from '../types/discussion';
import { createThreadChannelState } from './threadChannelState.svelte';

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

describe('createThreadChannelState', () => {
  it('merges messages by sequence while preserving timeline order', () => {
    const state = createThreadChannelState();

    state.mergeMessages([
      makeMessage({ id: 'message-3', sequence: 3, content: 'third' }),
      makeMessage({ id: 'message-1', sequence: 1, content: 'first' }),
    ]);
    state.mergeMessages([
      makeMessage({ id: 'message-2', sequence: 2, content: 'second' }),
      makeMessage({ id: 'duplicate-3', sequence: 3, content: 'duplicate third' }),
    ]);

    expect(state.messages.map((message) => message.sequence)).toEqual([1, 2, 3]);
    expect(state.messages.map((message) => message.content)).toEqual(['first', 'second', 'third']);
  });

  it('keeps the messages array reference when incoming messages are duplicates', () => {
    const state = createThreadChannelState();
    state.mergeMessages([makeMessage({ id: 'message-1', sequence: 1 })]);
    const before = state.messages;

    state.mergeMessages([makeMessage({ id: 'duplicate-1', sequence: 1, content: 'ignored' })]);

    expect(state.messages).toBe(before);
    expect(state.messages.map((message) => message.content)).toEqual(['hello']);
  });

  it('tracks channel status separately from messages', () => {
    const state = createThreadChannelState();

    state.mergeMessages([makeMessage()]);
    state.setStatus('concluded');

    expect(state.messages.length).toBe(1);
    expect(state.status).toBe('concluded');
  });

  it('clears messages and status for thread switches', () => {
    const state = createThreadChannelState();
    state.mergeMessages([makeMessage()]);
    state.setStatus('closed');

    state.clear();

    expect(state.messages).toEqual([]);
    expect(state.status).toBeNull();
  });
});
