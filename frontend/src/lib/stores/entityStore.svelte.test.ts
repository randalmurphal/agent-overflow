import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { flushSync } from 'svelte';
import { createEntityStore, DEFAULT_RETRY } from './entityStore.svelte';
import {
  frontendErrorCaptureStateForTest,
  resetFrontendErrorCaptureForTest,
} from '../utils/frontendErrorCapture';

// A controllable source. Each call records its args and hands back a
// deferred so a test can decide when (and whether) the acquire resolves.
function makeSource<V = string>() {
  const calls: Array<{
    key: string;
    apply: (value: V) => void;
    fail: (err: unknown) => void;
    getCtx: () => { tag: string };
    signal: AbortSignal;
    resolve: (cleanup?: () => void | Promise<void>) => void;
    reject: (err: unknown) => void;
  }> = [];
  const cleanups: string[] = [];

  const source = (args: {
    key: string;
    getCtx: () => { tag: string };
    apply: (value: V) => void;
    fail: (err: unknown) => void;
    signal: AbortSignal;
  }): Promise<() => void | Promise<void>> =>
    new Promise((resolveOuter, rejectOuter) => {
      calls.push({
        key: args.key,
        apply: args.apply,
        fail: args.fail,
        getCtx: args.getCtx,
        signal: args.signal,
        resolve: (cleanup) =>
          resolveOuter(cleanup ?? (() => {
            cleanups.push(args.key);
          })),
        reject: rejectOuter,
      });
    });

  return { source, calls, cleanups };
}

async function flush(times = 4): Promise<void> {
  for (let i = 0; i < times; i += 1) await Promise.resolve();
}

describe('createEntityStore — refcounted lifecycle', () => {
  let errors: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    errors = vi.spyOn(console, 'error').mockImplementation(() => {});
  });

  afterEach(() => {
    errors.mockRestore();
  });

  it('sources once for the first attacher and shares it with the second', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    const a = store.attach('k', { tag: 'a' });
    const b = store.attach('k', { tag: 'b' });
    await flush();

    expect(calls).toHaveLength(1);
    calls[0].apply('v1');
    calls[0].resolve();
    await flush();

    expect(a.current).toBe('v1');
    expect(b.current).toBe('v1');
    a.release();
    b.release();
  });

  it('a second attach while source() is in flight does not double-source', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    store.attach('k', { tag: 'a' });
    await flush();
    expect(calls).toHaveLength(1);

    // Still unresolved — the single-flight guard must hold.
    store.attach('k', { tag: 'b' });
    await flush();
    expect(calls).toHaveLength(1);
  });

  it('tears down on the LAST release, not the first', async () => {
    const { source, calls, cleanups } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    const a = store.attach('k', { tag: 'a' });
    const b = store.attach('k', { tag: 'b' });
    await flush();
    calls[0].resolve();
    await flush();

    a.release();
    await flush();
    expect(cleanups).toEqual([]);
    expect(store.keys()).toEqual(['k']);

    b.release();
    await flush();
    expect(cleanups).toEqual(['k']);
    expect(store.keys()).toEqual([]);
  });

  it('re-attaching after a full release sources again', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    const a = store.attach('k', { tag: 'a' });
    await flush();
    calls[0].apply('v1');
    calls[0].resolve();
    await flush();
    a.release();
    await flush();
    expect(store.peek('k')).toBeNull();

    const b = store.attach('k', { tag: 'a' });
    await flush();
    expect(calls).toHaveLength(2);
    calls[1].apply('v2');
    calls[1].resolve();
    await flush();
    expect(b.current).toBe('v2');
  });

  // attach → release → re-attach is one key's normal life (a pane closing
  // and reopening on the same workspace), and the first run's acquire can
  // land after the second one is already live. The late arrival owns
  // exactly its own resource: it must release that and nothing else, or a
  // shared-by-key teardown takes the live subscription down with it.
  it('a late first run cleans up only itself, leaving the re-attached run intact', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    const a = store.attach('k', { tag: 'a' });
    await flush();
    a.release();
    await flush();

    const b = store.attach('k', { tag: 'b' });
    await flush();
    expect(calls).toHaveLength(2);

    const secondCleanup = vi.fn();
    calls[1].apply('live');
    calls[1].resolve(secondCleanup);
    await flush();
    expect(b.current).toBe('live');

    // Run 1 finally lands, for a world that ended two steps ago.
    const firstCleanup = vi.fn();
    calls[0].apply('stale');
    calls[0].resolve(firstCleanup);
    await flush();

    expect(firstCleanup).toHaveBeenCalledTimes(1);
    expect(secondCleanup).not.toHaveBeenCalled();
    expect(b.current).toBe('live');
    expect(store.keys()).toEqual(['k']);

    b.release();
    await flush();
    expect(secondCleanup).toHaveBeenCalledTimes(1);
  });

  it('release() is idempotent — a double release cannot drop a sibling reference', async () => {
    const { source, calls, cleanups } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    const a = store.attach('k', { tag: 'a' });
    const b = store.attach('k', { tag: 'b' });
    await flush();
    calls[0].resolve();
    await flush();

    a.release();
    a.release();
    a.release();
    await flush();

    expect(cleanups).toEqual([]);
    expect(store.keys()).toEqual(['k']);
    expect(b.current).toBeNull();
  });

  it('a source completing after teardown runs its own cleanup and applies nothing', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    const a = store.attach('k', { tag: 'a' });
    await flush();
    a.release();
    await flush();

    // The acquire finally lands, with nobody holding the key.
    const orphanCleanup = vi.fn();
    calls[0].apply('late');
    calls[0].resolve(orphanCleanup);
    await flush();

    expect(orphanCleanup).toHaveBeenCalledTimes(1);
    expect(store.peek('k')).toBeNull();
  });
});

