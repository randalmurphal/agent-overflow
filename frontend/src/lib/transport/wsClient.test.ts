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
//     transport:gap channel; a gap marker is a resync instruction, so
//     its seq is adopted in BOTH directions and never dedup-dropped
//     (the restarted-backend case answers an above-head cursor with a
//     LOWER seq)
//   - mid-connection drop detection: a forward seq skip on a channel
//     already seen on THIS connection fires the same synthetic
//     transport:gap and still delivers the carried event. Contiguous
//     seqs, a channel's first event, a dedup-dropped duplicate, and an
//     explicit gap:true frame do not fire it; the batch path detects
//     too; a post-reconnect jump does NOT (Replay is the authority
//     across a connection boundary)
//   - exponential backoff: second reconnect waits >first delay
//   - backoff resets on stability (BACKOFF_RESET_AFTER_MS), not on
//     open — an accept-then-close server keeps climbing the ladder
//   - refused bootstrap credential latches the TERMINAL 'unauthorized'
//     state on a session that can't re-mint a token (non-loopback
//     origin, or a manifest that said remote): the ladder stops, RPCs
//     are refused locally, a manual retry un-latches and a repeat
//     refusal re-latches. A loopback session, a 1005 relay teardown,
//     and a network-down loop all keep the ordinary retry behavior.
//   - backoff collapse: an RPC / page resume / triggerReconnect fires a
//     queued backoff attempt immediately; the RPC path is rate-floored
//     (one attempt per RECONNECT_INITIAL_MS); resume-while-hidden no-ops
//   - triggerReconnect: settles the scheduled promise for queued
//     awaiters; no-ops while an attempt is in flight
//   - stale-socket watchdog: force-close after the traffic threshold,
//     armed per-connection by the first ping frame (version-skew guard,
//     reset across sockets); heartbeats keep the socket alive; remote
//     backends with in-flight RPCs are exempt mid-transfer
//   - superseded-socket identity guard: a stale socket's late events
//     can't clobber the live connection
//   - backoff caps: local vs remote ceiling, sticky across bootstrap
//     invalidation
//   - bootstrap cache invalidated every 2 consecutive pre-open failures
//   - outage diagnostics: one summary line per outage on reconnect
//   - bootstrap fetch validation: 404, malformed JSON, missing fields,
//     non-ws scheme
//   - close() during pending reconnect cancels the timer
//   - subscriber-throw isolation; unsubscribe-during-dispatch
//   - send-throw fails the matching pending RPC
//   - forward tolerance (the future-dialect fixture): unknown frame
//     types, unknown event channels, unknown fields on known frames and
//     unaddressable shapes all counted rather than thrown on, with the
//     known traffic still delivered in full and the console silent; the
//     replay cursor stays free of the mis-shaped entries that would
//     otherwise break gap recovery for the session; the per-kind tally
//     is capped against a peer naming a new type every frame
//   - the zero-seeded notification:activated cursor (the cold-launch
//     mechanism) is asked for only by a session that is local in both
//     senses; a remote session omits it, writes no checkpoint, and still
//     replays what it actually missed across a reconnect

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { BootstrapRejectedError } from './bootstrap';
import {
  BACKOFF_RESET_AFTER_MS,
  BOOTSTRAP_INVALIDATE_AFTER_FAILURES,
  createWSClient,
  DisconnectedError,
  MAX_PENDING_RPCS,
  MAX_REPLAY_CHANNELS,
  RECONNECT_INITIAL_MS,
  RECONNECT_MAX_LOCAL_MS,
  RECONNECT_MAX_REMOTE_MS,
  RETRY_ON_TRANSIENT_CLOSE,
  RPC_TIMEOUT_MS,
  STALE_TRAFFIC_THRESHOLD_MS,
  STALE_CHECK_INTERVAL_MS,
  TransportError,
  transportGapChannel,
} from './wsClient';
import { __resetRunModeForTest, isViewOnlySession } from './runMode';

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

  close(code?: number, reason?: string): void {
    this.triggerClose(code ?? 1005, reason);
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

  pushRawText(text: string): void {
    const ev = new MessageEvent('message', { data: text });
    for (const fn of [...this.listeners.message]) fn(ev);
  }

  // Default code 1006 (abnormal closure) — the shape of a network
  // death, which is what most tests simulate and what the outage
  // diagnostics record.
  // `reason` is the peer's close reason. Real deployments supply one on
  // every deliberate close (a relay teardown, a policy close), and it is
  // the single most useful field for telling those apart after the fact,
  // so the fake carries it.
  triggerClose(code = 1006, reason = ''): void {
    if (this.readyState === 3) return;
    this.readyState = 3; // CLOSED
    const ev = new CloseEvent('close', { code, reason });
    for (const fn of [...this.listeners.close]) fn(ev);
  }

  // `detail` stands in for the richer error object a non-browser socket
  // (the `--connect` shell) delivers, where the browser event carries
  // nothing.
  triggerError(detail?: Event): void {
    const ev = detail ?? new Event('error');
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
    sessionStorage.clear();
    delete (globalThis as { __AO_BOOTSTRAP__?: unknown }).__AO_BOOTSTRAP__;
    __resetRunModeForTest();
  });

  afterEach(() => {
    delete (globalThis as { __AO_BOOTSTRAP__?: unknown }).__AO_BOOTSTRAP__;
    __resetRunModeForTest();
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
    expect(ws.sent[0]).toEqual({
      type: 'replay',
      lastSeqByChannel: { 'notification:activated': 0 },
    });
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

  it('drops duplicate live/replay sequence numbers', async () => {
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    const seen: unknown[] = [];
    client.subscribe('notification:activated', (data) => seen.push(data));
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();
    ws.pushFrame({ type: 'replay' });

    const event = {
      type: 'event' as const,
      channel: 'notification:activated',
      seq: 1,
      data: { kind: 'thread', threadId: 'thread-1' },
    };
    ws.pushFrame(event);
    ws.pushFrame(event);

    expect(seen).toEqual([{ kind: 'thread', threadId: 'thread-1' }]);
    client.close();
  });

  it('persists the activation checkpoint across a page-level client reload', async () => {
    const firstClient = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    firstClient.subscribe('notification:activated', () => {});
    await flushMicrotasks();
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await flushMicrotasks();
    first.pushFrame({ type: 'replay' });
    first.pushFrame({
      type: 'event',
      channel: 'notification:activated',
      seq: 7,
      data: { kind: 'none' },
    });
    firstClient.close();

    const secondClient = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    secondClient.subscribe('notification:activated', () => {});
    await flushMicrotasks();
    const second = MockWebSocket.instances[1]!;
    second.acceptOpen();
    await flushMicrotasks();

    expect(second.sent[0]).toEqual({
      type: 'replay',
      lastSeqByChannel: { 'notification:activated': 7 },
    });
    secondClient.close();
  });

  it('does not reuse an activation checkpoint from a different backend boot', async () => {
    sessionStorage.setItem(
      'ao:notification-activation-seq',
      JSON.stringify({ scope: 'old-token', seq: 7 }),
    );
    const client = createWSClient({
      WebSocketCtor: FakeCtor,
      bootstrap: async () => ({ wsUrl: 'ws://example/ws', token: 'new-token' }),
    });
    client.subscribe('notification:activated', () => {});
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    expect(ws.sent[0]).toEqual({
      type: 'replay',
      lastSeqByChannel: { 'notification:activated': 0 },
    });
    client.close();
  });

  // The zero-seeded cursor asks the backend for the notification
  // channel's whole retained ring. That is right for the desktop window,
  // which a toast click can cold-launch before it has a socket, and wrong
  // for anything else: the queue on the other end OPENS every activation
  // it receives, so a phone attaching would have its pane walked through
  // every notification the desk has clicked since boot.
  it('does not ask for the activation ring when the manifest says the backend is remote', async () => {
    const client = createWSClient({
      WebSocketCtor: FakeCtor,
      bootstrap: async () => ({ wsUrl: 'ws://example/ws', token: 'test-token', remote: true }),
    });
    client.subscribe('notification:activated', () => {});
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    expect(ws.sent[0]).toEqual({ type: 'replay', lastSeqByChannel: {} });
    client.close();
  });

  it('does not ask for the activation ring when the page itself is not loopback-served', async () => {
    const client = createWSClient({
      WebSocketCtor: FakeCtor,
      bootstrap,
      loopbackOrigin: () => false,
    });
    client.subscribe('notification:activated', () => {});
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    expect(ws.sent[0]).toEqual({ type: 'replay', lastSeqByChannel: {} });
    client.close();
  });

  it('leaves no activation checkpoint behind on a remote session', async () => {
    const client = createWSClient({
      WebSocketCtor: FakeCtor,
      bootstrap: async () => ({ wsUrl: 'ws://example/ws', token: 'test-token', remote: true }),
    });
    client.subscribe('notification:activated', () => {});
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();
    ws.pushFrame({ type: 'replay' });
    ws.pushFrame({
      type: 'event',
      channel: 'notification:activated',
      seq: 3,
      data: { kind: 'none' },
    });

    // Nothing reads the checkpoint back on this session, so writing one
    // is a sessionStorage write per click that buys nothing.
    expect(sessionStorage.getItem('ao:notification-activation-seq')).toBeNull();
    client.close();
  });

  it('still replays what a remote session actually missed across a reconnect', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    const client = createWSClient({
      WebSocketCtor: FakeCtor,
      bootstrap: async () => ({ wsUrl: 'ws://example/ws', token: 'test-token', remote: true }),
    });
    client.subscribe('notification:activated', () => {});
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    first.pushFrame({ type: 'replay' });
    first.pushFrame({
      type: 'event',
      channel: 'notification:activated',
      seq: 5,
      data: { kind: 'none' },
    });

    first.triggerClose();
    await vi.advanceTimersByTimeAsync(125);
    const second = MockWebSocket.instances[1]!;
    second.acceptOpen();
    await flushMicrotasks();

    // Declining the cold-launch seed is not the same as opting out of
    // gap recovery: the ordinary cursor is still carried, so the session
    // asks for exactly the activations that happened while it was gone.
    expect(second.sent[0]).toEqual({
      type: 'replay',
      lastSeqByChannel: { 'notification:activated': 5 },
    });
    client.close();
  });

  it('orders a live activation that races ahead of replay completion', async () => {
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    const seen: string[] = [];
    client.subscribe('notification:activated', (data) => {
      seen.push((data as { threadId: string }).threadId);
    });
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    ws.pushFrame({
      type: 'event',
      channel: 'notification:activated',
      seq: 2,
      data: { kind: 'thread', threadId: 'thread-2' },
    });
    ws.pushFrame({
      type: 'event',
      channel: 'notification:activated',
      seq: 1,
      data: { kind: 'thread', threadId: 'thread-1' },
    });
    expect(seen).toEqual([]);

    ws.pushFrame({ type: 'replay' });
    expect(seen).toEqual(['thread-1', 'thread-2']);
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

  it('rejects with the close code and reason preserved, not a bare "socket closed"', async () => {
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });

    const p = client.callByID(1, []);
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    ws.triggerClose(1012, 'backend restarting');
    let caught: unknown;
    try {
      await p;
    } catch (err) {
      caught = err;
    }
    const err = caught as DisconnectedError;
    expect(err).toBeInstanceOf(DisconnectedError);
    expect(err.closeCode).toBe(1012);
    expect(err.closeReason).toBe('backend restarting');
    // In the MESSAGE too: the ~150 sites that render a failure read
    // `err.message`, and the cause has to survive that rendering or it
    // never reaches a human.
    expect(err.message).toContain('1012');
    expect(err.message).toContain('backend restarting');
    // Not terminal: the ladder is still running, so a store that
    // suspends on this edge will re-source on the next connect.
    expect(err.terminal).toBe(false);

    client.close();
  });

  it('carries the socket error that preceded the close as the rejection cause', async () => {
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });

    const p = client.callByID(1, []);
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    // A socket-level failure arrives before the close on every engine
    // that reports one. 1006 is the same code for every abnormal end, so
    // this object is the only thing distinguishing them.
    const detail = new ErrorEvent('error', { message: 'TLS handshake failed' });
    ws.triggerError(detail);
    ws.triggerClose(1006);

    let caught: unknown;
    try {
      await p;
    } catch (err) {
      caught = err;
    }
    expect((caught as DisconnectedError).cause).toBe(detail);

    client.close();
  });

  it('clears a previous socket error so it cannot be blamed for the next close', async () => {
    vi.useFakeTimers();
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });

    client.subscribe('thread:updated', () => {});
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    first.triggerError(new ErrorEvent('error', { message: 'first socket died' }));
    first.triggerClose(1006);

    await vi.advanceTimersByTimeAsync(300);
    const second = MockWebSocket.instances[1]!;
    second.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);

    const p = client.callByID(1, []);
    await vi.advanceTimersByTimeAsync(0);
    // Second socket dies cleanly, with no error event of its own.
    second.triggerClose(1001, 'going away');
    let caught: unknown;
    try {
      await p;
    } catch (err) {
      caught = err;
    }
    expect((caught as DisconnectedError).cause).toBeUndefined();
    expect((caught as DisconnectedError).closeReason).toBe('going away');

    client.close();
    vi.useRealTimers();
  });

  it('ships an EMPTY retry-on-transient-close allowlist', () => {
    // A tripwire, not a formality. The allowlist is the one place an RPC
    // can be re-sent without its caller knowing, and every entry trades
    // a lost answer for a possibly-duplicated action. Growing it must be
    // a reviewed decision that also updates this expectation and the
    // entry's `why`, never a quiet append.
    expect(RETRY_ON_TRANSIENT_CLOSE).toHaveLength(0);
  });

  it('does not retry an ordinary call across a transient close', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });

    const p = client.callByID(1, []);
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    first.triggerClose(1006);

    await expect(p).rejects.toBeInstanceOf(DisconnectedError);
    await vi.advanceTimersByTimeAsync(300);
    const second = MockWebSocket.instances[1]!;
    second.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    // Only the replay handshake — the dead call was NOT re-sent.
    expect(second.sent.filter((f) => f.type === 'rpc')).toHaveLength(0);

    client.close();
    vi.useRealTimers();
  });

  it('re-sends an allowlisted call once after a transient close, and only once', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    const client = createWSClient({
      WebSocketCtor: FakeCtor,
      bootstrap,
      retryOnTransientClose: [{ methodId: 42, why: 'test seam' }],
    });

    const p = client.callByID(42, ['arg']);
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    const id = first.sent.find((f) => f.type === 'rpc')!.id as string;

    first.triggerClose(1006);
    await vi.advanceTimersByTimeAsync(300);
    const second = MockWebSocket.instances[1]!;
    second.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);

    // Re-sent under the SAME id: request ids are client-side bookkeeping,
    // so the caller's promise is still the one waiting on this answer.
    const resent = second.sent.filter((f) => f.type === 'rpc');
    expect(resent).toHaveLength(1);
    expect(resent[0]!.id).toBe(id);

    // The one retry is spent. A second close settles the call rather
    // than parking it again — otherwise a flapping link would keep a
    // caller waiting indefinitely on a call it believes is in flight.
    second.triggerClose(1006, 'flapped again');
    await expect(p).rejects.toMatchObject({ closeReason: 'flapped again' });

    client.close();
    vi.useRealTimers();
  });

  it('records the hello frame and answers capability questions from it', async () => {
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });

    // Before any hello: every capability question answers false, so a
    // feature degrades rather than being attempted against a backend
    // that may not have it.
    expect(client.getHello()).toBeNull();
    expect(client.hasCapability('anything')).toBe(false);

    const p = client.callByID(1, []);
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    ws.pushFrame({
      type: 'hello',
      protocolVersion: 1,
      capabilities: ['demo.feature'],
      backendId: 'backend-uuid-1',
      serverTimeMs: Date.now(),
    });

    expect(client.getHello()?.backendId).toBe('backend-uuid-1');
    expect(client.getHello()?.protocolVersion).toBe(1);
    expect(client.hasCapability('demo.feature')).toBe(true);
    // An unrecognised name is false, never a guess from the version.
    expect(client.hasCapability('some.future.feature')).toBe(false);

    const id = ws.sent.find((f) => f.type === 'rpc')!.id as string;
    ws.pushFrame({ type: 'rpc', id, result: 'ok' });
    await expect(p).resolves.toBe('ok');
    client.close();
  });

  it('keeps the hello answer across a reconnect and republishes only on change', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    const seen: Array<string | null> = [];
    client.onHelloChange((hello) => seen.push(hello?.backendId ?? null));

    client.subscribe('thread:updated', () => {});
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    const hello = {
      type: 'hello',
      protocolVersion: 1,
      capabilities: ['demo.feature'],
      backendId: 'backend-uuid-1',
      serverTimeMs: Date.now(),
    };
    first.pushFrame(hello);

    first.triggerClose(1006);
    // The ladder is trying to reach the SAME backend, so the capability
    // answer must not flap to "unsupported" for the length of an outage.
    expect(client.hasCapability('demo.feature')).toBe(true);

    await vi.advanceTimersByTimeAsync(300);
    const second = MockWebSocket.instances[1]!;
    second.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    // Same backend, same answer but a fresh clock: waking every consumer
    // for a few milliseconds of skew would turn a routine reconnect into
    // a re-render.
    second.pushFrame({ ...hello, serverTimeMs: Date.now() + 5 });
    expect(seen).toEqual([null, 'backend-uuid-1']);

    // A different backend IS a change, and must be published.
    second.pushFrame({ ...hello, backendId: 'backend-uuid-2' });
    expect(seen).toEqual([null, 'backend-uuid-1', 'backend-uuid-2']);

    client.close();
    vi.useRealTimers();
  });

  it('accepts a hello whose fields are the wrong shape, falling back neutrally', async () => {
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    client.subscribe('thread:updated', () => {});
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    // A future backend may send shapes this build has never seen.
    // Half-understanding the frame beats rejecting it: refusing would
    // make an additive server change look like a backend with no
    // capabilities at all.
    ws.pushFrame({
      type: 'hello',
      protocolVersion: 'two',
      capabilities: [null, 'demo.feature', 7],
      backendId: 42,
      serverTimeMs: 'soon',
      unknownFutureField: { nested: true },
    });

    const hello = client.getHello()!;
    expect(hello.protocolVersion).toBe(0);
    expect(hello.capabilities).toEqual(['demo.feature']);
    expect(hello.backendId).toBe('');
    expect(hello.serverTimeMs).toBe(0);
    expect(hello.clockSkewMs).toBe(0);
    expect(client.hasCapability('demo.feature')).toBe(true);

    client.close();
  });

  // The future-dialect fixture (docs/specs/remote-access.md §9).
  //
  // The swap window — an old bundle live against a just-updated backend
  // for minutes — is a normal operating state. This drives a recorded
  // stream salted with everything the NEXT dialect could plausibly add
  // and asserts the client keeps working: unknown frame types, unknown
  // event channels, unknown fields on frames it does know, and shapes it
  // cannot address at all. Nothing may throw, nothing may reach the
  // error console, and no known-frame handling may be disturbed.
  it('runs normally against a future dialect, and reports zero errors', async () => {
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    const consoleWarn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    vi.spyOn(console, 'debug').mockImplementation(() => {});

    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    const known: unknown[] = [];
    const futureChannel: unknown[] = [];
    client.subscribe('thread:updated', (data) => known.push(data));
    client.subscribe('future:channel', (data) => futureChannel.push(data));

    const p = client.callByID(1, []);
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();
    const id = ws.sent.find((f) => f.type === 'rpc')!.id as string;

    // A hello carrying fields this build has never seen, alongside the
    // ones it has.
    ws.pushFrame({
      type: 'hello',
      protocolVersion: 2,
      capabilities: ['demo.feature'],
      backendId: 'backend-uuid-1',
      serverTimeMs: Date.now(),
      negotiatedCiphers: ['x'],
      leaseTtlMs: 30_000,
    });

    // Frame types from a dialect that does not exist yet.
    ws.pushFrame({ type: 'lease', scopes: ['threads:read'], ttlMs: 30_000 });
    ws.pushFrame({ type: 'push-token-request', nonce: 'abc' });
    ws.pushFrame({ type: 'hello-again', protocolVersion: 3 });

    // A known frame with unknown fields, and a known event on a channel
    // this build never registered.
    ws.pushFrame({
      type: 'event',
      channel: 'thread:updated',
      seq: 1,
      data: { id: 't1' },
      deviceId: 'device-9',
      originScope: 'threads:write',
    });
    ws.pushFrame({ type: 'event', channel: 'future:channel', seq: 1, data: { future: true } });
    ws.pushFrame({ type: 'event', channel: 'nobody:listens', seq: 1, data: null });

    // A batch mixing a known event with one on an unknown channel, plus
    // an entry shape this build cannot address at all. The known event
    // must still arrive — dropping a prefix or a suffix would leave the
    // seq cursor lying about what was delivered.
    ws.pushFrame({
      type: 'batch',
      events: [
        { channel: 'thread:updated', seq: 2, data: { id: 't2' }, replicaEpoch: 4 },
        { channel: 'future:channel', seq: 2, data: { future: true } },
        { channel: 42, seq: 'soon', data: null },
      ],
      coalescedFrom: 3,
    });

    // Shapes that are not frames at all.
    ws.pushRawText('not json at all');
    ws.pushFrame(null);
    ws.pushFrame([1, 2, 3]);
    ws.pushFrame({ id: 'no-type-field' });

    // The RPC still completes across all of it.
    ws.pushFrame({ type: 'rpc', id, result: 'ok' });
    await expect(p).resolves.toBe('ok');

    // Known traffic was delivered in full, including the batch entries
    // on either side of the unaddressable one.
    expect(known).toEqual([{ id: 't1' }, { id: 't2' }]);
    expect(futureChannel).toEqual([{ future: true }, { future: true }]);
    // Unknown fields did not disturb the hello this build understands.
    expect(client.getHello()?.backendId).toBe('backend-uuid-1');
    expect(client.hasCapability('demo.feature')).toBe(true);

    // Everything unaddressable was COUNTED, never thrown on.
    const stats = client.getUnknownInputStats();
    expect(stats.kinds).toMatchObject({
      lease: 1,
      'push-token-request': 1,
      'hello-again': 1,
      'event-shape': 1,
      unparseable: 1,
      'non-object': 2, // null and the array
      untyped: 1,
    });
    expect(stats.total).toBe(8);

    // Zero errors, and no per-frame warn spam either: a routine version
    // skew must not fill the console.
    expect(consoleError).not.toHaveBeenCalled();
    expect(consoleWarn).not.toHaveBeenCalled();

    // And the client is still usable afterwards — the point of the whole
    // exercise is that the swap window is survivable, not merely quiet.
    const after = client.callByID(2, []);
    await flushMicrotasks();
    const secondId = ws.sent.filter((f) => f.type === 'rpc')[1]!.id as string;
    ws.pushFrame({ type: 'rpc', id: secondId, result: 'still working' });
    await expect(after).resolves.toBe('still working');

    client.close();
  });

  it('keeps the replay cursor clean when a future dialect sends an event it cannot address', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    vi.spyOn(console, 'debug').mockImplementation(() => {});
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    client.subscribe('thread:updated', () => {});

    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);

    first.pushFrame({ type: 'event', channel: 'thread:updated', seq: 4, data: null });
    // Channel and seq of a shape this build cannot read. Before the
    // ingest check these landed in the cursor map as key "undefined" with
    // a NaN seq, which serialized into the next replay request as
    // {"undefined": null} — a map[string]uint64 decode failure that made
    // the server refuse the WHOLE replay, costing the client its gap
    // recovery for the rest of the session.
    first.pushFrame({ type: 'event', seq: 5, data: null });
    first.pushFrame({ type: 'event', channel: 'thread:updated', seq: 'later', data: null });

    first.triggerClose(1006);
    // Drain the reconnect timer (Math.random=0.5, attempt=0 -> 125ms).
    await vi.advanceTimersByTimeAsync(125);
    const second = MockWebSocket.instances[1]!;
    second.acceptOpen();
    await flushMicrotasks();

    const replay = second.sent.find((f) => f.type === 'replay')!;
    const cursors = replay.lastSeqByChannel as Record<string, unknown>;
    expect(cursors['thread:updated']).toBe(4);
    for (const [channel, seq] of Object.entries(cursors)) {
      expect(typeof channel).toBe('string');
      expect(channel).not.toBe('undefined');
      expect(Number.isSafeInteger(seq)).toBe(true);
    }

    client.close();
  });

  it('bounds the unknown-kind tally against a peer naming a new frame type every frame', async () => {
    vi.spyOn(console, 'debug').mockImplementation(() => {});
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    client.subscribe('thread:updated', () => {});
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    for (let i = 0; i < 200; i += 1) {
      ws.pushFrame({ type: `future-type-${i}` });
    }

    const stats = client.getUnknownInputStats();
    // Every frame counted, but the per-kind breakdown is capped so the
    // map cannot be grown without limit by the peer.
    expect(stats.total).toBe(200);
    expect(Object.keys(stats.kinds).length).toBeLessThanOrEqual(8);

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
      lastSeqByChannel: {
        'notification:activated': 0,
        'thread:updated': 7,
        'other:channel': 3,
      },
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

  // The server emits a gap marker whose seq sits BELOW our cursor when
  // our cursor sits above its head — the backend restarted and re-seeded
  // every channel from 1. Dedup must not eat it, or the cursor stays
  // stranded above the new head and every live event on that channel is
  // discarded forever.
  it('honours a gap marker whose seq is below the tracked cursor', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });

    const gaps: unknown[] = [];
    const channel: unknown[] = [];
    client.subscribe(transportGapChannel, (data) => gaps.push(data));
    client.subscribe('thread:updated', (data) => channel.push(data));
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    // Climb to seq 900 against the old backend.
    ws.pushFrame({ type: 'event', channel: 'thread:updated', seq: 900, data: 'old' });
    expect(channel).toEqual(['old']);

    // New backend: the replay answers our above-head cursor with a gap
    // marker at ITS head (seq 2), then live events resume from there.
    ws.pushFrame({ type: 'event', channel: 'thread:updated', seq: 2, data: null, gap: true });
    expect(gaps).toEqual([{ channel: 'thread:updated', seq: 2 }]);
    expect(warn).toHaveBeenCalled();

    ws.pushFrame({ type: 'event', channel: 'thread:updated', seq: 3, data: 'fresh' });
    expect(channel).toEqual(['old', null, 'fresh']);

    // The cursor was adopted downward, so the reconnect handshake asks
    // the new backend for events after ITS seq, not the stale 900.
    ws.triggerClose();
    await vi.waitFor(() => expect(MockWebSocket.instances).toHaveLength(2));
    const second = MockWebSocket.instances[1]!;
    second.acceptOpen();
    await flushMicrotasks();
    expect(second.sent[0]).toMatchObject({
      type: 'replay',
      lastSeqByChannel: { 'thread:updated': 3 },
    });

    client.close();
  });

  // A gap-flagged event that is genuinely stale still resyncs: gap is an
  // instruction, and the marker's seq is authoritative in both
  // directions. This is the same code path as above driven from the
  // opposite side, so a future "only reset downward" shortcut fails here.
  it('adopts the gap marker seq even when it moves the cursor forward', async () => {
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    client.subscribe('thread:updated', () => {});
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    ws.pushFrame({ type: 'event', channel: 'thread:updated', seq: 5, data: 'a' });
    ws.pushFrame({ type: 'event', channel: 'thread:updated', seq: 40, data: null, gap: true });
    ws.triggerClose();
    await vi.waitFor(() => expect(MockWebSocket.instances).toHaveLength(2));
    const second = MockWebSocket.instances[1]!;
    second.acceptOpen();
    await flushMicrotasks();
    expect(second.sent[0]).toMatchObject({
      type: 'replay',
      lastSeqByChannel: { 'thread:updated': 40 },
    });

    client.close();
  });

  // Mid-connection drop detection. The bus drops non-blockingly into a
  // full subscriber buffer and records nothing, so the forward seq skip
  // on the next event is the ONLY evidence — and for an edge-triggered
  // channel (one frame per state change) the only thing standing between
  // a dropped frame and permanently stale UI.
  it('fires transport:gap on a forward seq skip and still dispatches the event', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });

    const gaps: unknown[] = [];
    const seen: unknown[] = [];
    client.subscribe(transportGapChannel, (data) => gaps.push(data));
    client.subscribe('git:status', (data) => seen.push(data));
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    ws.pushFrame({ type: 'event', channel: 'git:status', seq: 4, data: { cwd: '/a' } });
    expect(gaps).toEqual([]);

    // seq 5 and 6 were dropped into a full subscriber buffer.
    ws.pushFrame({ type: 'event', channel: 'git:status', seq: 7, data: { cwd: '/b' } });

    expect(gaps).toEqual([{ channel: 'git:status', seq: 7 }]);
    expect(warn).toHaveBeenCalledWith(expect.stringContaining('dropped 2 event(s) on git:status'));
    // The carried event is real data, not a marker — it must be delivered.
    expect(seen).toEqual([{ cwd: '/a' }, { cwd: '/b' }]);

    // The cursor advanced to the event we did receive, so the next
    // contiguous one is not a second gap.
    ws.pushFrame({ type: 'event', channel: 'git:status', seq: 8, data: { cwd: '/c' } });
    expect(gaps).toHaveLength(1);
    expect(seen).toHaveLength(3);

    client.close();
  });

  it('does not fire transport:gap on contiguous seqs or a channel first event', async () => {
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });

    const gaps: unknown[] = [];
    client.subscribe(transportGapChannel, (data) => gaps.push(data));
    client.subscribe('mcp:status', () => {});
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    // First event of a channel has no cursor to skip past, whatever its
    // seq — the client joined mid-stream, it didn't miss anything.
    ws.pushFrame({ type: 'event', channel: 'mcp:status', seq: 91, data: { name: 'a' } });
    ws.pushFrame({ type: 'event', channel: 'mcp:status', seq: 92, data: { name: 'b' } });
    ws.pushFrame({ type: 'event', channel: 'mcp:status', seq: 93, data: { name: 'c' } });
    // A duplicate is dedup-dropped, and must not read as a backward gap.
    ws.pushFrame({ type: 'event', channel: 'mcp:status', seq: 93, data: { name: 'c' } });
    // Interleaving another channel doesn't confuse the per-channel cursor.
    ws.pushFrame({ type: 'event', channel: 'pr:updated', seq: 1, data: {} });
    ws.pushFrame({ type: 'event', channel: 'mcp:status', seq: 94, data: { name: 'd' } });

    expect(gaps).toEqual([]);

    client.close();
  });

  it('detects a forward seq skip inside a batch frame', async () => {
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });

    const gaps: unknown[] = [];
    const seen: unknown[] = [];
    client.subscribe(transportGapChannel, (data) => gaps.push(data));
    client.subscribe('pr:updated', (data) => seen.push(data));
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    ws.pushFrame({
      type: 'batch',
      events: [
        { channel: 'pr:updated', seq: 10, data: { v: 1 } },
        { channel: 'other:channel', seq: 1, data: {} },
        { channel: 'pr:updated', seq: 14, data: { v: 2 } },
      ],
    });

    expect(gaps).toEqual([{ channel: 'pr:updated', seq: 14 }]);
    expect(seen).toEqual([{ v: 1 }, { v: 2 }]);

    client.close();
  });

  it('does not double-fire the synthetic gap for an explicit gap:true frame', async () => {
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });

    const gaps: unknown[] = [];
    client.subscribe(transportGapChannel, (data) => gaps.push(data));
    client.subscribe('thread:updated', () => {});
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    ws.pushFrame({ type: 'event', channel: 'thread:updated', seq: 3, data: 'a' });
    // A server gap marker skips far forward. The explicit path already
    // dispatched for it; the skip check must not fire a second time.
    ws.pushFrame({ type: 'event', channel: 'thread:updated', seq: 30, data: null, gap: true });

    expect(gaps).toEqual([{ channel: 'thread:updated', seq: 30 }]);

    // The marker adopted the cursor, so the next live event is contiguous
    // and the detection is armed again from there.
    ws.pushFrame({ type: 'event', channel: 'thread:updated', seq: 31, data: 'b' });
    expect(gaps).toHaveLength(1);

    client.close();
  });

  // Across a reconnect a forward skip is not evidence of a drop: Replay
  // answers an ephemeral channel with nothing and a latest-only channel
  // with just its newest frame, both by design. Judging those against the
  // carried-over cursor would resync spuriously on every reconnect.
  it('does not treat a post-reconnect seq jump as a mid-connection drop', async () => {
    vi.useFakeTimers();
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });

    const gaps: unknown[] = [];
    client.subscribe(transportGapChannel, (data) => gaps.push(data));
    client.subscribe('system:stats', () => {});
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    first.pushFrame({ type: 'event', channel: 'system:stats', seq: 5, data: { cpu: 1 } });
    expect(gaps).toEqual([]);

    first.triggerClose();
    await vi.advanceTimersByTimeAsync(125);
    const second = MockWebSocket.instances[1]!;
    second.acceptOpen();
    await flushMicrotasks();

    // Samples 6..80 were emitted while we were away; the capacity-1 ring
    // replays only the newest, with no gap marker.
    second.pushFrame({ type: 'event', channel: 'system:stats', seq: 81, data: { cpu: 2 } });
    expect(gaps).toEqual([]);

    // Detection re-arms from the first event observed on THIS connection.
    second.pushFrame({ type: 'event', channel: 'system:stats', seq: 90, data: { cpu: 3 } });
    expect(gaps).toEqual([{ channel: 'system:stats', seq: 90 }]);

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

  it('wraps a malformed-bootstrap failure as a transport error and keeps the cause', async () => {
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
    // The caller must see a TRANSPORT-class failure, not the raw
    // SyntaxError: a manifest that would not parse means the request was
    // never sent, and a caller with side effects classifies on
    // DisconnectedError to decide whether "nothing happened" is safe to
    // assume. The original error survives as `cause` so triage keeps it.
    expect(caught).toBeInstanceOf(DisconnectedError);
    expect((caught as DisconnectedError).cause).toBeInstanceOf(SyntaxError);
    expect((caught as DisconnectedError).message).toContain('Unexpected token');
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
    (globalThis as { __AO_BOOTSTRAP__?: { wsUrl: string; token: string; remote?: boolean } }).__AO_BOOTSTRAP__ = {
      wsUrl: 'ws://injected/',
      token: 'inj',
      remote: true,
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
      expect(isViewOnlySession()).toBe(true);

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

  it('publishes fetched bootstrap locality to the view-only helper', async () => {
    const fetchMock = vi.fn(async () => ({
      ok: true,
      status: 200,
      headers: new Headers({ 'content-type': 'application/json' }),
      json: async () => ({ wsUrl: `ws://${window.location.host}/ws`, token: 'abc123', remote: true }),
    }));
    vi.stubGlobal('fetch', fetchMock);

    const client = createWSClient({ WebSocketCtor: FakeCtor });
    try {
      const unsubscribe = client.subscribe('workflow:item-state', () => {});
      await vi.waitFor(() => expect(MockWebSocket.instances).toHaveLength(1));
      expect(isViewOnlySession()).toBe(true);
      unsubscribe();
    } finally {
      client.close();
      vi.unstubAllGlobals();
    }
  });

  it('stashes the URL token and falls back to it once the URL is scrubbed', async () => {
    // First load carries ?t=; defaultBootstrap must scrub it from the
    // URL, stash it in sessionStorage, and serve a tokenless "reload"
    // (second client, scrubbed URL) from the stash.
    window.history.replaceState(null, '', '/?t=abc123');
    const fetchMock = vi.fn(async () => ({
      ok: true,
      status: 200,
      headers: new Headers({ 'content-type': 'application/json' }),
      json: async () => ({ wsUrl: `ws://${window.location.host}/ws`, token: 'abc123' }),
    }));
    vi.stubGlobal('fetch', fetchMock);

    try {
      const first = createWSClient({ WebSocketCtor: FakeCtor });
      void first.callByID(1, []).catch(() => {});
      await flushMicrotasks();
      expect(fetchMock).toHaveBeenCalledWith('/bootstrap.json?t=abc123', expect.anything());
      expect(window.sessionStorage.getItem('ao:bootstrap-token')).toBe('abc123');
      expect(window.location.search).toBe('');
      first.close();

      const second = createWSClient({ WebSocketCtor: FakeCtor });
      void second.callByID(1, []).catch(() => {});
      await flushMicrotasks();
      expect(fetchMock).toHaveBeenLastCalledWith('/bootstrap.json?t=abc123', expect.anything());
      second.close();
    } finally {
      window.sessionStorage.clear();
      window.history.replaceState(null, '', '/');
      vi.unstubAllGlobals();
    }
  });

  it('marks the post-invalidation refetch as a revalidation', async () => {
    // An injected (--connect) manifest short-circuits an ordinary fetch,
    // so the fetcher must be told when the refetch exists to observe a
    // suspect credential (defaultBootstrap then routes it through the
    // stub's /bootstrap.json probe). The flag arms on invalidation and
    // stands down once a fetch resolves.
    vi.useFakeTimers();
    try {
      const calls: Array<boolean | undefined> = [];
      const client = createWSClient({
        WebSocketCtor: FakeCtor,
        bootstrap: async (opts?: { revalidate?: boolean }) => {
          calls.push(opts?.revalidate);
          return { wsUrl: 'ws://example/ws', token: 't', remote: true };
        },
      });
      client.subscribe('x', () => {});
      await vi.advanceTimersByTimeAsync(0);
      expect(calls).toEqual([false]);

      // Two consecutive pre-open deaths trip the cache invalidation; the
      // refetch that follows must carry the revalidation mark, and the
      // pre-invalidation attempts must not have fetched at all (cache).
      for (let i = 0; i < BOOTSTRAP_INVALIDATE_AFTER_FAILURES; i++) {
        MockWebSocket.instances.at(-1)!.triggerClose();
        await vi.advanceTimersByTimeAsync(RECONNECT_MAX_REMOTE_MS);
      }
      expect(calls).toEqual([false, true]);
      MockWebSocket.instances.at(-1)!.acceptOpen();
      await vi.advanceTimersByTimeAsync(0);
      client.close();
    } finally {
      vi.useRealTimers();
    }
  });

  it('keeps a stashed token the server refuses', async () => {
    // Re-presenting a stale token costs the identical 404, so clearing
    // buys nothing — and it would destroy the one copy that lets a page
    // reload recover from a refusal that wasn't real (a proxy blip
    // answering 404 for a token the server still honours).
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    window.sessionStorage.setItem('ao:bootstrap-token', 'stale');
    const fetchMock = vi.fn(async () => ({ ok: false, status: 404 }));
    vi.stubGlobal('fetch', fetchMock);

    try {
      const client = createWSClient({ WebSocketCtor: FakeCtor });
      let caught: unknown;
      try {
        await client.callByID(1, []);
      } catch (err) {
        caught = err;
      }
      expect((caught as Error).message).toMatch(/HTTP 404/);
      expect(fetchMock).toHaveBeenCalledWith('/bootstrap.json?t=stale', expect.anything());
      expect(window.sessionStorage.getItem('ao:bootstrap-token')).toBe('stale');
      client.close();
    } finally {
      window.sessionStorage.clear();
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
    await vi.advanceTimersByTimeAsync(RPC_TIMEOUT_MS + 1);
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
      lastSeqByChannel: { 'notification:activated': 0, unsubscribed: 9 },
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

  it('an RPC dispatched during a queued backoff fires the attempt immediately', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);

    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);

    // Escalate the ladder so the queued delay (500ms) clearly exceeds
    // the collapse floor: close → attempt (125ms) → close → attempt
    // (250ms) → close, leaving a 500ms backoff queued.
    first.triggerClose();
    await vi.advanceTimersByTimeAsync(130);
    MockWebSocket.instances[1]!.triggerClose();
    await vi.advanceTimersByTimeAsync(260);
    MockWebSocket.instances[2]!.triggerClose();
    expect(MockWebSocket.instances).toHaveLength(3);

    // Sit out the rate floor (the last attempt just started), then
    // dispatch an RPC — the demand collapse must start the attempt
    // right away instead of waiting out the remaining ~240ms.
    await vi.advanceTimersByTimeAsync(RECONNECT_INITIAL_MS + 10);
    expect(MockWebSocket.instances).toHaveLength(3);
    const p = client.callByID(9, ['now']);
    await vi.advanceTimersByTimeAsync(0);
    expect(MockWebSocket.instances).toHaveLength(4);

    // The RPC rides the accelerated attempt: same scheduled
    // connectPromise settles on open and the frame goes out.
    const fourth = MockWebSocket.instances[3]!;
    fourth.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    const rpcFrame = fourth.sent.find((f) => f.type === 'rpc')!;
    expect(rpcFrame).toMatchObject({ methodId: 9, params: ['now'] });
    fourth.pushFrame({ type: 'rpc', id: rpcFrame.id, result: 'ok' });
    await expect(p).resolves.toBe('ok');

    // The original backoff timer was cancelled — advancing past it must
    // not mint a fifth socket.
    await vi.advanceTimersByTimeAsync(1_000);
    expect(MockWebSocket.instances).toHaveLength(4);

    client.close();
  });

  it('rate-floors the RPC demand collapse so background RPCs cannot defeat backoff', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);

    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);

    first.triggerClose();
    // The failed attempt just started; an RPC arriving inside the
    // RECONNECT_INITIAL_MS floor (e.g. the diagnostics flush reacting
    // to the disconnect) must NOT fire the queued attempt early.
    const p = client.callByID(9, ['later']).catch(() => {});
    await vi.advanceTimersByTimeAsync(0);
    expect(MockWebSocket.instances).toHaveLength(1);

    // The queued backoff still runs on its own schedule and the RPC
    // rides that attempt.
    await vi.advanceTimersByTimeAsync(130);
    expect(MockWebSocket.instances).toHaveLength(2);
    client.close();
    await p;
  });

  it('page resume fires the queued attempt immediately and resets the ladder', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);

    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);

    // Escalate the ladder with two failed visible-session attempts:
    // close -> attempt (125ms) -> close -> attempt (250ms) -> close.
    first.triggerClose();
    await vi.advanceTimersByTimeAsync(130);
    MockWebSocket.instances[1]!.triggerClose();
    await vi.advanceTimersByTimeAsync(260);
    MockWebSocket.instances[2]!.triggerClose();
    expect(MockWebSocket.instances).toHaveLength(3);

    // Third backoff is now queued at base 250*2^2=1000 -> 500ms with
    // random=0.5. A resume signal must not wait for it.
    document.dispatchEvent(new Event('visibilitychange'));
    await vi.advanceTimersByTimeAsync(0);
    expect(MockWebSocket.instances).toHaveLength(4);

    // The reset also applies to the NEXT failure: dropping the resumed
    // attempt schedules at the initial delay again (125ms), not the
    // escalated one.
    MockWebSocket.instances[3]!.triggerClose();
    await vi.advanceTimersByTimeAsync(130);
    expect(MockWebSocket.instances).toHaveLength(5);

    client.close();
  });

  it('a resume signal while the page is still hidden does not fire the queued attempt', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.999);

    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);

    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      get: () => 'hidden',
    });
    try {
      first.triggerClose();
      // A visibilitychange that lands while STILL hidden (minimise, tab
      // switch away) is not a resume — the queued backoff must keep its
      // schedule instead of firing early.
      document.dispatchEvent(new Event('visibilitychange'));
      await vi.advanceTimersByTimeAsync(0);
      expect(MockWebSocket.instances).toHaveLength(1);
      // The ladder keeps escalating normally while hidden; the queued
      // attempt fires on its own schedule (249ms with random=0.999).
      await vi.advanceTimersByTimeAsync(250);
      expect(MockWebSocket.instances).toHaveLength(2);
    } finally {
      delete (document as { visibilityState?: unknown }).visibilityState;
    }

    client.close();
  });

  it('triggerReconnect settles the scheduled connectPromise for queued awaiters', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);

    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);

    first.triggerClose();
    // Queue a real awaiter behind the backoff: the RPC lands inside the
    // collapse rate floor, so it parks on the scheduled connectPromise.
    const p = client.callByID(11, ['queued']);
    await vi.advanceTimersByTimeAsync(0);
    expect(MockWebSocket.instances).toHaveLength(1);

    client.triggerReconnect();
    await vi.advanceTimersByTimeAsync(0);
    expect(MockWebSocket.instances).toHaveLength(2);
    const second = MockWebSocket.instances[1]!;
    second.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);

    // The retried attempt is the scheduled one — its replay frame went
    // out on the new socket, and the queued RPC rode it instead of
    // hanging on an abandoned promise.
    expect(second.sent[0]).toMatchObject({ type: 'replay' });
    const rpcFrame = second.sent.find((f) => f.type === 'rpc')!;
    expect(rpcFrame).toMatchObject({ methodId: 11, params: ['queued'] });
    second.pushFrame({ type: 'rpc', id: rpcFrame.id, result: 'rode-retry' });
    await expect(p).resolves.toBe('rode-retry');

    // The cancelled backoff timer must not produce a third socket later.
    await vi.advanceTimersByTimeAsync(1_000);
    expect(MockWebSocket.instances).toHaveLength(2);

    client.close();
  });

  it('triggerReconnect no-ops while a connect attempt is already in flight', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);

    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);

    first.triggerClose();
    // Let the backoff fire so an attempt is mid-flight (socket #2 is
    // CONNECTING, no backoff queued).
    await vi.advanceTimersByTimeAsync(130);
    expect(MockWebSocket.instances).toHaveLength(2);

    // Retry clicked while the attempt runs: racing a second connect
    // would mint a parallel socket and orphan this one.
    client.triggerReconnect();
    await vi.advanceTimersByTimeAsync(0);
    expect(MockWebSocket.instances).toHaveLength(2);

    MockWebSocket.instances[1]!.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    expect(client.getStatus().status).toBe('connected');

    client.close();
  });

  it('force-closes a stale socket once the server has proven it heartbeats', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    vi.spyOn(console, 'warn').mockImplementation(() => {});

    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    const diagnostics: string[] = [];
    client.setDiagnosticsSink((m) => diagnostics.push(m));
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);

    // One heartbeat arms the watchdog; then the socket goes silent
    // (half-open: no close event will ever fire on its own).
    first.pushFrame({ type: 'ping' });
    await vi.advanceTimersByTimeAsync(STALE_TRAFFIC_THRESHOLD_MS + 10_000);
    // Drain the post-close backoff so the reconnect attempt fires.
    await vi.advanceTimersByTimeAsync(1_000);

    // The watchdog force-closed the socket and the reconnect path took
    // over: a second socket exists after the (collapsed-cap) backoff.
    expect(first.readyState).toBe(3);
    expect(MockWebSocket.instances.length).toBeGreaterThanOrEqual(2);
    expect(diagnostics.some((m) => m.includes('no traffic'))).toBe(true);

    client.close();
  });

  it('defers stale-socket verdicts while hidden and starts a fresh window on resume', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    vi.spyOn(console, 'warn').mockImplementation(() => {});

    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    first.pushFrame({ type: 'ping' });

    let visibilityState: DocumentVisibilityState = 'hidden';
    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      get: () => visibilityState,
    });
    try {
      document.dispatchEvent(new Event('visibilitychange'));
      await vi.advanceTimersByTimeAsync(STALE_TRAFFIC_THRESHOLD_MS * 3);
      expect(first.readyState).toBe(1);
      expect(MockWebSocket.instances).toHaveLength(1);

      visibilityState = 'visible';
      document.dispatchEvent(new Event('visibilitychange'));
      await vi.advanceTimersByTimeAsync(STALE_TRAFFIC_THRESHOLD_MS);
      expect(first.readyState).toBe(1);

      await vi.advanceTimersByTimeAsync(STALE_CHECK_INTERVAL_MS);
      expect(first.readyState).toBe(3);
    } finally {
      delete (document as { visibilityState?: unknown }).visibilityState;
      client.close();
    }
  });

  it('keeps a socket alive while heartbeats keep arriving', async () => {
    vi.useFakeTimers();
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);

    for (let i = 0; i < 6; i++) {
      first.pushFrame({ type: 'ping' });
      await vi.advanceTimersByTimeAsync(20_000);
    }
    expect(first.readyState).toBe(1);
    expect(MockWebSocket.instances).toHaveLength(1);

    client.close();
  });

  it('never force-closes when the server has not sent a heartbeat (version skew guard)', async () => {
    vi.useFakeTimers();
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);

    // A heartbeat-less (older) server with an idle-but-healthy socket
    // must not get reconnect-looped by the watchdog.
    await vi.advanceTimersByTimeAsync(STALE_TRAFFIC_THRESHOLD_MS * 3);
    expect(first.readyState).toBe(1);
    expect(MockWebSocket.instances).toHaveLength(1);

    client.close();
  });

  it('resets the heartbeat proof per connection and re-arms on the next ping', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    vi.spyOn(console, 'warn').mockImplementation(() => {});

    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);

    // Socket #1 proves heartbeats, goes silent, and is force-closed.
    first.pushFrame({ type: 'ping' });
    await vi.advanceTimersByTimeAsync(STALE_TRAFFIC_THRESHOLD_MS + STALE_CHECK_INTERVAL_MS);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(first.readyState).toBe(3);
    const second = MockWebSocket.instances.at(-1)!;
    expect(second).not.toBe(first);
    second.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);

    // Socket #2 has NOT proven heartbeats — the proof is per
    // connection, so a rolled-back (heartbeat-less) backend can't get
    // reconnect-looped by evidence from the previous socket.
    await vi.advanceTimersByTimeAsync(STALE_TRAFFIC_THRESHOLD_MS * 3);
    expect(second.readyState).toBe(1);

    // And the watchdog re-arms cleanly: one ping on socket #2, silence,
    // force-close again.
    second.pushFrame({ type: 'ping' });
    await vi.advanceTimersByTimeAsync(STALE_TRAFFIC_THRESHOLD_MS + STALE_CHECK_INTERVAL_MS);
    expect(second.readyState).toBe(3);

    client.close();
  });

  it('does not force-close a remote socket while RPCs are in flight (mid-transfer guard)', async () => {
    vi.useFakeTimers();
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    vi.spyOn(Math, 'random').mockReturnValue(0.5);

    const client = createWSClient({
      WebSocketCtor: FakeCtor,
      bootstrap: async () => ({ wsUrl: 'ws://example/ws', token: 't', remote: true }),
    });
    const p = client.callByID(5, []);
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    first.pushFrame({ type: 'ping' });

    // Silence past the threshold with the RPC still pending: a single
    // huge response frame can legitimately hold the wire that long on a
    // remote link, so the watchdog must stand down and let the RPC
    // timeout arbitrate.
    await vi.advanceTimersByTimeAsync(STALE_TRAFFIC_THRESHOLD_MS + STALE_CHECK_INTERVAL_MS);
    expect(first.readyState).toBe(1);

    // Once the response lands (no pending RPCs), renewed silence
    // force-closes as usual.
    const rpcFrame = first.sent.find((f) => f.type === 'rpc')!;
    first.pushFrame({ type: 'rpc', id: rpcFrame.id, result: 'ok' });
    await expect(p).resolves.toBe('ok');
    await vi.advanceTimersByTimeAsync(STALE_TRAFFIC_THRESHOLD_MS + STALE_CHECK_INTERVAL_MS);
    expect(first.readyState).toBe(3);

    client.close();
  });

  it('a superseded socket closing late cannot clobber the live connection', async () => {
    vi.useFakeTimers();
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    vi.spyOn(Math, 'random').mockReturnValue(0.5);

    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    first.pushFrame({ type: 'ping' });

    // Simulate the real browser CLOSING window: close() moves the
    // socket out of OPEN but the close event arrives later.
    first.close = () => {
      first.readyState = 2; // CLOSING, no event yet
    };
    await vi.advanceTimersByTimeAsync(STALE_TRAFFIC_THRESHOLD_MS + STALE_CHECK_INTERVAL_MS);
    expect(first.readyState).toBe(2);

    // Fresh demand mints a successor while #1 is still CLOSING.
    const p = client.callByID(3, []);
    await vi.advanceTimersByTimeAsync(0);
    expect(MockWebSocket.instances).toHaveLength(2);
    const second = MockWebSocket.instances[1]!;
    second.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    expect(client.getStatus().status).toBe('connected');

    // Socket #1's close event finally lands. It must not null the live
    // socket, fail #2's pending RPCs, or schedule a reconnect.
    first.triggerClose();
    await vi.advanceTimersByTimeAsync(0);
    expect(client.getStatus().status).toBe('connected');

    const rpcFrame = second.sent.find((f) => f.type === 'rpc')!;
    second.pushFrame({ type: 'rpc', id: rpcFrame.id, result: 'alive' });
    await expect(p).resolves.toBe('alive');

    // No reconnect was scheduled by the stale close.
    await vi.advanceTimersByTimeAsync(5_000);
    expect(MockWebSocket.instances).toHaveLength(2);

    client.close();
  });

  it('caps backoff at the local ceiling for a same-machine backend', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.999);

    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    MockWebSocket.instances[0]!.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    MockWebSocket.instances[0]!.triggerClose();

    // Fail enough visible attempts that an uncapped ladder would sit
    // far above the local ceiling (250 * 2^6 = 16s).
    for (let i = 0; i < 6; i++) {
      const before = MockWebSocket.instances.length;
      await vi.advanceTimersByTimeAsync(RECONNECT_MAX_REMOTE_MS);
      expect(MockWebSocket.instances.length).toBe(before + 1);
      MockWebSocket.instances.at(-1)!.triggerClose();
    }

    const snap = client.getStatus();
    expect(snap.status).toBe('reconnecting');
    expect(snap.nextAttemptAt).not.toBeNull();
    expect(snap.nextAttemptAt! - Date.now()).toBeLessThanOrEqual(RECONNECT_MAX_LOCAL_MS);
    // Prove escalation actually happened before hitting the cap.
    expect(snap.nextAttemptAt! - Date.now()).toBeGreaterThan(RECONNECT_MAX_LOCAL_MS / 2);

    client.close();
  });

  it('keeps the remote ceiling for a remote backend, across bootstrap invalidation', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.999);

    const client = createWSClient({
      WebSocketCtor: FakeCtor,
      bootstrap: async () => ({ wsUrl: 'ws://example/ws', token: 't', remote: true }),
    });
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    MockWebSocket.instances[0]!.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    MockWebSocket.instances[0]!.triggerClose();

    // 6 pre-open failures also trip the bootstrap-cache invalidation;
    // the sticky remoteBackend flag must keep the remote ceiling anyway.
    for (let i = 0; i < 6; i++) {
      const before = MockWebSocket.instances.length;
      await vi.advanceTimersByTimeAsync(RECONNECT_MAX_REMOTE_MS);
      expect(MockWebSocket.instances.length).toBe(before + 1);
      MockWebSocket.instances.at(-1)!.triggerClose();
    }

    const snap = client.getStatus();
    expect(snap.nextAttemptAt).not.toBeNull();
    expect(snap.nextAttemptAt! - Date.now()).toBeGreaterThan(RECONNECT_MAX_LOCAL_MS);

    client.close();
  });

  it('refetches bootstrap after two consecutive close-before-open failures', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);

    const fetchSpy = vi
      .fn<() => Promise<{ wsUrl: string; token: string }>>()
      .mockResolvedValue({ wsUrl: 'ws://example/ws', token: 't' });
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap: fetchSpy });
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    MockWebSocket.instances[0]!.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchSpy).toHaveBeenCalledTimes(1);

    // Simulate a restarted backend refusing the stale token: every new
    // socket dies before open. Attempts 1 and 2 reuse the cached
    // bootstrap; the failure threshold then drops the cache so attempt
    // 3 refetches.
    MockWebSocket.instances[0]!.triggerClose();
    for (let i = 0; i < 2; i++) {
      const before = MockWebSocket.instances.length;
      await vi.advanceTimersByTimeAsync(RECONNECT_MAX_REMOTE_MS);
      expect(MockWebSocket.instances.length).toBe(before + 1);
      MockWebSocket.instances.at(-1)!.triggerClose();
    }
    expect(fetchSpy).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(RECONNECT_MAX_REMOTE_MS);
    expect(fetchSpy).toHaveBeenCalledTimes(2);

    // The counter reset at invalidation: attempt 3's failure is failure
    // #1 of the next window, so attempt 4 still uses the cache…
    MockWebSocket.instances.at(-1)!.triggerClose();
    await vi.advanceTimersByTimeAsync(RECONNECT_MAX_REMOTE_MS);
    expect(fetchSpy).toHaveBeenCalledTimes(2);
    // …and the second pair of failures invalidates again: attempt 5
    // refetches. Cadence is every 2 failures, not every failure.
    MockWebSocket.instances.at(-1)!.triggerClose();
    await vi.advanceTimersByTimeAsync(RECONNECT_MAX_REMOTE_MS);
    expect(fetchSpy).toHaveBeenCalledTimes(3);

    client.close();
  });

  // A backend that accepts the handshake and then dies is the storm the
  // ladder exists for: resetting on `open` pinned every retry at the
  // jitter floor (~50ms), i.e. ~10 connects/second for as long as the
  // condition lasted.
  it('does not reset the backoff for a socket that dies right after opening', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.999);

    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);

    for (let i = 0; i < 5; i++) {
      const ws = MockWebSocket.instances.at(-1)!;
      ws.acceptOpen();
      await vi.advanceTimersByTimeAsync(0);
      ws.triggerClose();
      await vi.advanceTimersByTimeAsync(RECONNECT_MAX_LOCAL_MS);
    }

    const stormed = MockWebSocket.instances.at(-1)!;
    stormed.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    // Just short of the stability window: still not a healthy backend.
    await vi.advanceTimersByTimeAsync(BACKOFF_RESET_AFTER_MS - 1_000);
    stormed.triggerClose();

    const snap = client.getStatus();
    expect(snap.status).toBe('reconnecting');
    expect(snap.nextAttemptAt).not.toBeNull();
    expect(snap.nextAttemptAt! - Date.now()).toBeGreaterThan(RECONNECT_MAX_LOCAL_MS / 2);

    client.close();
  });

  it('resets the backoff after a connection that stayed up past the stability window', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.999);

    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);

    // Climb the ladder on sockets that never open.
    MockWebSocket.instances[0]!.triggerClose();
    for (let i = 0; i < 4; i++) {
      await vi.advanceTimersByTimeAsync(RECONNECT_MAX_LOCAL_MS);
      MockWebSocket.instances.at(-1)!.triggerClose();
    }
    expect(client.getStatus().nextAttemptAt! - Date.now())
      .toBeGreaterThan(RECONNECT_MAX_LOCAL_MS / 2);

    // One connection that actually served, then dropped.
    await vi.advanceTimersByTimeAsync(RECONNECT_MAX_LOCAL_MS);
    const stable = MockWebSocket.instances.at(-1)!;
    stable.acceptOpen();
    await vi.advanceTimersByTimeAsync(BACKOFF_RESET_AFTER_MS);
    stable.triggerClose();

    const snap = client.getStatus();
    expect(snap.nextAttemptAt).not.toBeNull();
    expect(snap.nextAttemptAt! - Date.now()).toBeLessThanOrEqual(RECONNECT_INITIAL_MS);

    client.close();
  });

  // A restarted backend answers the stale token with a refusal, not a
  // dead socket: the loop would otherwise show "Reconnecting…" forever
  // while every attempt is structurally doomed. On a session that cannot
  // mint a new token the refusal is TERMINAL — the ladder stops.
  it('latches a terminal unauthorized state on a refused credential and stops retrying', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    vi.spyOn(console, 'warn').mockImplementation(() => {});

    const fetchSpy = vi
      .fn<() => Promise<{ wsUrl: string; token: string }>>()
      // Transient first (server up, manifest gated) — that one must keep
      // the ordinary loop — then the refusal.
      .mockRejectedValueOnce(new Error('bootstrap fetch failed: HTTP 503'))
      .mockRejectedValue(new BootstrapRejectedError(404));

    const client = createWSClient({
      WebSocketCtor: FakeCtor,
      bootstrap: fetchSpy,
      // Stale bookmarked share link: this session never got a manifest,
      // so the non-loopback document origin is the only locality signal.
      loopbackOrigin: () => false,
    });
    client.subscribe('x', () => {});

    await vi.advanceTimersByTimeAsync(0);
    expect(fetchSpy).toHaveBeenCalledTimes(1);
    expect(client.getStatus().status).toBe('reconnecting');

    // attempt=0 backoff is 250 * 0.5 = 125ms.
    await vi.advanceTimersByTimeAsync(150);
    expect(fetchSpy).toHaveBeenCalledTimes(2);
    expect(client.getStatus()).toEqual({ status: 'unauthorized', nextAttemptAt: null });

    // Terminal: minutes of wall clock produce no further attempt and no
    // socket. This is the whole point — a phone on the couch must not
    // keep dialling a server that will refuse it identically forever.
    await vi.advanceTimersByTimeAsync(5 * 60_000);
    expect(fetchSpy).toHaveBeenCalledTimes(2);
    expect(MockWebSocket.instances).toHaveLength(0);
    expect(client.getStatus().status).toBe('unauthorized');

    client.close();
  });

  // Passive demand must not defeat the stop: an RPC issued while
  // latched is refused locally rather than turning the dead ladder into
  // one bootstrap fetch per caller.
  it('refuses RPCs without a network fetch while the credential is dead', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    vi.spyOn(console, 'warn').mockImplementation(() => {});

    const fetchSpy = vi
      .fn<() => Promise<{ wsUrl: string; token: string }>>()
      .mockRejectedValue(new BootstrapRejectedError(401));

    const client = createWSClient({
      WebSocketCtor: FakeCtor,
      bootstrap: fetchSpy,
      loopbackOrigin: () => false,
    });
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    expect(client.getStatus().status).toBe('unauthorized');
    expect(fetchSpy).toHaveBeenCalledTimes(1);

    const call = client.callByName('App.Anything', []);
    await expect(call).rejects.toBeInstanceOf(DisconnectedError);
    expect(fetchSpy).toHaveBeenCalledTimes(1);

    client.close();
  });

  // Transitions on the latch itself: a manual Retry un-latches (one
  // attempt, user-initiated), a second refusal re-latches, and a served
  // manifest recovers. State coverage is not transition coverage.
  it('un-latches on manual retry and re-latches when the refusal repeats', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    vi.spyOn(console, 'warn').mockImplementation(() => {});

    const fetchSpy = vi
      .fn<() => Promise<{ wsUrl: string; token: string }>>()
      .mockRejectedValueOnce(new BootstrapRejectedError(404))
      .mockRejectedValueOnce(new BootstrapRejectedError(404))
      .mockResolvedValue({ wsUrl: 'ws://example/ws', token: 'fresh' });

    const client = createWSClient({
      WebSocketCtor: FakeCtor,
      bootstrap: fetchSpy,
      loopbackOrigin: () => false,
    });
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    expect(client.getStatus().status).toBe('unauthorized');

    // on -> off -> on: retry attempts exactly once and re-latches.
    client.triggerReconnect();
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchSpy).toHaveBeenCalledTimes(2);
    expect(client.getStatus().status).toBe('unauthorized');
    await vi.advanceTimersByTimeAsync(60_000);
    expect(fetchSpy).toHaveBeenCalledTimes(2);

    // on -> off, this time with a server willing to serve us.
    client.triggerReconnect();
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchSpy).toHaveBeenCalledTimes(3);
    expect(client.getStatus().status).toBe('reconnecting');
    MockWebSocket.instances.at(-1)!.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    expect(client.getStatus().status).toBe('connected');

    client.close();
  });

  // Transition the other way: a healthy remote session whose backend
  // restarts must ENTER the terminal state, not stay in 'reconnecting'.
  it('enters the terminal state when a live remote session\'s backend restarts', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    vi.spyOn(console, 'warn').mockImplementation(() => {});

    const fetchSpy = vi
      .fn<() => Promise<{ wsUrl: string; token: string; remote?: boolean }>>()
      // `remote: true` is the server's own pre-upgrade verdict — this
      // session's locality is known from the manifest, not the origin.
      .mockResolvedValueOnce({ wsUrl: 'ws://example/ws', token: 't', remote: true })
      .mockRejectedValue(new BootstrapRejectedError(404));

    const client = createWSClient({
      WebSocketCtor: FakeCtor,
      bootstrap: fetchSpy,
      loopbackOrigin: () => true,
    });
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    MockWebSocket.instances[0]!.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    expect(client.getStatus().status).toBe('connected');

    // Backend restarts: the socket dies, and every attempt from here
    // dies before open until the cached bootstrap is invalidated and
    // the refetch surfaces the refusal.
    MockWebSocket.instances[0]!.triggerClose();
    expect(client.getStatus().status).toBe('reconnecting');
    for (let i = 0; i < BOOTSTRAP_INVALIDATE_AFTER_FAILURES; i++) {
      await vi.advanceTimersByTimeAsync(RECONNECT_MAX_REMOTE_MS);
      MockWebSocket.instances.at(-1)!.triggerClose();
    }
    await vi.advanceTimersByTimeAsync(RECONNECT_MAX_REMOTE_MS);
    // The refetch happened (bootstrap-cache invalidation) and the
    // refusal it surfaced is what the user now sees. Later attempts
    // cascade inside this window — the count is a floor, not a total.
    expect(fetchSpy.mock.calls.length).toBeGreaterThanOrEqual(2);
    expect(client.getStatus()).toEqual({ status: 'unauthorized', nextAttemptAt: null });

    const socketsAtLatch = MockWebSocket.instances.length;
    const fetchesAtLatch = fetchSpy.mock.calls.length;
    await vi.advanceTimersByTimeAsync(5 * 60_000);
    expect(MockWebSocket.instances).toHaveLength(socketsAtLatch);
    expect(fetchSpy.mock.calls.length).toBe(fetchesAtLatch);

    client.close();
  });

  // Requirement the other side of the gate protects: the embedded
  // webview loads from loopback and is handed a live token by the shell
  // that owns the backend. It must never be told to "reopen the share
  // link" — there is no share link — so a refusal there stays an
  // ordinary retry.
  it('never latches for a loopback session', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    vi.spyOn(console, 'warn').mockImplementation(() => {});

    const fetchSpy = vi
      .fn<() => Promise<{ wsUrl: string; token: string }>>()
      .mockRejectedValueOnce(new BootstrapRejectedError(404))
      .mockRejectedValueOnce(new BootstrapRejectedError(404))
      .mockResolvedValue({ wsUrl: 'ws://example/ws', token: 't' });

    const client = createWSClient({
      WebSocketCtor: FakeCtor,
      bootstrap: fetchSpy,
      loopbackOrigin: () => true,
    });
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    expect(client.getStatus().status).toBe('reconnecting');

    // The ladder keeps climbing through the refusals and recovers on
    // its own when the local backend finishes coming back up.
    await vi.advanceTimersByTimeAsync(150);
    expect(client.getStatus().status).toBe('reconnecting');
    await vi.advanceTimersByTimeAsync(300);
    expect(fetchSpy.mock.calls.length).toBeGreaterThanOrEqual(3);
    expect(MockWebSocket.instances).toHaveLength(1);
    MockWebSocket.instances[0]!.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    expect(client.getStatus().status).toBe('connected');

    client.close();
  });

  // The WSL relay tears sockets down with 1005 and a minimised WebView2
  // gets suspended out from under its connection; neither is an auth
  // failure. Same backend, same token, recovery must be silent.
  it('rides out a relay teardown on a remote session without latching', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    vi.spyOn(console, 'warn').mockImplementation(() => {});

    const fetchSpy = vi
      .fn<() => Promise<{ wsUrl: string; token: string; remote?: boolean }>>()
      .mockResolvedValue({ wsUrl: 'ws://example/ws', token: 'same-token', remote: true });

    const client = createWSClient({
      WebSocketCtor: FakeCtor,
      bootstrap: fetchSpy,
      loopbackOrigin: () => false,
    });
    const seen: string[] = [];
    client.onStatusChange((snap) => seen.push(snap.status));
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    MockWebSocket.instances[0]!.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);

    // 1005: closed with no close frame — the intermediary-teardown
    // signature (internal/transport/AGENTS.md §Keepalive).
    MockWebSocket.instances[0]!.triggerClose(1005);
    // Two pre-open deaths force a bootstrap refetch; the server is the
    // same one, so the manifest comes back clean and nothing latches.
    for (let i = 0; i < BOOTSTRAP_INVALIDATE_AFTER_FAILURES; i++) {
      await vi.advanceTimersByTimeAsync(RECONNECT_MAX_REMOTE_MS);
      MockWebSocket.instances.at(-1)!.triggerClose(1005);
    }
    await vi.advanceTimersByTimeAsync(RECONNECT_MAX_REMOTE_MS);
    expect(fetchSpy.mock.calls.length).toBeGreaterThanOrEqual(2);
    MockWebSocket.instances.at(-1)!.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);

    expect(client.getStatus().status).toBe('connected');
    expect(seen).not.toContain('unauthorized');

    client.close();
  });

  // An unreachable backend throws a network error, which proves nothing
  // about the token. That loop must keep running forever.
  it('keeps retrying a network-down remote session and never latches', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    vi.spyOn(console, 'warn').mockImplementation(() => {});

    const fetchSpy = vi
      .fn<() => Promise<{ wsUrl: string; token: string }>>()
      .mockRejectedValue(new TypeError('Failed to fetch'));

    const client = createWSClient({
      WebSocketCtor: FakeCtor,
      bootstrap: fetchSpy,
      loopbackOrigin: () => false,
    });
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    expect(client.getStatus().status).toBe('reconnecting');

    await vi.advanceTimersByTimeAsync(5 * 60_000);
    expect(fetchSpy.mock.calls.length).toBeGreaterThan(5);
    expect(client.getStatus().status).toBe('reconnecting');
    expect(client.getStatus().nextAttemptAt).not.toBeNull();

    client.close();
  });

  it('reports an outage summary through the diagnostics sink on reconnect', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);

    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
    const diagnostics: Array<{ message: string; detail?: string }> = [];
    client.setDiagnosticsSink((message, detail) => diagnostics.push({ message, detail }));
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    const first = MockWebSocket.instances[0]!;
    first.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);
    expect(diagnostics).toHaveLength(0);

    first.triggerClose();
    // One failed reconnect attempt, then a successful one.
    await vi.advanceTimersByTimeAsync(1_000);
    MockWebSocket.instances[1]!.triggerClose();
    await vi.advanceTimersByTimeAsync(1_000);
    MockWebSocket.instances[2]!.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);

    expect(diagnostics).toHaveLength(1);
    // Fixed message (the persistence layer dedupes by it); varying
    // numbers in detail, carrying the original close code (1006).
    expect(diagnostics[0]!.message).toBe('transport: reconnected after outage');
    expect(diagnostics[0]!.detail).toMatch(/^down \d+\.\ds, close code 1006, 1 failed attempts$/);

    client.close();
  });

  it('rejects matching pending RPC immediately on oversized frame (no 30s timeout wait)', async () => {
    // Regression guard: a frame that exceeds the cap used to be
    // silently dropped via console.warn, leaving the matching RPC
    // pending until its 30s timeout fired and the UI stuck in
    // `loading=true`. The handler now extracts the rpc id and rejects
    // immediately with a TransportError('frame_too_large').
    //
    // Production cap is 75 MiB (symmetric with the server's
    // DefaultReadLimit). Tests pin a 4 KiB cap so a single oversized
    // frame allocates kilobytes, not megabytes — the code path is the
    // same and the cap value isn't the contract under test.
    const testCap = 4 * 1024;
    const client = createWSClient({
      WebSocketCtor: FakeCtor,
      bootstrap,
      maxFrameBytes: testCap,
    });

    const p = client.callByID(7, []);
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();
    const id = ws.sent[1]!.id as string;

    // Synthesize an oversize frame whose `id` matches the pending
    // RPC's. Real-world cause: a heavy `result` payload (large
    // `items.meta` blob) crosses the cap.
    const filler = 'x'.repeat(testCap + 1);
    const oversized = `{"type":"rpc","id":"${id}","result":"${filler}"}`;
    expect(oversized.length).toBeGreaterThan(testCap);
    ws.pushRawText(oversized);

    let caught: unknown;
    try {
      await p;
    } catch (err) {
      caught = err;
    }
    expect(caught).toBeInstanceOf(TransportError);
    expect((caught as TransportError).code).toBe('frame_too_large');

    client.close();
  });

  it('dispatches each event in a batch frame to the correct channel subscriber', async () => {
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });

    const seenA: unknown[] = [];
    const seenB: unknown[] = [];
    client.subscribe('channel:a', (data) => seenA.push(data));
    client.subscribe('channel:b', (data) => seenB.push(data));

    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    ws.pushFrame({
      type: 'batch',
      events: [
        { channel: 'channel:a', seq: 1, data: { v: 1 } },
        { channel: 'channel:b', seq: 1, data: { v: 2 } },
        { channel: 'channel:a', seq: 2, data: { v: 3 } },
      ],
    });

    expect(seenA).toEqual([{ v: 1 }, { v: 3 }]);
    expect(seenB).toEqual([{ v: 2 }]);

    client.close();
  });

  it('handles gap flags on individual batch entries', async () => {
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });

    const gaps: unknown[] = [];
    client.subscribe('transport:gap', (data) => gaps.push(data));

    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    ws.pushFrame({
      type: 'batch',
      events: [
        { channel: 'channel:a', seq: 10, data: { v: 1 }, gap: true },
        { channel: 'channel:a', seq: 11, data: { v: 2 } },
      ],
    });

    expect(gaps).toEqual([{ channel: 'channel:a', seq: 10 }]);

    client.close();
  });

  it('updates lastSeqByChannel for batch entries on reconnect replay', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });

    client.subscribe('ch:x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    const ws1 = MockWebSocket.instances[0]!;
    ws1.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);

    // Deliver events via a batch frame so lastSeqByChannel is populated.
    ws1.pushFrame({
      type: 'batch',
      events: [
        { channel: 'ch:x', seq: 5, data: {} },
        { channel: 'ch:y', seq: 3, data: {} },
      ],
    });

    // Disconnect and reconnect.
    ws1.triggerClose();
    await vi.advanceTimersByTimeAsync(125);

    expect(MockWebSocket.instances).toHaveLength(2);
    const ws2 = MockWebSocket.instances[1]!;
    ws2.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);

    // The replay frame should include seqs from the batch.
    const replay = ws2.sent.find(
      (f: Record<string, unknown>) => f.type === 'replay',
    ) as Record<string, unknown> | undefined;
    expect(replay).toBeDefined();
    const seqs = replay!.lastSeqByChannel as Record<string, number>;
    expect(seqs['ch:x']).toBe(5);
    expect(seqs['ch:y']).toBe(3);

    client.close();
    vi.useRealTimers();
  });
});
