// What the fixture files of sharedWorkerRunner.test.ts share across the
// files of their worker: a worker global, since each file evaluates its
// own instance of this module.
// oxlint-disable-next-line no-restricted-imports -- the run's own temporary marker file, not a repository file
import { existsSync, writeFileSync } from 'node:fs';

export type FixtureState = {
  /** Calls of each file's 'ao-fixture' listener. */
  readonly hits: Record<string, number>;
  readonly lateGate: Gate;
  readonly rejectGate: Gate;
  lateEvaluations: number;
  lateResets: number;
  teardownEvaluations: number;
};

type Gate = { readonly opened: Promise<void>; open(): void };

const KEY = Symbol.for('agent-overflow.test.sharedWorkerFixtures');

function gate(): Gate {
  let open = () => {};
  const opened = new Promise<void>((resolve) => {
    open = resolve;
  });
  return { opened, open };
}

export function fixtureState(): FixtureState {
  const worker = globalThis as { [KEY]?: FixtureState };
  worker[KEY] ??= { hits: {}, lateGate: gate(), rejectGate: gate(), lateEvaluations: 0, lateResets: 0, teardownEvaluations: 0 };
  return worker[KEY];
}

/** Adds an 'ao-fixture' listener on the window that counts under `name`. */
export function listen(name: string): void {
  const { hits } = fixtureState();
  hits[name] = 0;
  window.addEventListener('ao-fixture', () => {
    hits[name] = (hits[name] ?? 0) + 1;
  });
}

/** Fires 'ao-fixture' on the window and returns each listener's calls. */
export function fire(): Record<string, number> {
  window.dispatchEvent(new Event('ao-fixture'));
  return { ...fixtureState().hits };
}

/** Adds a listener under `name`, fires, and checks that it heard: a file's
 * own listener and timer work in a worker that already ran files. */
export async function expectOwnListenerAndTimer(name: string): Promise<Record<string, number>> {
  listen(name);
  const hits = fire();
  if (hits[name] !== 1) throw new Error(`${name}'s own listener heard ${hits[name]} events, want 1`);
  await new Promise((resolve) => setTimeout(resolve, 0));
  return hits;
}

/** The environment variable that names the file the last fixture creates;
 * fixtures.workload.ts sets it for the run. A file, because the pool stops
 * the last fixture's worker once it is done, before a late holder asks. */
export const FIXTURES_DONE_ENV = 'AO_SHARED_WORKER_FIXTURES_DONE';

function doneMarker(): string {
  const path = process.env[FIXTURES_DONE_ENV];
  if (path === undefined || path === '') throw new Error(`${FIXTURES_DONE_ENV} is unset: run the fixtures through fixtures.workload.ts`);
  return path;
}

/** Resolves once the last fixture has called `fixturesFinished`, in the
 * other worker of the run, whether that came before or after this call. */
export async function fixturesDone(): Promise<void> {
  const marker = doneMarker();
  while (!existsSync(marker)) await new Promise((resolve) => setTimeout(resolve, 20));
}

/** Tells the holding fixture the run is done, now and to a late asker. */
export function fixturesFinished(): void {
  writeFileSync(doneMarker(), '');
}