describe('createEntityStore — apply chokepoint', () => {
  beforeEach(() => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('runs onApply with the previous value on every apply path', async () => {
    const { source, calls } = makeSource();
    const seen: Array<[string, string, string | null]> = [];
    const store = createEntityStore<string, { tag: string }>({
      name: 'test',
      source,
      onApply: (key, value, prev) => seen.push([key, value, prev]),
    });

    store.attach('k', { tag: 'a' });
    await flush();
    calls[0].apply('v1');
    calls[0].resolve();
    await flush();
    store.apply('k', 'v2');

    expect(seen).toEqual([
      ['k', 'v1', null],
      ['k', 'v2', 'v1'],
    ]);
  });

  it('apply on a key nobody holds is a no-op, not a resurrection', async () => {
    const { source } = makeSource();
    const onApply = vi.fn();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source, onApply });

    store.apply('ghost', 'v1');
    expect(store.peek('ghost')).toBeNull();
    expect(store.keys()).toEqual([]);
    expect(onApply).not.toHaveBeenCalled();
  });

  it('applyError records the message and apply clears it', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    const a = store.attach('k', { tag: 'a' });
    await flush();
    calls[0].resolve();
    await flush();

    store.applyError('k', new Error('git busy'));
    expect(a.error).toBe('git busy');
    expect(store.peekError('k')).toBe('git busy');

    store.apply('k', 'v1');
    expect(a.error).toBeNull();
  });

  it('a source error lands in `error` AND console.error', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    const a = store.attach('k', { tag: 'a' });
    await flush();
    calls[0].reject(new Error('subscribe failed'));
    await flush();

    expect(a.error).toBe('subscribe failed');
    expect(console.error).toHaveBeenCalled();
  });

  it('getCtx() returns a live attacher ctx, and survives the original attacher leaving', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    const a = store.attach('k', { tag: 'a' });
    const b = store.attach('k', { tag: 'b' });
    await flush();
    expect(calls[0].getCtx()).toEqual({ tag: 'a' });

    a.release();
    expect(calls[0].getCtx()).toEqual({ tag: 'b' });
    b.release();
    expect(() => calls[0].getCtx()).toThrow(/no live attacher/);
  });
});

