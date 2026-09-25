// The client half of per-thread subscription narrowing: the `watch` frame
// the SPA sends, the ordering it sends it in, and the one client behavior
// that has to change alongside it.
//
// Coverage:
//   - a set is sent once and an identical set sends nothing (order and
//     duplicates do not count as a change)
//   - an EMPTY set is a real value, distinct from never having sent one
//   - a set composed while disconnected is retained and restated on open
//   - on reconnect the watch frame, scopes included, precedes the replay
//     frame on the same socket, which is what makes the backend apply the
//     filter before the replay it answers
//   - the scope set is absolute, sorted and deduped; `[]` is stated, and a
//     set past the wire bound is omitted (every scope) rather than truncated
//   - the forward-skip loss heuristic is exempted on entity-filtered
//     channels while a filter is armed, and on nothing else: not before a
//     filter exists, not on an unfiltered channel, and never for an
//     explicit gap:true marker
import { describe, expect, it, vi } from 'vitest';
import { createWSClient, MAX_WATCH_SCOPES, transportGapChannel } from './wsClient';
import { ENTITY_FILTERED_CHANNELS } from './entityFilteredChannels';
import { FakeCtor, flushMicrotasks, MockWebSocket } from '../../test/helpers/mockWebSocket';

const bootstrap = async () => ({ wsUrl: 'ws://example/ws', token: 'test-token' });

const FILTERED = ENTITY_FILTERED_CHANNELS[0]!;
// Any channel absent from that list; thread:updated is the one the
// filtered channels re-homed their off-pane consumers onto, so it is
// deliberately wildcard and will stay that way.
const UNFILTERED = 'thread:updated';

async function connectedClient() {
  MockWebSocket.reset();
  const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
  client.subscribe(UNFILTERED, () => {});
  await flushMicrotasks();
  const ws = MockWebSocket.instances[0]!;
  ws.acceptOpen();
  await flushMicrotasks();
  return { client, ws };
}

function watchFrames(ws: MockWebSocket): Array<Record<string, unknown>> {
  return ws.sent.filter((frame) => frame.type === 'watch');
}

describe('watch frame', () => {
  it('sends the set once and dedups an identical one', async () => {
    const { client, ws } = await connectedClient();

    client.setWatchedThreads(['t1', 't2'], []);
    client.setWatchedThreads(['t1', 't2'], []);
    // Same membership, different order and a duplicate: still not a change.
    client.setWatchedThreads(['t2', 't1', 't1'], []);

    expect(watchFrames(ws)).toEqual([{ type: 'watch', threads: ['t1', 't2'], scopes: [] }]);

    client.setWatchedThreads(['t1'], []);
    expect(watchFrames(ws)).toHaveLength(2);

    client.close();
  });

  it('treats an empty set as a real value, distinct from never sending one', async () => {
    const { client, ws } = await connectedClient();

    // Nothing sent yet: the connection is wildcard, which is what every
    // client that does not speak this frame stays on.
    expect(watchFrames(ws)).toHaveLength(0);

    client.setWatchedThreads([], []);
    expect(watchFrames(ws)).toEqual([{ type: 'watch', threads: [], scopes: [] }]);

    client.close();
  });

  it('retains a set composed while disconnected and restates it on open', async () => {
    MockWebSocket.reset();
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });

    // No socket yet — the send is dropped, the set is kept.
    client.setWatchedThreads(['t1'], []);

    client.subscribe(UNFILTERED, () => {});
    await flushMicrotasks();
    const ws = MockWebSocket.instances[0]!;
    ws.acceptOpen();
    await flushMicrotasks();

    expect(watchFrames(ws)).toEqual([{ type: 'watch', threads: ['t1'], scopes: [] }]);

    client.close();
  });

  it('sends the watch frame before the replay frame on reconnect', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    try {
      MockWebSocket.reset();
      const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap });
      client.subscribe(UNFILTERED, () => {});
      await vi.advanceTimersByTimeAsync(0);
      const first = MockWebSocket.instances[0]!;
      first.acceptOpen();
      await vi.advanceTimersByTimeAsync(0);
      client.setWatchedThreads(['t1'], [{ threadId: 't1', scopeRootId: 'agent-1' }]);

      first.triggerClose();
      await vi.advanceTimersByTimeAsync(125);
      const second = MockWebSocket.instances[1]!;
      second.acceptOpen();
      await flushMicrotasks();

      // Ordering is the whole contract: the backend reads frames in order
      // on one loop, so a replay ahead of the watch would answer for every
      // thread this client stopped looking at.
      expect(second.sent[0]).toEqual({ type: 'watch', threads: ['t1'], scopes: [{ threadId: 't1', scopeRootId: 'agent-1' }] });
      expect(second.sent[1]).toMatchObject({ type: 'replay' });

      client.close();
    } finally {
      vi.useRealTimers();
      vi.restoreAllMocks();
    }
  });
});

