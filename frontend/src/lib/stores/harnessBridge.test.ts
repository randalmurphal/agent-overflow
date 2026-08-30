// The wire edge's three load-bearing claims, none of which is visible from
// inside lib/harness/:
//
//   1. an ordinary (or remote) session never imports the bridge chunk, and
//      a harness session does not import it until a query actually arrives;
//   2. a bridge that fails to LOAD still answers the backend, because the
//      alternative is a caller parked for the full 10s waiter timeout;
//   3. teardown takes the subscription and the bridge down together.
//
// Each case runs against a freshly reset module graph: the store memoises
// the loaded chunk and both mode modules latch, so "was it imported" is
// only a question you can ask once per graph.

import { afterEach, describe, expect, it, vi } from 'vitest';

// The chunk's own instrumentation. `activations` is the per-test signal —
// the store calls activateHarnessBridge exactly once per graph, the moment
// it first reaches the module — while `loads` counts factory evaluations,
// which vitest caches across resetModules and so only ever fires once per
// file.
const chunk = vi.hoisted(() => ({ loads: 0, activations: 0, stops: 0, receipts: [] as unknown[] }));

// Named so the one case that replaces this mock (a chunk that fails to
// load) can put it back afterwards.
const countingBridge = vi.hoisted(
  () =>
    async (importOriginal: () => Promise<typeof import('../harness/bridge')>) => {
      chunk.loads += 1;
      const actual = await importOriginal();
      return {
        ...actual,
        activateHarnessBridge: () => {
          chunk.activations += 1;
          return actual.activateHarnessBridge();
        },
        stopHarnessBridge: (...args: Parameters<typeof actual.stopHarnessBridge>) => {
          chunk.stops += 1;
          const receipt = actual.stopHarnessBridge(...args);
          chunk.receipts.push(receipt);
          return receipt;
        },
      };
    },
);

vi.mock('../harness/bridge', countingBridge);

interface Session {
  install: () => () => void;
  emitQuery: (id: string, spec: unknown) => void;
  listeners: () => number;
  replies: () => unknown[][];
  teardownReceipts: () => unknown[];
}

/**
 * Boots a fresh module graph with the bootstrap bits already decided. The
 * transport mocks have to come from the SAME graph — after `resetModules`
 * the event registry the store subscribes to is a different object than
 * the one this file imported at the top.
 */
async function session(opts: { harness: boolean; remote?: boolean }): Promise<Session> {
  vi.resetModules();
  const runtime = await import('../../test/mocks/wailsio-runtime');
  const bindings = await import('../../test/mocks/bindings-app');
  const runMode = await import('../transport/runMode');
  const harnessMode = await import('../transport/harnessMode');
  const store = await import('./harnessBridge');

  // Manifest order: bootstrap.ts settles the remote bit before the harness
  // bit, so the arm always sees a final locality answer.
  runMode.setViewOnlySessionFromBootstrap(opts.remote === true);
  harnessMode.setHarnessSessionFromBootstrap(opts.harness);

  let pageId = store.__frontendPageIDForTest();
  bindings.setBindingMock('HarnessRegisterPage', (...args: unknown[]) => {
    return { pageId, marker: String(args[1] ?? ''), origin: String(args[2] ?? '') };
  });
  const reply = bindings.setBindingMock('HarnessUIQueryReply', () => null);
  return {
    install: store.installHarnessBridge,
    emitQuery: (id, spec) => runtime.emitWailsEvent('harness:ui-query', { id, spec, pageId }),
    listeners: () => runtime.wailsListenerCount('harness:ui-query'),
    replies: () => reply.mock.calls,
    teardownReceipts: () => chunk.receipts,
  };
}

// The wait below can spend real seconds on a starved worker, and three of
// them in one case. Raised from the 5s default so an honest slow import
// still reports what it was waiting for rather than a bare test timeout.
vi.setConfig({ testTimeout: 20_000 });

/** How long `settle(until)` waits before declaring the chain broken. */
const SETTLE_TIMEOUT_MS = 5_000;