describe('createEntityStore — retry', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.spyOn(console, 'error').mockImplementation(() => {});
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it('retries on a doubling backoff while attached', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({
      name: 'test',
      source,
    });

    store.attach('k', { tag: 'a' });
    await flush();
    calls[0].reject(new Error('boom'));
    await flush();
    expect(calls).toHaveLength(1);

    await vi.advanceTimersByTimeAsync(DEFAULT_RETRY.initialMs - 1);
    expect(calls).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(calls).toHaveLength(2);

    calls[1].reject(new Error('boom'));
    await flush();
    await vi.advanceTimersByTimeAsync(DEFAULT_RETRY.initialMs * 2 - 1);
    expect(calls).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(calls).toHaveLength(3);
  });

  // A consumer mounting onto a key that is deep in someone else's backoff
  // must not inherit it: the curve exists to stop hammering a failing
  // backend, not to keep a freshly opened pane blank for half a minute.
  it('an attach onto a key in backoff resets the curve and sources now', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    const a = store.attach('k', { tag: 'a' });
    await flush();
    calls[0].reject(new Error('boom'));
    await flush();
    // Curve armed at initialMs; let it fire once so the next wait is 2x.
    await vi.advanceTimersByTimeAsync(DEFAULT_RETRY.initialMs);
    calls[1].reject(new Error('boom again'));
    await flush();
    expect(calls).toHaveLength(2);

    const b = store.attach('k', { tag: 'b' });
    await flush();
    expect(calls).toHaveLength(3);

    // And the curve it reset is the initial one, not the doubled wait the
    // superseded timer was holding.
    calls[2].reject(new Error('still boom'));
    await flush();
    await vi.advanceTimersByTimeAsync(DEFAULT_RETRY.initialMs);
    expect(calls).toHaveLength(4);
    a.release();
    b.release();
  });

  it('does not retry after the last release', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({
      name: 'test',
      source,
    });

    const a = store.attach('k', { tag: 'a' });
    await flush();
    a.release();
    calls[0].reject(new Error('boom'));
    await flush();

    await vi.advanceTimersByTimeAsync(10_000);
    expect(calls).toHaveLength(1);
  });

  it('a pending retry is cancelled by release', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({
      name: 'test',
      source,
    });

    const a = store.attach('k', { tag: 'a' });
    await flush();
    calls[0].reject(new Error('boom'));
    await flush();

    a.release();
    await vi.advanceTimersByTimeAsync(10_000);
    expect(calls).toHaveLength(1);
  });

  it('resetAll during a pending retry re-sources immediately and drops the stale timer', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({
      name: 'test',
      source,
    });

    store.attach('k', { tag: 'a' });
    await flush();
    calls[0].reject(new Error('boom'));
    await flush();

    store.resetAll();
    await flush();
    expect(calls).toHaveLength(2);

    // The retry timer armed before resetAll must not fire a THIRD source.
    await vi.advanceTimersByTimeAsync(10_000);
    expect(calls).toHaveLength(2);
  });

  // fail() is a throw minus the unwind: a source whose observations are
  // PUSHED (an event-driven poll) reports failure that way, and it must get
  // the same recovery — or every such store hand-rolls one.
  it('fail() from a live run schedules the same backed-off re-source', async () => {
    const { source, calls, cleanups } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    store.attach('k', { tag: 'a' });
    await flush();
    calls[0].resolve();
    await flush();

    calls[0].fail(new Error('poll failed'));
    await flush();
    expect(store.peekError('k')).toContain('poll failed');
    expect(calls).toHaveLength(1);

    await vi.advanceTimersByTimeAsync(DEFAULT_RETRY.initialMs);
    expect(calls).toHaveLength(2);
    // …and the resource the failing run had acquired was released before the
    // retry re-acquired: fail() leaves it in place, unlike a throw.
    expect(cleanups).toEqual(['k']);
  });

  it('repeated fail()s inside one window are ONE failure, not a curve reset', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    store.attach('k', { tag: 'a' });
    await flush();
    calls[0].resolve();
    await flush();

    // Five reports of the same outage, spread across the first window.
    for (let i = 0; i < 5; i += 1) {
      calls[0].fail(new Error('poll failed'));
      await vi.advanceTimersByTimeAsync(DEFAULT_RETRY.initialMs / 10);
    }
    expect(calls).toHaveLength(1);

    await vi.advanceTimersByTimeAsync(DEFAULT_RETRY.initialMs);
    expect(calls).toHaveLength(2);

    // The curve advanced exactly once across those five reports, so the
    // still-broken re-source waits the DOUBLED delay.
    calls[1].reject(new Error('still broken'));
    await flush();
    await vi.advanceTimersByTimeAsync(DEFAULT_RETRY.initialMs * 2 - 1);
    expect(calls).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(calls).toHaveLength(3);
  });

  it('an apply() cancels the pending retry and puts the curve back at the bottom', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    store.attach('k', { tag: 'a' });
    await flush();
    calls[0].resolve();
    await flush();

    calls[0].fail(new Error('poll failed'));
    await flush();
    // The next poll succeeds before the retry lands: nothing to recover.
    calls[0].apply('ok');
    await flush();
    expect(store.peekError('k')).toBeNull();

    await vi.advanceTimersByTimeAsync(10_000);
    expect(calls).toHaveLength(1);

    // And the curve is back at the bottom — the next failure waits initialMs,
    // not the doubled delay the cancelled one had armed.
    calls[0].fail(new Error('poll failed again'));
    await flush();
    await vi.advanceTimersByTimeAsync(DEFAULT_RETRY.initialMs - 1);
    expect(calls).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(calls).toHaveLength(2);
  });

  // The other half of "an attach is fresh demand": a fail() that arrived
  // AFTER its run acquired a cleanup leaves that cleanup in place, so the
  // entry still reads as sourced. Cancelling the armed timer and then
  // sourcing only "if nothing is sourced" started nothing and left nothing
  // armed — the key stayed errored for as long as anybody held it.
  it('an attach onto a key that failed AFTER acquiring re-sources it', async () => {
    const { source, calls, cleanups } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    const a = store.attach('k', { tag: 'a' });
    await flush();
    calls[0].resolve();
    await flush();
    calls[0].fail(new Error('poll failed'));
    await flush();
    expect(calls).toHaveLength(1);
    expect(store.peekError('k')).toContain('poll failed');

    const b = store.attach('k', { tag: 'b' });
    await flush();
    expect(calls).toHaveLength(2);
    // The failing run's resource is released before the fresh acquire, so
    // its listeners cannot stack up behind the new ones.
    expect(cleanups).toEqual(['k']);

    calls[1].apply('v1');
    calls[1].resolve();
    await flush();
    expect(store.peekError('k')).toBeNull();
    expect(b.current).toBe('v1');
    a.release();
    b.release();
  });

  // The source-provided apply cancelled the retry; the public one did not,
  // so a healthy wire event landing after a fail() left the timer armed —
  // and it later tore down and re-acquired a resource nothing was wrong
  // with, dropping the pushes that arrive in the gap.
  it('an apply through the public chokepoint cancels a retry that would re-acquire', async () => {
    const { source, calls, cleanups } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    store.attach('k', { tag: 'a' });
    await flush();
    calls[0].resolve();
    await flush();
    calls[0].fail(new Error('poll failed'));
    await flush();

    store.apply('k', 'from-the-wire');
    await flush();
    expect(store.peekError('k')).toBeNull();

    await vi.advanceTimersByTimeAsync(60_000);
    expect(calls).toHaveLength(1);
    expect(cleanups).toEqual([]);
  });

  // …and the other exception is a key with NOTHING acquired. A wire event
  // can reach one through a sibling alias (two workspace keys sharing a
  // canonical cwd), and the armed timer is then the only path back to a live
  // source: cancelling it left the key unsourced for good — holders never
  // re-attach, so nothing re-arms it.
  it('an apply keeps the retry when the source never acquired anything', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    store.attach('k', { tag: 'a' });
    await flush();
    calls[0].reject(new Error('subscribe failed'));
    await flush();
    // Let one retry run and fail, so the curve is at the doubled delay.
    await vi.advanceTimersByTimeAsync(DEFAULT_RETRY.initialMs);
    expect(calls).toHaveLength(2);
    calls[1].reject(new Error('subscribe failed again'));
    await flush();

    store.apply('k', 'from-a-sibling-alias');
    expect(store.peek('k')).toBe('from-a-sibling-alias');
    expect(store.peekError('k')).toBeNull();

    // The subscribe failure is not over because data arrived by another
    // route, so the curve is not reset either: the retry is still the
    // ESCALATED wait away, and it re-runs the source when it lands.
    await vi.advanceTimersByTimeAsync(DEFAULT_RETRY.initialMs * 2 - 1);
    expect(calls).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(calls).toHaveLength(3);
  });

  // …and a PARTIAL observation is the exception: it refreshes the value but
  // is no evidence the failing thing recovered, so it must leave both the
  // error and the curve alone.
  it('a preserveError apply keeps the error and the armed retry', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    store.attach('k', { tag: 'a' });
    await flush();
    calls[0].resolve();
    await flush();
    calls[0].fail(new Error('poll failed'));
    await flush();

    store.apply('k', 'partial', { preserveError: true });
    expect(store.peek('k')).toBe('partial');
    expect(store.peekError('k')).toContain('poll failed');

    await vi.advanceTimersByTimeAsync(DEFAULT_RETRY.initialMs);
    expect(calls).toHaveLength(2);
  });

  it('does not retry a fail() from a superseded run', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    const handle = store.attach('k', { tag: 'a' });
    await flush();
    calls[0].resolve();
    await flush();

    handle.release();
    calls[0].fail(new Error('too late'));
    await flush();

    await vi.advanceTimersByTimeAsync(10_000);
    expect(calls).toHaveLength(1);
    expect(store.keys()).toEqual([]);
  });
});

