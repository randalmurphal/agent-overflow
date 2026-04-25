// Tests drive a hand-rolled MockWebSocket through the WSClient state
// machine. We avoid pulling in `ws` or any other npm package — the
// fake here is small enough that adding a dependency would cost more
// than it saves.
//
// Coverage matrix:
//   - first connect lazily on call/subscribe; subsequent calls share it
//   - RPC round-trip (success + server error); all FrameError codes
//   - subscribe receives event frames and dispatches; unsubscribe stops
//   - disconnect rejects pending RPCs with DisconnectedError
//   - reconnect re-sends the replay frame with lastSeqByChannel
//   - gap event fires both console.warn and the synthetic
//     transport:gap channel
//   - exponential backoff: second reconnect waits >first delay
//   - bootstrap fetch validation: 404, malformed JSON, missing fields,
//     non-ws scheme
//   - close() during pending reconnect cancels the timer
//   - subscriber-throw isolation; unsubscribe-during-dispatch
//   - send-throw fails the matching pending RPC

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  createWSClient,
  DisconnectedError,
  MAX_PENDING_RPCS,
  MAX_REPLAY_CHANNELS,
  TransportError,
  transportGapChannel,
} from './wsClient';

// MockWebSocket is a hand-rolled fake that exposes the same shape as
// the WSLike interface the wsClient depends on. Tests drive it via
// `acceptOpen`, `pushFrame`, and `triggerClose`.
class MockWebSocket {
  static instances: MockWebSocket[] = [];
  static reset(): void {
    MockWebSocket.instances = [];
  }

  url: string;
  readyState = 0; // CONNECTING

  // Frames the client has sent. Captured as parsed JSON for ergonomic
  // assertions.
  sent: Array<Record<string, unknown>> = [];

  // When set, `send` throws this value rather than appending to `sent`.
  // Drives the send-failure branch in tests.
  sendError: Error | null = null;

  private listeners = {
    open: new Set<() => void>(),
    close: new Set<(ev: CloseEvent) => void>(),
    error: new Set<(ev: Event) => void>(),
    message: new Set<(ev: MessageEvent) => void>(),
  };

  constructor(url: string) {
    this.url = url;
    MockWebSocket.instances.push(this);
  }

  send(data: string): void {
    if (this.sendError) throw this.sendError;
    this.sent.push(JSON.parse(data) as Record<string, unknown>);
  }

  close(_code?: number, _reason?: string): void {
    this.triggerClose();
  }

  addEventListener(type: 'open', listener: () => void): void;
  addEventListener(type: 'close', listener: (ev: CloseEvent) => void): void;
  addEventListener(type: 'error', listener: (ev: Event) => void): void;
  addEventListener(type: 'message', listener: (ev: MessageEvent) => void): void;
  addEventListener(type: string, listener: unknown): void {
    if (type === 'open') this.listeners.open.add(listener as () => void);
    else if (type === 'close') this.listeners.close.add(listener as (ev: CloseEvent) => void);
    else if (type === 'error') this.listeners.error.add(listener as (ev: Event) => void);
    else if (type === 'message') this.listeners.message.add(listener as (ev: MessageEvent) => void);
  }

  // Test-driver helpers ----------------------------------------------------

  acceptOpen(): void {
    this.readyState = 1; // OPEN
    for (const fn of [...this.listeners.open]) fn();
  }

  pushFrame(frame: unknown): void {
    const ev = new MessageEvent('message', { data: JSON.stringify(frame) });
    for (const fn of [...this.listeners.message]) fn(ev);
  }

  triggerClose(): void {
    if (this.readyState === 3) return;
    this.readyState = 3; // CLOSED
    const ev = new CloseEvent('close');
    for (const fn of [...this.listeners.close]) fn(ev);
  }

  triggerError(): void {
    const ev = new Event('error');
    for (const fn of [...this.listeners.error]) fn(ev);
  }
}

// FakeCtor centralises the `as unknown as new (url: string) => MockWebSocket`
// dance. Each test passes it to createWSClient instead of repeating the
// cast.
const FakeCtor = MockWebSocket as unknown as new (url: string) => MockWebSocket;