/**
 * Lets the answer chain settle: macrotask hops rather than a microtask
 * drain, because its first link is a dynamic import of a module graph.
 * `until` short-circuits the wait for the cases that have something to
 * wait FOR; the cases asserting that nothing happens spend the full run.
 *
 * A wait that runs out THROWS, and is bounded by the WALL CLOCK rather
 * than by a count of hops. Both halves of that were the 2026-08-30 flake
 * in this file, which failed about one full-suite run in 45:
 *
 *   * A hop count is not a timeout. The event loop spins `setTimeout(0)`
 *     hops happily while the vite transform of the `lib/harness` graph
 *     holds up the import, so the old 500-hop budget expired on a fast
 *     machine's schedule instead of on the work's. Idle, that import
 *     lands in under 3 hops; on a worker starved by the other 600 files
 *     it outran all 500.
 *   * Running out returned NORMALLY, so the case went on to assert
 *     against state that had not arrived — and the activation it was
 *     waiting for then landed inside the NEXT case, past the afterEach
 *     that had just zeroed the counters. One slow import, two failures,
 *     neither naming the wait. Starving the budget to 3 hops reproduces
 *     both, in order, exactly.
 *
 * Only the third case pays that cold import: it is the first to reach the
 * chunk, and vitest caches the mock factory across `resetModules`. It also
 * cannot be pre-warmed, because proving the chunk is NOT loaded before the
 * first query is the whole contract it exists to hold.
 */
async function settle(until?: () => boolean): Promise<void> {
  // A case asserting that NOTHING happens has nothing to poll for, so it
  // spends a small fixed run of hops.
  if (!until) {
    for (let i = 0; i < 25; i += 1) {
      await new Promise((resolve) => setTimeout(resolve, 0));
    }
    return;
  }
  const deadline = Date.now() + SETTLE_TIMEOUT_MS;
  do {
    await new Promise((resolve) => setTimeout(resolve, 0));
    if (until()) return;
  } while (Date.now() < deadline);
  throw new Error(
    `harness bridge chain did not settle within ${SETTLE_TIMEOUT_MS}ms`,
  );
}

afterEach(() => {
  chunk.loads = 0;
  chunk.activations = 0;
  chunk.stops = 0;
  chunk.receipts = [];
  // Undo whatever the failing-load case installed. doUnmock would take the
  // file's own mock down with it, so the counting factory is re-registered
  // rather than removed.
  vi.doMock('../harness/bridge', countingBridge);
});