describe('createEntityStore — invalidate / resetAll / suspend', () => {
  beforeEach(() => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('invalidate re-sources a live key and cleans up the previous acquire', async () => {
    const { source, calls, cleanups } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    const a = store.attach('k', { tag: 'a' });
    await flush();
    calls[0].apply('v1');
    calls[0].resolve();
    await flush();

    store.invalidate('k');
    await flush();
    expect(cleanups).toEqual(['k']);
    expect(calls).toHaveLength(2);
    // The observed value survives a re-acquire — re-subscribing is not
    // evidence that what we last saw is wrong.
    expect(a.current).toBe('v1');

    calls[1].apply('v2');
    calls[1].resolve();
    await flush();
    expect(a.current).toBe('v2');
  });

  it('invalidate during an in-flight source discards the first acquire, not the second', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    const a = store.attach('k', { tag: 'a' });
    await flush();
    store.invalidate('k');
    await flush();
    expect(calls).toHaveLength(2);

    // The FIRST acquire lands late: its cleanup must run and its value must
    // not reach the entry, or the store would hold a subscription nobody
    // tracks and paint a value the second acquire is about to replace.
    const staleCleanup = vi.fn();
    calls[0].apply('stale');
    calls[0].resolve(staleCleanup);
    await flush();
    expect(staleCleanup).toHaveBeenCalledTimes(1);
    expect(a.current).toBeNull();

    calls[1].apply('fresh');
    calls[1].resolve();
    await flush();
    expect(a.current).toBe('fresh');
  });

  it('aborts a superseded run so its remaining work never starts', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    const a = store.attach('k', { tag: 'a' });
    await flush();
    expect(calls[0].signal.aborted).toBe(false);

    // Dropping apply()s from a stale run is not enough: a source that does
    // anything with a side effect after an await (mcpServers spawns a
    // provider health-check) has to be able to see that it was replaced.
    store.invalidate('k');
    expect(calls[0].signal.aborted).toBe(true);
    await flush();
    expect(calls[1].signal.aborted).toBe(false);

    calls[1].resolve();
    await flush();
    a.release();
    expect(calls[1].signal.aborted).toBe(true);
  });

  it('aborts the in-flight run on suspend, and the fresh one survives resetAll', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    store.attach('k', { tag: 'a' });
    await flush();

    store.suspend();
    expect(calls[0].signal.aborted).toBe(true);

    store.resetAll();
    await flush();
    expect(calls).toHaveLength(2);
    expect(calls[1].signal.aborted).toBe(false);
  });

  // A failed release leaks a backend resource. While somebody is still
  // holding the key that is user-facing state, not only a console line —
  // the resource they are nominally attached to was never released.
  it('a cleanup failure on a LIVE key surfaces as the entity error', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    const a = store.attach('k', { tag: 'a' });
    await flush();
    calls[0].apply('v1');
    calls[0].resolve(() => {
      throw new Error('unsubscribe failed');
    });
    await flush();

    store.invalidate('k');
    await flush();
    expect(a.error).toBe('unsubscribe failed');
    expect(console.error).toHaveBeenCalled();

    // The re-acquire's first observation clears it.
    calls[1].apply('v2');
    calls[1].resolve();
    await flush();
    expect(a.error).toBeNull();
    a.release();
  });

  it('a cleanup failure on the way OUT stays console-only — no reader is left', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    const a = store.attach('k', { tag: 'a' });
    await flush();
    calls[0].resolve(() => {
      throw new Error('unsubscribe failed');
    });
    await flush();

    a.release();
    await flush();
    expect(console.error).toHaveBeenCalled();
    expect(store.keys()).toEqual([]);
    expect(store.peekError('k')).toBeNull();
  });

  it('invalidate is a no-op for a key nobody holds', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });
    store.invalidate('ghost');
    await flush();
    expect(calls).toHaveLength(0);
  });

  // The transport-gap entry point. Unlike resetAll (a reconnect invalidated
  // the backend resources themselves) the socket is fine here and only the
  // observations are suspect, so every value has to survive the re-source —
  // blanking would flicker every consumer for data that is probably still
  // correct.
  it('invalidateAll re-sources every held key while keeping their values', async () => {
    const { source, calls, cleanups } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    const a = store.attach('a', { tag: 'a' });
    const b = store.attach('b', { tag: 'b' });
    await flush();
    calls[0].apply('va');
    calls[0].resolve();
    calls[1].apply('vb');
    calls[1].resolve();
    await flush();

    store.invalidateAll();
    await flush();

    expect(cleanups.sort()).toEqual(['a', 'b']);
    expect(calls).toHaveLength(4);
    expect(a.current).toBe('va');
    expect(b.current).toBe('vb');

    calls[2].apply('va2');
    calls[2].resolve();
    calls[3].apply('vb2');
    calls[3].resolve();
    await flush();
    expect(a.current).toBe('va2');
    expect(b.current).toBe('vb2');
  });

  it('invalidateAll no-ops while suspended — the resetAll that lifts it re-sources', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    const a = store.attach('a', { tag: 'a' });
    await flush();
    calls[0].apply('va');
    calls[0].resolve();
    await flush();

    store.suspend();
    await flush();
    store.invalidateAll();
    await flush();
    expect(calls).toHaveLength(1);

    store.resetAll();
    await flush();
    expect(calls).toHaveLength(2);
    a.release();
  });

  it('resetAll clears values and re-sources every held key', async () => {
    const { source, calls, cleanups } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    const a = store.attach('a', { tag: 'a' });
    const b = store.attach('b', { tag: 'b' });
    await flush();
    calls[0].apply('va');
    calls[0].resolve();
    calls[1].apply('vb');
    calls[1].resolve();
    await flush();

    store.resetAll();
    await flush();

    expect(cleanups.sort()).toEqual(['a', 'b']);
    expect(a.current).toBeNull();
    expect(b.current).toBeNull();
    expect(calls).toHaveLength(4);
  });

  it('suspend releases resources, keeps references, and refuses to source until resetAll', async () => {
    const { source, calls, cleanups } = makeSource();
    const store = createEntityStore<string, { tag: string }>({
      name: 'test',
      source,
    });

    const a = store.attach('k', { tag: 'a' });
    await flush();
    calls[0].apply('v1');
    calls[0].resolve();
    await flush();

    store.suspend();
    await flush();
    expect(cleanups).toEqual(['k']);
    expect(a.current).toBeNull();
    expect(store.keys()).toEqual(['k']);
    expect(calls).toHaveLength(1);

    // A fresh attach while suspended registers but does not source.
    const b = store.attach('k', { tag: 'b' });
    await flush();
    expect(calls).toHaveLength(1);

    store.resetAll();
    await flush();
    expect(calls).toHaveLength(2);
    calls[1].apply('v2');
    calls[1].resolve();
    await flush();
    expect(a.current).toBe('v2');
    expect(b.current).toBe('v2');
  });

  it('suspend drops keys nobody holds', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    const a = store.attach('k', { tag: 'a' });
    await flush();
    calls[0].resolve();
    await flush();
    a.release();
    await flush();

    store.suspend();
    expect(store.keys()).toEqual([]);
  });
});

