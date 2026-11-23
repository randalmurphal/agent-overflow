// Runs the between-test steps of config/testResets.ts for the modules a test
// file loaded. Each module registers itself as it loads (the registration
// config/testResetsPlugin.ts appends to its test transform), so the setup
// file imports no app module. Import this first in a setup file: a module
// that loads before it fails its registration.
//
// It also fails the test that called a binding with no mock installed
// (mocks/bindings-app.ts records each such call here). A call is charged to
// the test running when it was made, or, made between tests, to the test
// that ended last; the first check after it fails: the end of that test, the
// start of the next, or the end of the file. The record lives on the
// worker's global, so a call a file's modules make after the file ended
// fails the next file run in the same worker. A call made after the last
// file of a worker, or after a file with a context of its own (unit-isolated,
// browser) ended, has no later check to fail.
import { afterAll, afterEach, beforeEach } from 'vitest';
import { FIRST, POSTURE, RESET_REGISTRY_KEY, STORAGE, type Exports, type Step } from './config/testResets';

type Loaded = {
  readonly key: string;
  readonly exports: Exports;
  readonly auto: readonly string[];
  /** Loaded outside a test: by the file's imports or a beforeAll hook. */
  readonly held: boolean;
};

export type ResetRegistry = {
  loaded(key: string, exports: Exports, auto: readonly string[]): void;
  /** A source module with nothing to register finished loading. */
  settled(): void;
  /** Keys of the modules this file loaded, in load order. */
  keys(): string[];
  /** A binding was called with no mock installed. */
  unansweredBinding(name: string): void;
  /** The unanswered calls recorded since the last check, each with the
   * test it is charged to, and clears them. */
  takeUnansweredBindings(): string[];
};

type UnansweredCall = { readonly binding: string; readonly during: string };

// One list per worker global, shared by every file's registry instance.
const UNANSWERED = Symbol.for('agent-overflow.test.unansweredBindings');

function unansweredCalls(): UnansweredCall[] {
  const holder = globalThis as { [UNANSWERED]?: UnansweredCall[] };
  return (holder[UNANSWERED] ??= []);
}

function unansweredError(calls: readonly string[]): Error | null {
  if (calls.length === 0) return null;
  return new Error(
    `${calls.length} binding call${calls.length === 1 ? '' : 's'} had no mock installed:\n  ${calls.join('\n  ')}\n` +
    'Install an answer with setBindingMock in the test. A test that needs a binding absent installs a mock that rejects with the error the backend gives.',
  );
}

function call(entry: Step, exports: Exports): unknown {
  const fn = exports[entry.name];
  if (typeof fn !== 'function') throw new Error(`${entry.module} exports ${entry.name}, which is not a function`);
  return entry.run === undefined ? fn() : (entry.run as (fn: unknown) => unknown)(fn);
}

function wipeStorage(): void {
  localStorage.clear();
  sessionStorage.clear();
}

function fail(errors: unknown[]): void {
  if (errors.length === 1) throw errors[0];
  if (errors.length > 1) throw new AggregateError(errors, `${errors.length} between-test steps failed`);
}

function install(): ResetRegistry {
  // In load order. A module evaluated again in the same file (after
  // vi.resetModules) is a second instance. An instance loaded outside a test
  // is reset after every test, since the file holds it. Of the instances
  // tests loaded, each is reset after the test that loaded it, and then only
  // the newest of each module is kept: a test that holds an older one
  // across later tests is not supported.
  let modules: Loaded[] = [];
  // Set from the first step before a test to the last step after it.
  let inTest = false;
  // POSTURE steps of modules loaded during a test, in load order, that have
  // not run: a step that reads a module still loading (an import cycle)
  // waits for the next module to load.
  let pending: { readonly loaded: Loaded; readonly step: Step }[] = [];
  // Whom an unanswered binding call made now is charged to.
  let during = 'while the test file loaded';
  let file = 'its test file';
  const unanswered = unansweredCalls();

  function takeUnanswered(): string[] {
    return unanswered.splice(0).map(({ binding, during: at }) => `${binding}, ${at}`);
  }

  async function runSteps(steps: readonly (Step | typeof STORAGE)[], errors: unknown[]): Promise<void> {
    for (const entry of steps) {
      if (entry === STORAGE) {
        wipeStorage();
        continue;
      }
      for (const loaded of modules) {
        if (loaded.key !== entry.module) continue;
        try {
          await call(entry, loaded.exports);
        } catch (error) {
          errors.push(error);
        }
      }
    }
  }

  function runPending(): void {
    while (pending.length > 0) {
      const { loaded, step } = pending[0];
      try {
        call(step, loaded.exports);
      } catch (error) {
        if (error instanceof ReferenceError) return;
        throw error;
      }
      pending.shift();
    }
  }

  beforeEach(async ({ task }) => {
    const earlier = unansweredError(takeUnanswered());
    if (earlier) throw earlier;
    file = task.file.name;
    during = `in "${task.fullTestName}" (${file})`;
    pending = [];
    const errors: unknown[] = [];
    await runSteps(POSTURE, errors);
    fail(errors);
    inTest = true;
  });

  afterEach(async ({ task }) => {
    inTest = false;
    const errors: unknown[] = pending.map(({ loaded, step }) => new Error(
      `${loaded.key} loaded during the test, and its posture step ${step.name} could not run before the test ended: it read a module that was still loading`,
    ));
    pending = [];
    await runSteps(FIRST, errors);
    const ran = new Set<unknown>();
    for (const { exports, auto } of [...modules]) {
      for (const name of auto) {
        const fn = exports[name] as () => unknown;
        if (ran.has(fn)) continue;
        ran.add(fn);
        try {
          await fn();
        } catch (error) {
          errors.push(error);
        }
      }
    }
    const newest = new Map<string, Loaded>();
    for (const loaded of modules) if (!loaded.held) newest.set(loaded.key, loaded);
    modules = modules.filter((loaded) => loaded.held || newest.get(loaded.key) === loaded);
    const calls = unansweredError(takeUnanswered());
    if (calls) errors.push(calls);
    during = `after "${task.fullTestName}" (${task.file.name}) ended`;
    fail(errors);
  });

  afterAll(() => {
    const calls = unansweredError(takeUnanswered());
    during = `after ${file} ended`;
    if (calls) throw calls;
  });

  return {
    loaded(key, exports, auto) {
      const loaded = { key, exports, auto, held: !inTest };
      modules.push(loaded);
      if (!inTest) return;
      for (const step of POSTURE) if (step.module === key) pending.push({ loaded, step });
      runPending();
    },
    settled() {
      if (inTest) runPending();
    },
    keys: () => [...new Set(modules.map((loaded) => loaded.key))],
    unansweredBinding(name) {
      unanswered.push({ binding: name, during });
    },
    takeUnansweredBindings: takeUnanswered,
  };
}

export const resetRegistry: ResetRegistry = install();
(globalThis as { [key: symbol]: ResetRegistry })[Symbol.for(RESET_REGISTRY_KEY)] = resetRegistry;
