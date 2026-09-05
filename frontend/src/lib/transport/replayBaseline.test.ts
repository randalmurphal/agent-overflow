import { afterEach, expect, it, vi } from 'vitest';
import { createWSClient } from './wsClient';
import { FakeCtor, flushMicrotasks, MockWebSocket } from '../../test/helpers/mockWebSocket';

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

it('announces reconnect replay boundaries and cancels an interrupted recovery', async () => {
  vi.useFakeTimers();
  vi.spyOn(Math, 'random').mockReturnValue(0.5);
  MockWebSocket.reset();
  const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap: async () => ({
    wsUrl: 'ws://example/ws', token: 'test-token', remote: true,
  }) });
  const phases: string[] = [];
  client.onReplay((phase) => phases.push(phase));
  client.subscribe('test', () => {});
  await vi.advanceTimersByTimeAsync(0);
  const first = MockWebSocket.instances[0]!;
  first.acceptOpen();
  first.pushFrame({ type: 'replay' });
  expect(phases).toEqual([]);
  first.triggerClose();
  await vi.advanceTimersByTimeAsync(125);
  const second = MockWebSocket.instances[1]!;
  second.acceptOpen();
  expect(phases).toEqual(['start']);
  second.pushFrame({ type: 'replay' });
  second.pushFrame({ type: 'replay' });
  expect(phases).toEqual(['start', 'complete']);
  second.triggerClose();
  await vi.advanceTimersByTimeAsync(500);
  const third = MockWebSocket.instances[2]!;
  third.acceptOpen();
  client.close();
  third.pushFrame({ type: 'replay' });
  expect(phases).toEqual(['start', 'complete', 'start', 'cancel']);
});

it.each([0, 7])('replays a completion never received by this client (baseline %i)', async (completedBeforeConnect) => {
  vi.useFakeTimers();
  vi.spyOn(Math, 'random').mockReturnValue(0.5);
  MockWebSocket.reset();
  const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap: async () => ({
    wsUrl: 'ws://example/ws', token: 'test-token', remote: true,
  }) });
  const completed = vi.fn();
  client.subscribe('provider:turn_completed', completed);
  await vi.advanceTimersByTimeAsync(0);
  const first = MockWebSocket.instances[0]!;
  first.acceptOpen();
  first.pushFrame({ type: 'hello', protocolVersion: 1, capabilities: [], serverTimeMs: 0,
    replayBaseline: { 'provider:turn_started': 10, 'provider:turn_completed': completedBeforeConnect },
  });
  first.pushFrame({ type: 'event', channel: 'provider:turn_started', seq: 11, data: { turnId: 'turn' } });
  first.triggerClose();
  await vi.advanceTimersByTimeAsync(125);
  const second = MockWebSocket.instances[1]!;
  second.acceptOpen();
  await flushMicrotasks();
  expect(second.sent[0]).toMatchObject({ type: 'replay', lastSeqByChannel: {
    'provider:turn_started': 11, 'provider:turn_completed': completedBeforeConnect,
  } });
  // The new hello must not advance the old cursor past the missed event.
  second.pushFrame({ type: 'hello', protocolVersion: 1, capabilities: [], serverTimeMs: 1,
    replayBaseline: { 'provider:turn_completed': completedBeforeConnect + 1 },
  });
  second.pushFrame({ type: 'event', channel: 'provider:turn_completed',
    seq: completedBeforeConnect + 1, data: { turnId: 'turn' } });
  expect(completed).toHaveBeenCalledExactlyOnceWith({ turnId: 'turn' });
  client.close();
});

it('ignores invalid baseline cursors and preserves historical notification activation replay', async () => {
  vi.useFakeTimers();
  MockWebSocket.reset();
  const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap: async () => ({
    wsUrl: 'ws://example/ws', token: 'test-token', remote: true,
  }) });
  const activated = vi.fn();
  client.subscribe('notification:activated', activated);
  await vi.advanceTimersByTimeAsync(0);
  const socket = MockWebSocket.instances[0]!;
  socket.acceptOpen();
  socket.pushFrame({ type: 'hello', protocolVersion: 1, capabilities: [], serverTimeMs: 0,
    replayBaseline: {
      'notification:activated': 100, 'provider:turn_completed': -1,
      fraction: 0.5, string: '3', unsafe: Number.MAX_SAFE_INTEGER + 1,
    },
  });
  socket.pushFrame({ type: 'event', channel: 'notification:activated', seq: 1, data: 'open thread' });
  socket.pushFrame({ type: 'replay' });
  expect(activated).toHaveBeenCalledExactlyOnceWith('open thread');
  socket.triggerClose();
  await vi.advanceTimersByTimeAsync(250);
  const next = MockWebSocket.instances[1]!;
  next.acceptOpen();
  await flushMicrotasks();
  expect(next.sent[0]).toMatchObject({ type: 'replay', lastSeqByChannel: { 'notification:activated': 1 } });
  expect(Object.keys(next.sent[0].lastSeqByChannel as object)).toEqual(['notification:activated']);
  client.close();
});