describe('createEntityStore — onDrop', () => {
  beforeEach(() => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('fires when the last reference goes, and NOT on a re-source', async () => {
    const { source, calls } = makeSource();
    const dropped: string[] = [];
    const store = createEntityStore<string, { tag: string }>({
      name: 'test',
      source,
      onDrop: (key) => dropped.push(key),
    });

    const a = store.attach('k', { tag: 'a' });
    await flush();
    calls[0].resolve();
    await flush();

    // A re-acquire keeps the entry, so derived caches hanging off onDrop
    // must survive it.
    store.invalidate('k');
    await flush();
    calls[1].resolve();
    await flush();
    expect(dropped).toEqual([]);

    a.release();
    await flush();
    expect(dropped).toEqual(['k']);
  });

  it('fires for the keys suspend and resetAll actually remove', async () => {
    const { source, calls } = makeSource();
    const dropped: string[] = [];
    const store = createEntityStore<string, { tag: string }>({
      name: 'test',
      source,
      onDrop: (key) => dropped.push(key),
    });

    const held = store.attach('held', { tag: 'a' });
    const loose = store.attach('loose', { tag: 'b' });
    await flush();
    for (const call of calls) call.resolve();
    await flush();
    loose.release();
    await flush();
    expect(dropped).toEqual(['loose']);

    // A held key survives both — it is re-sourced, not dropped.
    store.suspend();
    store.resetAll();
    await flush();
    expect(dropped).toEqual(['loose']);

    held.release();
    await flush();
    expect(dropped).toEqual(['loose', 'held']);
  });
});

describe('createEntityStore — snapshot', () => {
  beforeEach(() => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('reads the value without subscribing the caller to the key', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    const a = store.attach('k', { tag: 'a' });
    await flush();
    calls[0].apply('v1');
    calls[0].resolve();
    await flush();
    expect(store.snapshot('k')).toBe('v1');
    expect(store.snapshot('ghost')).toBeNull();

    // The point of the non-reactive read: a handler that reads a value in
    // order to write the next one must not become a dependent of its own
    // write. peek() there re-enters; snapshot() takes no dependency at all.
    let runs = 0;
    const cleanup = $effect.root(() => {
      $effect(() => {
        runs += 1;
        store.snapshot('k');
      });
    });
    flushSync();
    expect(runs).toBe(1);

    store.apply('k', 'v2');
    flushSync();
    expect(runs).toBe(1);
    expect(store.snapshot('k')).toBe('v2');
    cleanup();
    a.release();
  });
});

describe('createEntityStore — reactivity boundary', () => {
  let errors: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    errors = vi.spyOn(console, 'error').mockImplementation(() => {});
  });

  afterEach(() => {
    errors.mockRestore();
  });

  it('attaching from inside an $effect does not invalidate that effect', async () => {
    // The normal way a component holds an entity is an $effect that
    // attaches and releases in its teardown. If the store's key lookup were
    // reactive on that path, the effect would depend on its own insert:
    // attach -> invalidate -> release -> re-attach, until svelte aborts the
    // flush with effect_update_depth_exceeded. Nothing about the failure
    // points at the store, so it is asserted here.
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    let runs = 0;
    let releases = 0;
    const cleanup = $effect.root(() => {
      $effect(() => {
        runs += 1;
        const attachment = store.attach('k', { tag: 'a' });
        return () => {
          releases += 1;
          attachment.release();
        };
      });
    });

    await flush();
    flushSync();
    await flush();

    expect(runs).toBe(1);
    expect(releases).toBe(0);
    expect(calls).toHaveLength(1);
    cleanup();
    expect(releases).toBe(1);
  });

  it('a peek that found nothing re-runs once the key is attached', async () => {
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'test', source });

    const seen: Array<string | null> = [];
    const cleanup = $effect.root(() => {
      $effect(() => {
        seen.push(store.peek('k'));
      });
    });
    flushSync();
    expect(seen).toEqual([null]);

    const a = store.attach('k', { tag: 'a' });
    await flush();
    calls[0].apply('v1');
    calls[0].resolve();
    await flush();
    flushSync();
    // Two re-runs: the key appearing (still unobserved) and the first
    // observation landing. The first is the one a plain Map cannot deliver.
    expect(seen).toEqual([null, null, 'v1']);

    a.release();
    await flush();
    flushSync();
    expect(seen).toEqual([null, null, 'v1', null]);
    cleanup();
  });
});

