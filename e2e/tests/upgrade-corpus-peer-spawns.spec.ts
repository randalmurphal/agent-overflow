// The upgrade corpus top-up's paired-spawn step (writePeerSpawns in
// upgrade-corpus-topup-records.ts) waits for its workers' turns, not only
// for their receipts. A run kept open is recorded when the spawn or the
// nudge is taken, so the receipts the step polls read as written while the
// worker's turn still runs. The spec holds the Codex worker's spawn turn,
// then the nudged worker's nudge turn, at a mock gate after its run opened,
// and requires the step to be waiting on that worker's turn and not to have
// returned or gone on; released, the step returns with every thread it
// made at rest.
//
// The step runs on a backend of the spec's own, not the worker's: it pairs
// that backend as an own device of a peer, which connects it back to the
// peer for good, and a reset keeps pairings. On the worker's backend every
// later page would list a computer that never answers.
import { randomBytes } from 'node:crypto';
import * as path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test } from './fixtures.js';
import { writePeerSpawns } from './upgrade-corpus-topup-records.js';
import { assertAtRest, storeThreads } from './upgrade-corpus-topup.js';

const HELD_TIMEOUT_MS = 180_000;

interface MockEvent {
  mockId: string;
  report: { kind: string; detail?: string };
}

/**
 * `harness` with every provider:turn_completed wait made through it
 * recorded, so the spec can tell that the step is waiting on one thread's
 * turn: waitingOn resolves once a wait whose predicate matches the thread
 * is registered and has not resolved.
 */
function watchTurnWaits(harness: HarnessApp): { harness: HarnessApp; waitingOn: (threadId: string) => Promise<'waiting'> } {
  const waits: Array<{ matches: (data: { threadId: string }) => boolean; settled: boolean }> = [];
  const listeners = new Set<() => void>();
  const watched = new Proxy(harness, {
    get(target, prop) {
      if (prop === 'waitForEvent') {
        return <T>(channel: string, predicate?: (data: T) => boolean, timeoutMs?: number): Promise<T> => {
          const promise = target.waitForEvent<T>(channel, predicate, timeoutMs);
          if (channel === 'provider:turn_completed' && predicate) {
            const wait = { matches: predicate as unknown as (data: { threadId: string }) => boolean, settled: false };
            waits.push(wait);
            const settle = () => { wait.settled = true; };
            promise.then(settle, settle);
            for (const listener of listeners) listener();
          }
          return promise;
        };
      }
      const value: unknown = Reflect.get(target, prop, target);
      return typeof value === 'function' ? (value as (...args: unknown[]) => unknown).bind(target) : value;
    },
  });
  const pending = (threadId: string) => waits.some((wait) => !wait.settled && wait.matches({ threadId }));
  const waitingOn = (threadId: string) => new Promise<'waiting'>((resolve) => {
    if (pending(threadId)) return resolve('waiting');
    const listener = () => {
      if (!pending(threadId)) return;
      listeners.delete(listener);
      resolve('waiting');
    };
    listeners.add(listener);
  });
  return { harness: watched, waitingOn };
}

function threadByTitle(harness: HarnessApp, title: string): string {
  const database = new DatabaseSync(path.join(harness.bootstrap.dataDir, 'agent-overflow.db'), { readOnly: true });
  try {
    const row = database.prepare('SELECT id FROM threads WHERE title = ?').get(title) as { id: string } | undefined;
    if (!row) throw new Error(`no thread is titled ${title}`);
    return row.id;
  } finally {
    database.close();
  }
}

test('the paired-spawn step returns only once the turns it held for open runs have ended', async () => {
  test.setTimeout(6 * 60_000);
  const harness = await launchHarness();
  try {
    const nonce = randomBytes(3).toString('hex');
    const version = `0.0.0-peer${nonce}`;
    const hold = { spawn: `peer-spawn-${nonce}`, nudge: `peer-nudge-${nonce}` };
    const inherited = storeThreads(harness);
    const held = (gate: string) => harness.waitForEvent<MockEvent>('harness:mock',
      (ev) => ev.report.kind === 'waiting_signal' && ev.report.detail === gate, HELD_TIMEOUT_MS);
    const spawnHeld = held(hold.spawn);
    const nudgeHeld = held(hold.nudge);
    const watched = watchTurnWaits(harness);
    let returned = false;
    const run = writePeerSpawns(watched.harness, version, nonce, hold).finally(() => { returned = true; });
    const returnedFirst = run.then(() => 'returned' as const);
    run.catch(() => undefined);

    const spawn = await Promise.race([spawnHeld, run.then(() => undefined)]);
    if (!spawn) throw new Error('writePeerSpawns returned before the Codex worker reached its gate');
    const codex = threadByTitle(harness, `Top-up v${version}: a spawn whose run stays open`);
    expect(await Promise.race([watched.waitingOn(codex), returnedFirst, nudgeHeld.then(() => 'went on to the nudges' as const)]),
      "writePeerSpawns did not wait for the Codex worker's held spawn turn").toBe('waiting');
    expect(returned).toBe(false);
    await harness.rpc('HarnessMockCommand', spawn.mockId, { type: 'advance', name: hold.spawn });

    const nudge = await Promise.race([nudgeHeld, run.then(() => undefined)]);
    if (!nudge) throw new Error('writePeerSpawns returned before the nudged worker reached its gate');
    const nudged = threadByTitle(harness, `Top-up v${version}: a spawn the person nudges`);
    expect(await Promise.race([watched.waitingOn(nudged), returnedFirst]),
      "writePeerSpawns did not wait for the nudged worker's held turn").toBe('waiting');
    expect(returned).toBe(false);
    await harness.rpc('HarnessMockCommand', nudge.mockId, { type: 'advance', name: hold.nudge });

    const result = await run;
    expect(result.threadIds).toEqual(expect.arrayContaining([codex, nudged]));
    await assertAtRest(harness, 'writePeerSpawns', inherited);
  } finally {
    await harness.close();
  }
});
