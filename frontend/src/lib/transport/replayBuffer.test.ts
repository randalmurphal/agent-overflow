import { describe, expect, it, vi } from 'vitest';
import {
  MAX_REPLAY_BUFFER_CHARS,
  MAX_REPLAY_BUFFER_EVENTS,
  ReplayBuffer,
} from './replayBuffer';
import { MAX_REPLAY_CHANNELS } from './frames';

describe('ReplayBuffer', () => {
  it('orders each channel across live/replay overlap while retaining cross-channel slots', () => {
    const buffer = new ReplayBuffer();
    for (const [channel, seq] of [['a', 3], ['b', 2], ['a', 1], ['b', 1], ['a', 2]] as const) {
      buffer.push({ channel, seq, data: `${channel}${seq}` });
    }
    const received: string[] = [];
    const recover = vi.fn();
    buffer.drain((event) => received.push(String(event.data)), recover);
    expect(received).toEqual(['a1', 'b1', 'a2', 'b2', 'a3']);
    expect(recover).not.toHaveBeenCalled();
  });

  it('places lower reset markers before new events and gaps before equal-sequence duplicates', () => {
    const buffer = new ReplayBuffer();
    buffer.push({ channel: 'a', seq: 2, data: 'live' });
    buffer.push({ channel: 'a', seq: 0, data: null, gap: true });
    buffer.push({ channel: 'a', seq: 1, data: 'replayed' });
    buffer.push({ channel: 'a', seq: 2, data: null, gap: true });
    const received: unknown[] = [];
    buffer.drain((event) => received.push([event.seq, event.gap === true, event.data]), vi.fn());
    expect(received).toEqual([
      [0, true, null], [1, false, 'replayed'], [2, true, null], [2, false, 'live'],
    ]);
  });

  it('discards all payloads on event overflow but tracks the latest recovery heads', () => {
    const buffer = new ReplayBuffer();
    for (let seq = 1; seq <= MAX_REPLAY_BUFFER_EVENTS + 1; seq++) {
      buffer.push({ channel: 'a', seq, data: 'text' });
    }
    buffer.push({ channel: 'a', seq: 2, data: 'older replay' });
    buffer.push({ channel: 'b', seq: 7, data: 'other channel' });
    const deliver = vi.fn();
    const recover = vi.fn();
    buffer.drain(deliver, recover);
    expect(deliver).not.toHaveBeenCalled();
    expect(recover).toHaveBeenCalledExactlyOnceWith(new Map([
      ['a', MAX_REPLAY_BUFFER_EVENTS + 1], ['b', 7],
    ]));
  });

  it('counts frame bytes without retaining payloads beyond the byte budget', () => {
    const buffer = new ReplayBuffer();
    buffer.addFrameSize(MAX_REPLAY_BUFFER_CHARS);
    buffer.push({ channel: 'a', seq: 1, data: 'at limit' });
    buffer.addFrameSize(1);
    buffer.push({ channel: 'a', seq: 2, data: 'over limit' });
    const deliver = vi.fn();
    const recover = vi.fn();
    buffer.drain(deliver, recover);
    expect(deliver).not.toHaveBeenCalled();
    expect(recover).toHaveBeenCalledExactlyOnceWith(new Map([['a', 2]]));
  });

  it('bounds channel metadata after overflow while continuing to advance known heads', () => {
    const buffer = new ReplayBuffer();
    for (let index = 0; index < MAX_REPLAY_CHANNELS + 100; index++) {
      buffer.push({ channel: `channel-${index}`, seq: 1, data: null });
    }
    buffer.push({ channel: 'channel-0', seq: 12, data: null });
    const deliver = vi.fn();
    let heads: ReadonlyMap<string, number> | undefined;
    buffer.drain(deliver, (result) => { heads = result; });
    expect(deliver).not.toHaveBeenCalled();
    expect(heads?.size).toBe(MAX_REPLAY_CHANNELS);
    expect(heads?.get('channel-0')).toBe(12);
    expect(heads?.has(`channel-${MAX_REPLAY_CHANNELS}`)).toBe(false);
  });

  it('also bounds retained channel names, not only the number of channels', () => {
    const buffer = new ReplayBuffer();
    buffer.push({ channel: 'a'.repeat(MAX_REPLAY_BUFFER_CHARS + 1), seq: 1, data: null });
    buffer.push({ channel: 'known', seq: 2, data: null });
    const deliver = vi.fn();
    const recover = vi.fn();
    buffer.drain(deliver, recover);
    expect(deliver).not.toHaveBeenCalled();
    expect(recover).toHaveBeenCalledExactlyOnceWith(new Map([['known', 2]]));
  });
});