/**
 * `rawValue` buys one signal per write by not proxying the value. The cost is
 * that the ONLY thing a reader can observe is the assignment, so a writer that
 * mutates the held object and applies it back writes a reference the signal
 * already holds: no reader wakes, the surface freezes on stale content, and
 * every gate stays green. The contract is enforced here rather than documented.
 */
describe('createEntityStore — the rawValue replace contract', () => {
  beforeEach(() => {
    resetFrontendErrorCaptureForTest();
  });
  afterEach(() => {
    resetFrontendErrorCaptureForTest();
  });

  function rawStore() {
    const { source, calls } = makeSource<{ n: number }>();
    const store = createEntityStore<{ n: number }, { tag: string }>({
      name: 'raw', rawValue: true, source,
    });
    return { store, calls };
  }

  it('a replacement wakes the reader', async () => {
    const { store, calls } = rawStore();
    const seen: Array<number | null> = [];
    const cleanup = $effect.root(() => {
      $effect(() => { seen.push(store.peek('k')?.n ?? null); });
    });
    const a = store.attach('k', { tag: 'a' });
    await flush();
    flushSync();

    calls[0].apply({ n: 1 });
    flushSync();
    calls[0].apply({ n: 2 });
    flushSync();

    expect(seen.filter((v) => v !== null)).toEqual([1, 2]);
    a.release();
    cleanup();
  });

  it('a mutate-and-reapply is REPORTED, not silently accepted', async () => {
    const errors = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { store, calls } = rawStore();
    const a = store.attach('k', { tag: 'a' });
    await flush();

    const held = { n: 1 };
    calls[0].apply(held);
    const before = frontendErrorCaptureStateForTest().pendingCount;
    expect(errors).not.toHaveBeenCalled();

    held.n = 2;
    calls[0].apply(held);

    expect(errors).toHaveBeenCalledTimes(1);
    expect(String(errors.mock.calls[0]?.[0])).toContain('same object reference for k');
    // And it reaches the persisted diagnostic trail, not just a console
    // nobody has open: this is a defect whose only symptom is stale pixels.
    expect(frontendErrorCaptureStateForTest().pendingCount).toBe(before + 1);
    // The value still lands — it IS current truth; the report is the fix.
    expect(store.peek('k')).toBe(held);

    a.release();
    errors.mockRestore();
  });

  it('says nothing when two DIFFERENT objects happen to be equal', async () => {
    const errors = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { store, calls } = rawStore();
    const a = store.attach('k', { tag: 'a' });
    await flush();

    calls[0].apply({ n: 1 });
    calls[0].apply({ n: 1 });

    expect(errors).not.toHaveBeenCalled();
    a.release();
    errors.mockRestore();
  });

  it('does not fire for a store that did not opt into rawValue', async () => {
    const errors = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { source, calls } = makeSource();
    const store = createEntityStore<string, { tag: string }>({ name: 'proxied', source });
    const a = store.attach('k', { tag: 'a' });
    await flush();

    // A proxied entry re-applied by reference is fine: the proxy reports the
    // field writes underneath it, so readers already woke.
    calls[0].apply('v1');
    calls[0].apply('v1');

    expect(errors).not.toHaveBeenCalled();
    a.release();
    errors.mockRestore();
  });
});