describe('installHarnessBridge', () => {
  it('does nothing at all in an ordinary session', async () => {
    const s = await session({ harness: false });
    const teardown = s.install();
    s.emitQuery('uq-1', { v: 1, kind: 'viewport' });
    await settle();

    expect(s.listeners()).toBe(0);
    expect(chunk.activations).toBe(0);
    expect(s.replies()).toHaveLength(0);
    teardown();
  });

  // harness:ui-query is an AudienceLoopbackOnly channel, so a LAN browser
  // cannot be sent one. Arming there would subscribe, and eventually load a
  // loopback-only tooling chunk, for a query that can never arrive.
  it('does not arm for a remote page attached to a harness backend', async () => {
    const s = await session({ harness: true, remote: true });
    const teardown = s.install();
    await settle();

    expect(s.listeners()).toBe(0);
    expect(chunk.activations).toBe(0);
    teardown();
  });

  // The soak rig runs for hours specifically to reproduce renderer memory
  // behaviour, and the chunk is where every probe the bridge owns lives: a
  // run that never queries the page must cost nothing but the listener —
  // no fetch, no parse, no evaluation.
  it('subscribes without loading the chunk, and loads it on the first query', async () => {
    const s = await session({ harness: true });
    const teardown = s.install();
    await settle();
    expect(s.listeners()).toBe(1);
    expect(chunk.activations).toBe(0);

    s.emitQuery('uq-1', { v: 1, kind: 'viewport' });
    await settle(() => s.replies().length > 0);
    expect(chunk.activations).toBe(1);
    expect(s.replies()).toHaveLength(1);
    expect(s.replies()[0]![1]).toBe('uq-1');
    expect(s.replies()[0]![2]).toMatchObject({ v: 1 });

    // Memoised: a second query reuses the loaded chunk.
    s.emitQuery('uq-2', { v: 1, kind: 'viewport' });
    await settle(() => s.replies().length > 1);
    expect(chunk.activations).toBe(1);
    expect(s.replies()).toHaveLength(2);
    teardown();
  });

  // The second half of the same promise, and the one a perf run depends
  // on: reaching the chunk is not the same as arming the document-wide
  // MutationObserver. A bench workload's queries are `perf` ops, and a run
  // that measured a renderer carrying an observer production does not have
  // is a run measuring the instrument.
  it('loads the chunk for a perf query without installing the observer', async () => {
    const s = await session({ harness: true });
    const bridge = await import('../harness/bridge');
    const teardown = s.install();
    await settle();

    s.emitQuery('uq-1', { v: 1, kind: 'perf', op: 'start', meters: ['dom'] });
    await settle(() => s.replies().length > 0);
    expect(chunk.activations).toBe(1);
    expect(s.replies()[0]![2]).toMatchObject({ armed: true });
    expect(bridge.mutationClockArmed()).toBe(false);

    // A settledness query is what pays for it, through the same door.
    s.emitQuery('uq-2', { v: 1, kind: 'viewport' });
    await settle(() => s.replies().length > 1);
    expect(bridge.mutationClockArmed()).toBe(true);

    teardown();
    await settle(() => chunk.stops > 0);
    expect(bridge.mutationClockArmed()).toBe(false);
  });

  it('ignores a query event with no id rather than replying to nothing', async () => {
    const s = await session({ harness: true });
    const teardown = s.install();
    s.emitQuery('', { v: 1, kind: 'viewport' });
    await settle();
    expect(chunk.activations).toBe(0);
    expect(s.replies()).toHaveLength(0);
    teardown();
  });

  // The backend registers its waiter before it emits and parks for 10s. A
  // chunk that 404s (a stale index against a new build) must still produce
  // a reply, or every query in that session costs the caller ten seconds
  // to learn nothing.
  it('answers with an error envelope when the chunk itself fails to load', async () => {
    const s = await session({ harness: true });
    // doMock rather than a branch in the hoisted factory: vitest caches the
    // factory's result, so a flag read there would already be moot by the
    // time this case runs.
    vi.doMock('../harness/bridge', () => {
      throw new Error('bridge chunk unavailable');
    });
    const teardown = s.install();
    await settle();
    s.emitQuery('uq-1', { v: 1, kind: 'viewport' });
    await settle(() => s.replies().length > 0);

    expect(s.replies()).toHaveLength(1);
    const [, id, result] = s.replies()[0]!;
    expect(id).toBe('uq-1');
    // The message is the loader's, not ours (under vitest the rejection is
    // wrapped by the mock machinery) — what this pins is that a rejection
    // becomes an {error} the backend can unwrap, rather than silence.
    const error = (result as { error?: unknown }).error;
    expect(typeof error).toBe('string');
    expect(error as string).not.toBe('');
    teardown();
  });

  it('takes the subscription and the bridge down on teardown', async () => {
    const s = await session({ harness: true });
    const teardown = s.install();
    await settle();
    s.emitQuery('uq-1', { v: 1, kind: 'viewport' });
    await settle(() => s.replies().length > 0);
    expect(s.listeners()).toBe(1);

    teardown();
    await settle(() => chunk.stops > 0);
    expect(s.listeners()).toBe(0);
    expect(chunk.stops).toBe(1);

    // Nothing arrives after teardown, and nothing is answered.
    s.emitQuery('uq-2', { v: 1, kind: 'viewport' });
    await settle();
    expect(s.replies()).toHaveLength(1);
  });

  it('delivers partial perf and monitor receipts before unregistering the page', async () => {
    const s = await session({ harness: true });
    const teardown = s.install();
    await settle();

    s.emitQuery('perf-start', { v: 1, kind: 'perf', op: 'start', runId: 'reload-perf', meters: ['dom'] });
    await settle(() => s.replies().length > 0);
    s.emitQuery('monitor-start', {
      v: 1,
      kind: 'monitor',
      op: 'start',
      runId: 'reload-monitor',
      monitorIds: ['semantic-dom-stability'],
      atMs: 1,
    });
    await settle(() => s.replies().length > 1);

    teardown();
    await settle(() => s.teardownReceipts().length > 0);

    expect(s.listeners()).toBe(0);
    expect(s.teardownReceipts()[0]).toMatchObject({
      v: 1,
      kind: 'bridge-teardown',
      reason: 'page-unload',
      partial: true,
      perf: {
        kind: 'perf-teardown',
        runId: 'reload-perf',
        partial: true,
      },
      monitors: [{ runId: 'reload-monitor' }],
      errors: [],
    });
  });

  it('is safe to tear down a session that never armed', async () => {
    const s = await session({ harness: false });
    const teardown = s.install();
    expect(() => teardown()).not.toThrow();
    await settle();
    expect(chunk.stops).toBe(0);
  });
});
