// A backend that answers its manifest with a starting report is up and
// booting. The client publishes 'starting' with the report, asks again
// every STARTING_POLL_MS without climbing the reconnect ladder, and
// connects on the first manifest the backend serves. The poll pauses while
// the document is hidden and asks at once when it is shown again.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  createWSClient,
  DisconnectedError,
  DORMANT_AFTER_MS,
  STARTING_POLL_MS,
  type TransportStatusSnapshot,
} from './wsClient';
import { BackendStartingError, type StartupProgress } from './startupProgress';
import { __resetRunModeForTest } from './runMode';
import { FakeCtor, MockWebSocket } from '../../test/helpers/mockWebSocket';

const manifest = { wsUrl: 'ws://example/ws', token: 'test-token' };

function report(step: number, updatedAt: number, patch: Partial<StartupProgress> = {}): StartupProgress {
  return {
    phase: 'store.migrate',
    detail: `Applying migration ${step} of 7 add_index`,
    step,
    steps: 7,
    startedAt: 1_000,
    updatedAt,
    aliveAt: updatedAt,
    updatingTo: '',
    ...patch,
  };
}

describe('WSClient against a starting backend', () => {
  beforeEach(() => {
    MockWebSocket.reset();
    sessionStorage.clear();
    __resetRunModeForTest();
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
  });

  afterEach(() => {
    __resetRunModeForTest();
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it('reports progress, polls every 500 ms and connects on the first served manifest', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const fetchSpy = vi.fn<() => Promise<typeof manifest>>()
      .mockRejectedValueOnce(new BackendStartingError(report(3, 13_000)))
      .mockRejectedValueOnce(new BackendStartingError(report(4, 14_000, { updatingTo: '1.2.3' })))
      .mockResolvedValue(manifest);
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap: fetchSpy });
    const statuses: TransportStatusSnapshot[] = [];
    client.onStatusChange((snapshot) => statuses.push(snapshot));
    const diagnostics: string[] = [];
    client.setDiagnosticsSink((message) => diagnostics.push(message));

    const first = client.ready();
    const rejected = expect(first).rejects.toMatchObject({
      name: 'DisconnectedError',
      message: 'backend is starting: the computer is still starting',
      terminal: false,
    });
    await vi.advanceTimersByTimeAsync(0);
    await rejected;
    expect(client.getStatus()).toEqual({
      status: 'starting',
      nextAttemptAt: null,
      lastConnectedAt: null,
      startup: {
        phase: 'store.migrate',
        detail: 'Applying migration 3 of 7 add_index',
        step: 3,
        steps: 7,
        elapsedMs: 12_000,
        updatingTo: '',
      },
    });

    await vi.advanceTimersByTimeAsync(STARTING_POLL_MS - 1);
    expect(fetchSpy).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(fetchSpy).toHaveBeenCalledTimes(2);
    expect(client.getStatus().startup).toMatchObject({ step: 4, elapsedMs: 13_000, updatingTo: '1.2.3' });

    await vi.advanceTimersByTimeAsync(STARTING_POLL_MS);
    expect(fetchSpy).toHaveBeenCalledTimes(3);
    expect(MockWebSocket.instances).toHaveLength(1);
    MockWebSocket.instances[0]!.acceptOpen();
    await vi.advanceTimersByTimeAsync(0);

    expect(client.getStatus().status).toBe('connected');
    expect(client.getStatus().startup).toBeUndefined();
    // Starting is not an outage: no ladder status between the reports and
    // the connection, and nothing logged as a failed preparation.
    expect(statuses.map((snapshot) => snapshot.status)).toEqual(['disconnected', 'starting', 'starting', 'connected']);
    expect(warn.mock.calls.filter((call) => call[0] === 'wsClient: connection preparation failed')).toEqual([]);
    expect(diagnostics).toEqual([]);
    client.close();
  });

  it('keeps the poll flat for longer than the ladder takes to go dormant', async () => {
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    vi.spyOn(console, 'info').mockImplementation(() => {});
    let updatedAt = 2_000;
    let starting = true;
    const fetchSpy = vi.fn(async () => {
      if (!starting) throw new Error('fetch failed');
      updatedAt += STARTING_POLL_MS;
      throw new BackendStartingError(report(1, updatedAt));
    });
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap: fetchSpy });
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);

    await vi.advanceTimersByTimeAsync(DORMANT_AFTER_MS + 10 * STARTING_POLL_MS);
    const polls = DORMANT_AFTER_MS / STARTING_POLL_MS + 10;
    expect(fetchSpy).toHaveBeenCalledTimes(polls + 1);
    expect(client.getStatus()).toMatchObject({ status: 'starting' });
    expect(client.getStatus().dormant).toBeUndefined();

    // A backend that stops answering after starting is an ordinary outage,
    // entered at the ladder's first rung.
    starting = false;
    await vi.advanceTimersByTimeAsync(STARTING_POLL_MS);
    expect(client.getStatus()).toMatchObject({ status: 'reconnecting', dormant: false });
    const next = client.getStatus().nextAttemptAt;
    expect(next).not.toBeNull();
    expect(next! - Date.now()).toBeLessThanOrEqual(250);
    client.close();
  });

  it('rejects calls during startup as a passive, non-terminal disconnect', async () => {
    const fetchSpy = vi.fn(async () => { throw new BackendStartingError(report(1, 2_000)); });
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap: fetchSpy });
    const call = client.callByID(123, []);
    const settled = call.catch((err: unknown) => err);
    await vi.advanceTimersByTimeAsync(STARTING_POLL_MS * 2);
    const err = await settled;
    expect(err).toBeInstanceOf(DisconnectedError);
    expect((err as DisconnectedError).terminal).toBe(false);
    expect(client.getStatus().status).toBe('starting');
    client.close();
  });

  describe('while the document is hidden', () => {
    let visibilityState: DocumentVisibilityState = 'visible';
    function setVisibility(state: DocumentVisibilityState): void {
      visibilityState = state;
      document.dispatchEvent(new Event('visibilitychange'));
    }

    beforeEach(() => {
      visibilityState = 'visible';
      Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visibilityState });
    });

    afterEach(() => {
      delete (document as { visibilityState?: unknown }).visibilityState;
    });

    it('pauses the poll on hide and asks at once on show', async () => {
      const fetchSpy = vi.fn(async () => { throw new BackendStartingError(report(1, 2_000)); });
      const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap: fetchSpy });
      client.subscribe('x', () => {});
      await vi.advanceTimersByTimeAsync(0);
      await vi.advanceTimersByTimeAsync(STARTING_POLL_MS);
      expect(fetchSpy).toHaveBeenCalledTimes(2);

      setVisibility('hidden');
      await vi.advanceTimersByTimeAsync(STARTING_POLL_MS * 20);
      expect(fetchSpy).toHaveBeenCalledTimes(2);
      expect(client.getStatus().status).toBe('starting');

      setVisibility('visible');
      await vi.advanceTimersByTimeAsync(0);
      expect(fetchSpy).toHaveBeenCalledTimes(3);
      await vi.advanceTimersByTimeAsync(STARTING_POLL_MS);
      expect(fetchSpy).toHaveBeenCalledTimes(4);

      // A second hide pauses again: the transition is not one-shot.
      setVisibility('hidden');
      await vi.advanceTimersByTimeAsync(STARTING_POLL_MS * 4);
      expect(fetchSpy).toHaveBeenCalledTimes(4);
      setVisibility('visible');
      await vi.advanceTimersByTimeAsync(0);
      expect(fetchSpy).toHaveBeenCalledTimes(5);
      client.close();
    });

    it('does not arm the next ask when a report lands while hidden', async () => {
      let answer!: () => void;
      const fetchSpy = vi.fn(() => new Promise<typeof manifest>((_resolve, reject) => {
        answer = () => reject(new BackendStartingError(report(1, 2_000)));
      }));
      const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap: fetchSpy });
      client.subscribe('x', () => {});
      await vi.advanceTimersByTimeAsync(0);
      expect(fetchSpy).toHaveBeenCalledTimes(1);

      setVisibility('hidden');
      answer();
      await vi.advanceTimersByTimeAsync(STARTING_POLL_MS * 20);
      expect(client.getStatus().status).toBe('starting');
      expect(fetchSpy).toHaveBeenCalledTimes(1);

      setVisibility('visible');
      await vi.advanceTimersByTimeAsync(0);
      expect(fetchSpy).toHaveBeenCalledTimes(2);
      client.close();
    });

    it('still asks on demand while hidden', async () => {
      const fetchSpy = vi.fn(async () => { throw new BackendStartingError(report(1, 2_000)); });
      const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap: fetchSpy });
      client.subscribe('x', () => {});
      await vi.advanceTimersByTimeAsync(0);
      setVisibility('hidden');
      await vi.advanceTimersByTimeAsync(STARTING_POLL_MS * 4);
      expect(fetchSpy).toHaveBeenCalledTimes(1);

      const settled = client.callByID(123, []).catch((err: unknown) => err);
      await vi.advanceTimersByTimeAsync(0);
      expect(fetchSpy).toHaveBeenCalledTimes(2);
      expect(await settled).toBeInstanceOf(DisconnectedError);
      await vi.advanceTimersByTimeAsync(STARTING_POLL_MS * 4);
      expect(fetchSpy).toHaveBeenCalledTimes(2);
      client.close();
    });
  });

  it('stops polling when closed', async () => {
    const fetchSpy = vi.fn(async () => { throw new BackendStartingError(report(1, 2_000)); });
    const client = createWSClient({ WebSocketCtor: FakeCtor, bootstrap: fetchSpy });
    client.subscribe('x', () => {});
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchSpy).toHaveBeenCalledTimes(1);
    client.close();
    await vi.advanceTimersByTimeAsync(STARTING_POLL_MS * 4);
    expect(fetchSpy).toHaveBeenCalledTimes(1);
  });
});
