// What the fixture files of sharedWorkerRunner.test.ts share across the
// files of their worker: a worker global, since each file evaluates its
// own instance of this module.
import { BroadcastChannel } from 'node:worker_threads';
export type FixtureState = {
  /** Calls of each file's 'ao-fixture' listener. */
  readonly hits: Record<string, number>;
  /** Opens the gate lateModule.ts waits on. */
  openGate(): void;
  readonly gate: Promise<void>;
  lateEvaluations: number;
  lateResets: number;
};

const KEY = Symbol.for('agent-overflow.test.sharedWorkerFixtures');

export function fixtureState(): FixtureState {
  const worker = globalThis as { [KEY]?: FixtureState };
  if (worker[KEY] === undefined) {
    let openGate = () => {};
    const gate = new Promise<void>((resolve) => {
      openGate = resolve;
    });
    worker[KEY] = { hits: {}, openGate, gate, lateEvaluations: 0, lateResets: 0 };
  }
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

const DONE_CHANNEL = 'agent-overflow.test.sharedWorkerFixtures.done';

/** Resolves once the last fixture calls `fixturesFinished`, in either
 * worker of the run. */
export function fixturesDone(): Promise<void> {
  const channel = new BroadcastChannel(DONE_CHANNEL);
  return new Promise((resolve) => {
    channel.onmessage = () => {
      channel.close();
      resolve();
    };
  });
}

export function fixturesFinished(): void {
  const channel = new BroadcastChannel(DONE_CHANNEL);
  channel.postMessage('done');
  channel.close();
}