describe('watch scopes', () => {
  const scope = (threadId: string, scopeRootId: string) => ({ threadId, scopeRootId });

  it('sends the scope set sorted and deduped, and a scope change alone as a new frame', async () => {
    const { client, ws } = await connectedClient();

    client.setWatchedThreads(['t1'], [scope('t1', 'b'), scope('t1', 'a'), scope('t1', 'b')]);
    expect(watchFrames(ws)).toEqual([
      { type: 'watch', threads: ['t1'], scopes: [scope('t1', 'a'), scope('t1', 'b')] },
    ]);

    // Same pairs in another order: no frame.
    client.setWatchedThreads(['t1'], [scope('t1', 'a'), scope('t1', 'b')]);
    expect(watchFrames(ws)).toHaveLength(1);

    // Same threads, one scope closed: the absolute set is restated.
    client.setWatchedThreads(['t1'], [scope('t1', 'a')]);
    expect(watchFrames(ws)[1]).toEqual({ type: 'watch', threads: ['t1'], scopes: [scope('t1', 'a')] });

    client.setWatchedThreads(['t1'], []);
    expect(watchFrames(ws)[2]).toEqual({ type: 'watch', threads: ['t1'], scopes: [] });

    client.close();
  });

  it('drops a pair with an empty id rather than sending a frame the backend refuses', async () => {
    const { client, ws } = await connectedClient();

    client.setWatchedThreads(['t1'], [scope('t1', ''), scope('', 'a'), scope('t1', 'a')]);
    expect(watchFrames(ws)).toEqual([{ type: 'watch', threads: ['t1'], scopes: [scope('t1', 'a')] }]);

    client.close();
  });

  it('states a set past the pair bound by naming its heaviest threads whole', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    try {
      const { client, ws } = await connectedClient();
      const diagnostics: string[] = [];
      client.setDiagnosticsSink((message) => { diagnostics.push(message); });
      const agents = (threadId: string, n: number) =>
        Array.from({ length: n }, (_, i) => scope(threadId, `agent-${i}`));

      // Exactly at the bound is stated pair by pair.
      client.setWatchedThreads(['t1'], agents('t1', MAX_WATCH_SCOPES));
      expect(watchFrames(ws)[0]?.scopes).toHaveLength(MAX_WATCH_SCOPES);
      expect(watchFrames(ws)[0]?.scopeThreads).toBeUndefined();

      // One past it names the heaviest thread whole and keeps the rest.
      const light = agents('t2', 3);
      client.setWatchedThreads(['t1', 't2'], [...agents('t1', MAX_WATCH_SCOPES - 2), ...light]);
      expect(watchFrames(ws)[1]).toEqual({ type: 'watch', threads: ['t1', 't2'], scopes: light, scopeThreads: ['t1'] });

      // Unchanged: nothing sent. Fewer again: back to pairs.
      client.setWatchedThreads(['t1', 't2'], [...agents('t1', MAX_WATCH_SCOPES - 2), ...light]);
      expect(watchFrames(ws)).toHaveLength(2);
      client.setWatchedThreads(['t1', 't2'], light);
      expect(watchFrames(ws)[2]).toEqual({ type: 'watch', threads: ['t1', 't2'], scopes: light });

      // A scope root past the backend's byte bound (multi-byte characters
      // count) names its thread whole; one at the bound is a pair.
      client.setWatchedThreads(['t1'], [scope('t1', 'é'.repeat(129))]);
      expect(watchFrames(ws)[3]).toEqual({ type: 'watch', threads: ['t1'], scopes: [], scopeThreads: ['t1'] });
      client.setWatchedThreads(['t1'], [scope('t1', 'é'.repeat(128))]);
      expect(watchFrames(ws)[4]).toEqual({ type: 'watch', threads: ['t1'], scopes: [scope('t1', 'é'.repeat(128))] });

      // A thread id past the bound names nothing the backend accepts: the
      // set is omitted and reported.
      client.setWatchedThreads(['t1'], [scope('é'.repeat(129), 'a')]);
      expect(watchFrames(ws)[5]).toEqual({ type: 'watch', threads: ['t1'] });
      expect(diagnostics).toEqual(['transport: watched-scope set exceeded the wire bound']);

      client.close();
    } finally {
      warn.mockRestore();
    }
  });
});

describe('forward-skip exemption', () => {
  async function skipProbe(channel: string, arm: boolean) {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const { client, ws } = await connectedClient();
    const gaps: unknown[] = [];
    client.subscribe(transportGapChannel, (data) => { gaps.push(data); });
    const delivered: unknown[] = [];
    client.subscribe(channel, (data) => { delivered.push(data); });
    await flushMicrotasks();
    if (arm) client.setWatchedThreads(['t1'], []);

    ws.pushFrame({ type: 'event', channel, seq: 1, data: { v: 1 } });
    ws.pushFrame({ type: 'event', channel, seq: 9, data: { v: 9 } });
    await flushMicrotasks();

    const result = { gaps: gaps.length, warned: warn.mock.calls.length, delivered: delivered.length };
    client.close();
    warn.mockRestore();
    return result;
  }

  it('suppresses the skip resync on an entity-filtered channel while armed', async () => {
    const result = await skipProbe(FILTERED, true);
    expect(result.gaps).toBe(0);
    expect(result.warned).toBe(0);
    // The carried event is real data and still arrives; only the report
    // about what came BEFORE it is suppressed.
    expect(result.delivered).toBe(2);
  });

  it('keeps the skip resync on an entity-filtered channel before a filter is armed', async () => {
    const result = await skipProbe(FILTERED, false);
    expect(result.gaps).toBe(1);
  });

  it('keeps the skip resync on an unfiltered channel even while armed', async () => {
    const result = await skipProbe(UNFILTERED, true);
    expect(result.gaps).toBe(1);
  });

  it('still honours an explicit gap marker on an entity-filtered channel', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const { client, ws } = await connectedClient();
    const gaps: unknown[] = [];
    client.subscribe(transportGapChannel, (data) => { gaps.push(data); });
    client.subscribe(FILTERED, () => {});
    await flushMicrotasks();
    client.setWatchedThreads(['t1'], []);

    // A server statement about a real loss, not a heuristic — the
    // exemption must not touch it.
    ws.pushFrame({ type: 'event', channel: FILTERED, seq: 42, data: null, gap: true });
    await flushMicrotasks();

    expect(gaps).toHaveLength(1);
    client.close();
    warn.mockRestore();
  });
});