// flushMicrotasks yields the event loop a few times so promise-chained
// callbacks (ensureConnected.then(...)) run before the test reads
// state. Two ticks covers our actual chain depth; bumping if a future
// test introduces deeper chaining is fine.
async function flushMicrotasks(): Promise<void> {
  await Promise.resolve();
  await Promise.resolve();
}

const bootstrap = async () => ({ wsUrl: 'ws://example/ws', token: 'test-token' });

describe('WSClient', () => {
  beforeEach(() => {
    MockWebSocket.reset();
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it('connects lazily and reuses the connection across calls', async () => {
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });

    expect(MockWebSocket.instances).toHaveLength(0);

    const p1 = client.callByID(123, ['arg']);
    await flushMicrotasks();
    expect(MockWebSocket.instances).toHaveLength(1);

    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    // Replay frame sent on open, then the RPC frame.
    expect(ws.sent[0]).toMatchObject({ type: 'replay' });
    expect(ws.sent[1]).toMatchObject({ type: 'rpc', methodId: 123, params: ['arg'] });
    const rpcId = ws.sent[1]!.id as string;
    ws.pushFrame({ type: 'rpc', id: rpcId, result: 'ok' });
    await expect(p1).resolves.toBe('ok');

    // Second call reuses the open socket.
    const p2 = client.callByName('Foo', [42]);
    await flushMicrotasks();
    expect(MockWebSocket.instances).toHaveLength(1);
    const rpc2Id = ws.sent[2]!.id as string;
    expect(ws.sent[2]).toMatchObject({ type: 'rpc', method: 'Foo', params: [42] });
    ws.pushFrame({ type: 'rpc', id: rpc2Id, result: 'two' });
    await expect(p2).resolves.toBe('two');

    client.close();
  });

  it('rejects with TransportError on a server-side FrameError', async () => {
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });

    const p = client.callByID(7, []);
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();
    const id = ws.sent[1]!.id as string;
    ws.pushFrame({ type: 'rpc', id, error: { code: 'method_not_found', message: 'missing' } });
    let caught: unknown;
    try {
      await p;
    } catch (err) {
      caught = err;
    }
    expect(caught).toBeInstanceOf(TransportError);
    expect((caught as TransportError).code).toBe('method_not_found');
    expect((caught as TransportError).message).toBe('missing');

    client.close();
  });

  it('subscribe receives event frames and unsubscribe stops them', async () => {
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });

    const seen: unknown[] = [];
    const unsubscribe = client.subscribe('thread:updated', (data) => {
      seen.push(data);
    });

    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    ws.pushFrame({ type: 'event', channel: 'thread:updated', seq: 1, data: { id: 'a' } });
    ws.pushFrame({ type: 'event', channel: 'thread:updated', seq: 2, data: { id: 'b' } });
    expect(seen).toEqual([{ id: 'a' }, { id: 'b' }]);

    unsubscribe();
    ws.pushFrame({ type: 'event', channel: 'thread:updated', seq: 3, data: { id: 'c' } });
    expect(seen).toHaveLength(2);

    client.close();
  });

  it('rejects pending RPCs with DisconnectedError on close', async () => {
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });

    const p = client.callByID(1, []);
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    ws.triggerClose();
    let caught: unknown;
    try {
      await p;
    } catch (err) {
      caught = err;
    }
    expect(caught).toBeInstanceOf(DisconnectedError);

    client.close();
  });

  it('re-sends replay frame on reconnect with lastSeqByChannel', async () => {
    vi.useFakeTimers();
    // Stub Math.random so the jitter delay is deterministic. Initial
    // backoff is 250ms * 2^0 = 250; pick 0.5 so delay = 125. We just
    // need a value < the backoff cap and >0 so the timer actually fires.
    vi.spyOn(Math, 'random').mockReturnValue(0.5);

    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });

    client.subscribe('thread:updated', () => {});
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    first.pushFrame({ type: 'event', channel: 'thread:updated', seq: 7, data: { id: 'x' } });
    first.pushFrame({ type: 'event', channel: 'other:channel', seq: 3, data: { v: 1 } });

    first.triggerClose();
    // Drain the reconnect timer (Math.random=0.5, attempt=0 -> 125ms).
    await vi.advanceTimersByTimeAsync(125);

    expect(MockWebSocket.instances).toHaveLength(2);
    const second = MockWebSocket.instances[1]!;
    second.acceptOpen();
    await flushMicrotasks();

    expect(second.sent[0]).toEqual({
      type: 'replay',
      lastSeqByChannel: { 'thread:updated': 7, 'other:channel': 3 },
    });

    client.close();
  });

  it('emits transport:gap and console.warn on a gap event', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });

    const gaps: unknown[] = [];
    const channel: unknown[] = [];
    client.subscribe(transportGapChannel, (data) => {
      gaps.push(data);
    });
    client.subscribe('provider:item_event', (data) => {
      channel.push(data);
    });
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    ws.pushFrame({
      type: 'event',
      channel: 'provider:item_event',
      seq: 42,
      data: { kind: 'tool_call' },
      gap: true,
    });

    expect(warn).toHaveBeenCalled();
    expect(gaps).toEqual([{ channel: 'provider:item_event', seq: 42 }]);
    // The originating event still dispatches to its channel — gap is a
    // signal that the client's history is incomplete, not an instruction
    // to drop the event itself.
    expect(channel).toEqual([{ kind: 'tool_call' }]);

    client.close();
  });

  it('backs off exponentially between reconnect attempts', async () => {
    vi.useFakeTimers();
    // random=0 makes the jittered delay 0; we want to verify the
    // `base` doubles between attempts. Capture the timer delays via a
    // setTimeout spy.
    vi.spyOn(Math, 'random').mockReturnValue(0.999);

    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });

    client.subscribe('foo', () => {});
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);

    // First reconnect: base = 250 * 2^0 = 250, jitter ~= 250 * 0.999.
    first.triggerClose();
    await vi.advanceTimersByTimeAsync(125);
    expect(MockWebSocket.instances).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(150);
    expect(MockWebSocket.instances).toHaveLength(2);

    const second = MockWebSocket.instances[1]!;
    // Drop the second connect before it opens so the next attempt
    // increments the backoff exponent.
    second.triggerClose();

    // Second reconnect: base = 250 * 2^1 = 500. We confirm 250ms is
    // not enough, but 500ms is.
    await vi.advanceTimersByTimeAsync(250);
    expect(MockWebSocket.instances).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(300);
    expect(MockWebSocket.instances).toHaveLength(3);

    client.close();
  });

  it('reconnects when bootstrap fetch returns 404', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    vi.spyOn(console, 'warn').mockImplementation(() => {});

    const fetchSpy = vi
      .fn<() => Promise<{ wsUrl: string; token: string }>>()
      // First two calls reject; the third resolves so the test can
      // verify the recovery path actually opens a socket.
      .mockRejectedValueOnce(new Error('bootstrap fetch failed: HTTP 404'))
      .mockRejectedValueOnce(new Error('bootstrap fetch failed: HTTP 404'))
      .mockResolvedValueOnce({ wsUrl: 'ws://example/ws', token: 't' });

    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap: fetchSpy });
    client.subscribe('foo', () => {});

    // First attempt: bootstrap rejects synchronously, scheduleReconnect
    // queues a retry. Drain microtasks first so the rejection settles.
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchSpy).toHaveBeenCalledTimes(1);
    expect(MockWebSocket.instances).toHaveLength(0);

    // Backoff for attempt=0 is up to 250ms; with random=0.5 it's 125ms.
    await vi.advanceTimersByTimeAsync(150);
    expect(fetchSpy).toHaveBeenCalledTimes(2);

    // attempt=1 -> base=500 -> jitter 250ms.
    await vi.advanceTimersByTimeAsync(300);
    expect(fetchSpy).toHaveBeenCalledTimes(3);
    expect(MockWebSocket.instances).toHaveLength(1);

    client.close();
  });

  it('rejects on malformed bootstrap JSON via fetch path', async () => {
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    // Stub global fetch with a response that returns invalid JSON.
    const fetchMock = vi.fn(async () => ({
      ok: true,
      status: 200,
      headers: new Headers({ 'content-type': 'application/json' }),
      json: async () => {
        throw new SyntaxError('Unexpected token');
      },
    }));
    vi.stubGlobal('fetch', fetchMock);

    const client = createWSClient({ WebSocketCtor: FakeCtor });
    const p = client.callByID(1, []);
    let caught: unknown;
    try {
      await p;
    } catch (err) {
      caught = err;
    }
    expect(caught).toBeInstanceOf(SyntaxError);
    client.close();
    vi.unstubAllGlobals();
  });

  it('rejects bootstrap missing wsUrl/token', async () => {
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    const fetchMock = vi.fn(async () => ({
      ok: true,
      status: 200,
      headers: new Headers({ 'content-type': 'application/json' }),
      json: async () => ({ wsUrl: 'ws://example/ws' }),
    }));
    vi.stubGlobal('fetch', fetchMock);

    const client = createWSClient({ WebSocketCtor: FakeCtor });
    const p = client.callByID(1, []);
    let caught: unknown;
    try {
      await p;
    } catch (err) {
      caught = err;
    }
    expect((caught as Error).message).toMatch(/missing wsUrl\/token/);
    client.close();
    vi.unstubAllGlobals();
  });

  it('rejects bootstrap with non-ws scheme', async () => {
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    const fetchMock = vi.fn(async () => ({
      ok: true,
      status: 200,
      headers: new Headers({ 'content-type': 'application/json' }),
      json: async () => ({ wsUrl: 'http://evil/', token: 'x' }),
    }));
    vi.stubGlobal('fetch', fetchMock);

    const client = createWSClient({ WebSocketCtor: FakeCtor });
    const p = client.callByID(1, []);
    let caught: unknown;
    try {
      await p;
    } catch (err) {
      caught = err;
    }
    expect((caught as Error).message).toMatch(/scheme not ws\/wss/);
    client.close();
    vi.unstubAllGlobals();
  });

  it('reads bootstrap from window.__AO_BOOTSTRAP__ without fetching', async () => {
    const fetchMock = vi.fn(async () => {
      throw new Error('fetch should not be called');
    });
    vi.stubGlobal('fetch', fetchMock);
    (globalThis as { __AO_BOOTSTRAP__?: { wsUrl: string; token: string } }).__AO_BOOTSTRAP__ = {
      wsUrl: 'ws://injected/',
      token: 'inj',
    };

    try {
      const client = createWSClient({ WebSocketCtor: FakeCtor });
      const p = client.callByID(1, []);
      await flushMicrotasks();
      expect(fetchMock).not.toHaveBeenCalled();
      expect(MockWebSocket.instances).toHaveLength(1);
      const ws = MockWebSocket.instances[0]!;
      expect(ws.url).toContain('ws://injected/');
      expect(ws.url).toContain('token=inj');

      ws.acceptOpen();
      await flushMicrotasks();
      const id = ws.sent[1]!.id as string;
      ws.pushFrame({ type: 'rpc', id, result: 'ok' });
      await expect(p).resolves.toBe('ok');
      client.close();
    } finally {
      delete (globalThis as { __AO_BOOTSTRAP__?: { wsUrl: string; token: string } }).__AO_BOOTSTRAP__;
      vi.unstubAllGlobals();
    }
  });

  it('omits methodId when caller passes 0 (sentinel for ByName fallback)', async () => {
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });

    // ByID(0, …) — methodId 0 is reserved as the "fall through to method
    // name" sentinel in Wails-generated bindings.
    const p = client.callByID(0, ['x']);
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();
    const frame = ws.sent[1]! as { methodId?: number; type: string; id: string };
    expect(frame.type).toBe('rpc');
    expect(frame).not.toHaveProperty('methodId');

    ws.pushFrame({ type: 'rpc', id: frame.id, result: 'ok' });
    await expect(p).resolves.toBe('ok');
    client.close();
  });

  it('rejects an RPC with code "timeout" after RPC_TIMEOUT_MS', async () => {
    vi.useFakeTimers();
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    const p = client.callByID(1, []);
    // Attach the catch synchronously so the eventual rejection has a
    // handler the moment it lands. Without this the test passes — the
    // rejection IS observed via `await p` further down — but Node prints
    // a PromiseRejectionHandledWarning because the handler arrived in a
    // later microtask than the rejection.
    const settled = p.catch((err) => err);
    // Advance microtasks so the bootstrap promise settles and the
    // socket is created.
    await vi.advanceTimersByTimeAsync(0);
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    // Default RPC_TIMEOUT_MS is 30_000; advance past it.
    await vi.advanceTimersByTimeAsync(30_001);
    const caught = await settled;
    expect(caught).toBeInstanceOf(TransportError);
    expect((caught as TransportError).code).toBe('timeout');
    client.close();
  });

  it('close() during pending reconnect cancels the timer', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);

    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    client.subscribe('foo', () => {});
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);

    first.triggerClose();
    expect(MockWebSocket.instances).toHaveLength(1);

    // Close before the backoff fires.
    client.close();

    // Advance well past the backoff window — no new socket should appear.
    await vi.advanceTimersByTimeAsync(5_000);
    expect(MockWebSocket.instances).toHaveLength(1);
  });

  it('isolates a throwing subscriber so siblings on the channel still receive', async () => {
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    const a = vi.fn(() => {
      throw new Error('boom');
    });
    const b = vi.fn();
    client.subscribe('x', a);
    client.subscribe('x', b);

    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    ws.pushFrame({ type: 'event', channel: 'x', seq: 1, data: 'hi' });
    expect(a).toHaveBeenCalled();
    expect(b).toHaveBeenCalled();
    client.close();
  });

  it('respects unsubscribe of a sibling handler that occurs during dispatch', async () => {
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    const b = vi.fn();
    let offB: (() => void) | null = null;
    const a = vi.fn(() => {
      offB?.();
    });
    client.subscribe('x', a);
    offB = client.subscribe('x', b);

    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    ws.pushFrame({ type: 'event', channel: 'x', seq: 1, data: 'hi' });
    expect(a).toHaveBeenCalledTimes(1);
    // a unsubscribed b synchronously, so b should not see this event.
    expect(b).not.toHaveBeenCalled();
    client.close();
  });

  it.each([
    'bad_params',
    'method_error',
    'internal',
    'shutting_down',
    'method_not_found',
  ])('preserves FrameError code %s on the caller', async (code) => {
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    const p = client.callByID(1, []);
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();
    const id = ws.sent[1]!.id as string;
    ws.pushFrame({ type: 'rpc', id, error: { code, message: `${code}-msg` } });
    let caught: unknown;
    try {
      await p;
    } catch (err) {
      caught = err;
    }
    expect(caught).toBeInstanceOf(TransportError);
    expect((caught as TransportError).code).toBe(code);
    client.close();
  });

  it('updates lastSeqByChannel even when no subscribers are registered', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    // Trigger an initial connect via an RPC the test never resolves.
    client.callByID(1, []).catch(() => {});
    await vi.advanceTimersByTimeAsync(0);
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    // No subscribers on this channel.
    ws.pushFrame({ type: 'event', channel: 'unsubscribed', seq: 9, data: { v: 1 } });
    ws.triggerClose();
    await vi.advanceTimersByTimeAsync(125);

    const second = MockWebSocket.instances[1]!;
    second.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    expect(second.sent[0]).toEqual({
      type: 'replay',
      lastSeqByChannel: { unsubscribed: 9 },
    });
    client.close();
  });

  it('fails the matching pending RPC with DisconnectedError when send throws', async () => {
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    const p = client.callByID(1, []);
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    // Pre-arm the failure so the next .send (the RPC frame) throws.
    // The replay frame fires first on open; arm AFTER it goes out.
    ws.acceptOpen();
    await flushMicrotasks();
    // At this point the replay frame has flushed; arm send to fail and
    // fire a follow-up RPC.
    ws.sendError = new Error('send broken');
    const p2 = client.callByID(2, []);
    await flushMicrotasks();
    let caught: unknown;
    try {
      await p2;
    } catch (err) {
      caught = err;
    }
    expect(caught).toBeInstanceOf(DisconnectedError);
    expect((caught as Error).message).toMatch(/send failed/);

    // Sanity: the original RPC is still pending until the socket actually
    // closes — failing one frame's send doesn't drain the whole map.
    ws.triggerClose();
    let caughtFirst: unknown;
    try {
      await p;
    } catch (err) {
      caughtFirst = err;
    }
    expect(caughtFirst).toBeInstanceOf(DisconnectedError);
    client.close();
  });

  it('evicts the oldest channel from lastSeqByChannel when capacity exceeds MAX_REPLAY_CHANNELS', async () => {
    // The bookkeeping cap mirrors the server-side limit; without
    // eviction, a malicious or flaky remote could pump unique channel
    // names and drive the client's Map past the wire-frame cap. This
    // test pins the eviction order: the oldest entry (insertion order)
    // is the one that drops.
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    client.subscribe('any-channel', () => {});
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    // Drive past the cap. recordChannelSeq is private but reachable
    // via the message handler — push event frames with synthetic
    // channel names. The eviction policy is "oldest first" via Map
    // insertion order.
    for (let i = 0; i < MAX_REPLAY_CHANNELS + 5; i++) {
      ws.pushFrame({
        type: 'event',
        channel: `ch-${i}`,
        seq: i + 1,
        data: { i },
      });
    }
    await flushMicrotasks();

    // Disconnect to force a reconnect and capture the next replay frame.
    // The replay frame's lastSeqByChannel is exactly the post-eviction
    // bookkeeping; the oldest 5 entries (ch-0..ch-4) should be missing.
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    ws.triggerClose();
    await vi.advanceTimersByTimeAsync(125);

    const second = MockWebSocket.instances[1]!;
    second.acceptOpen();
    await flushMicrotasks();
    const replay = second.sent[0]! as { lastSeqByChannel: Record<string, number> };
    expect(replay.lastSeqByChannel).toBeDefined();
    const trackedChannels = Object.keys(replay.lastSeqByChannel);
    expect(trackedChannels.length).toBeLessThanOrEqual(MAX_REPLAY_CHANNELS);
    // The newest channel must still be tracked; the oldest must NOT be.
    const newestKey = `ch-${MAX_REPLAY_CHANNELS + 4}`;
    expect(replay.lastSeqByChannel[newestKey]).toBe(MAX_REPLAY_CHANNELS + 5);
    // The first batch should have been evicted.
    expect(replay.lastSeqByChannel['ch-0']).toBeUndefined();

    client.close();
  });

  it('rejects new RPCs with client_overloaded when pending exceeds MAX_PENDING_RPCS', async () => {
    // Defensive cap so a buggy caller can't unbounded-grow the pending
    // map. The first MAX_PENDING_RPCS calls succeed (sit pending); the
    // overflow call rejects synchronously with a TransportError carrying
    // the documented code.
    vi.useFakeTimers();
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });

    // Queue enough RPCs to fill the cap. We don't have to await any —
    // they remain pending until close. Filling MAX_PENDING_RPCS in a
    // single test is heavy (10k entries) but still fast in v8 because
    // the path is fully synchronous up to the WS write.
    const queued: Promise<unknown>[] = [];
    for (let i = 0; i < MAX_PENDING_RPCS; i++) {
      queued.push(client.callByID(1, []).catch(() => {}));
    }
    // The next call exceeds the cap. dispatchRPC rejects synchronously
    // before scheduling any timer, so we don't need to advance timers.
    let caught: unknown;
    try {
      await client.callByID(1, []);
    } catch (err) {
      caught = err;
    }
    expect(caught).toBeInstanceOf(TransportError);
    expect((caught as TransportError).code).toBe('client_overloaded');

    // Drain queued promises by closing the client; otherwise they
    // would surface as unhandled rejections after this test exits.
    client.close();
    await Promise.allSettled(queued);
  });

  it('onStatusChange reports connected after open and reconnecting after close', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);

    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    const events: string[] = [];
    const off = client.onStatusChange((snap) => {
      events.push(snap.status);
    });

    // First snapshot: seeded synchronously as 'disconnected'.
    expect(events).toEqual(['disconnected']);

    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    expect(events).toContain('connected');

    first.triggerClose();
    // The close handler synchronously sets 'reconnecting'.
    expect(events.at(-1)).toBe('reconnecting');

    off();
    client.close();
  });

  it('triggerReconnect cancels the queued backoff and starts a fresh attempt', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);

    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    first.triggerClose();
    // Don't run the backoff timer; instead force-reconnect.
    expect(MockWebSocket.instances).toHaveLength(1);
    client.triggerReconnect();
    await vi.advanceTimersByTimeAsync(0);
    // A second socket exists immediately, without waiting on the
    // exponential backoff that scheduleReconnect would otherwise impose.
    expect(MockWebSocket.instances).toHaveLength(2);

    client.close();
  });
});
