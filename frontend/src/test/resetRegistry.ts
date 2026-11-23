// Runs the between-test steps of config/testResets.ts for the modules a test
// file loaded. Each module registers itself as it loads (the registration
// config/testResetsPlugin.ts appends to its test transform), so the setup
// file imports no app module. Import this first in a setup file: a module
// that loads before it fails its registration.
//
// Every module instance a file uses is registered with that file's
// registry. Work a file started can outlive it and load a module after the
// file ended (a lazy chunk still in flight); that instance is not this
// file's, so an ended registry holds it aside. In the `unit` project the
// next file's first setup file (sharedWorkerEndPrevious.ts) ends this
// registry, dropping what it held, and resets every module. A module that
// loads after that reset and before the next registry installs is the next
// file's instance, and the next registry adopts it as it installs.
//
// It also fails the test that called a binding with no mock installed
// (mocks/bindings-app.ts records each such call here). A call is charged to
// the test running when it was made, or, made between tests, to the test
// that ended last. The check at the end of each test and of each file fails
// on every call recorded since the last check: a call made between tests
// fails the next test. The record lives on the
// worker's global, so a call a file's modules make after the file ended
// fails the next file run in the same worker. A call made after the last
// file of a worker, or after a file with a context of its own (unit-isolated,
// browser) ended, has no later check to fail.
import { afterAll, afterEach, beforeEach } from 'vitest';
import { FIRST, POSTURE, RESET_REGISTRY_KEY, STORAGE, type Exports, type Step } from './config/testResets';

// A module instance, held weakly: through its exported functions, which
// stay reachable while anything references the instance. An instance
// something still references is reset after every test; one nothing
// references is dropped once it is collected.
type Loaded = {
  readonly key: string;
  readonly exports: ReadonlyMap<string, WeakRef<WeakKey>>;
  readonly auto: readonly string[];
};

/** The instance's exports, or null once it has been collected. */
function exportsOf(loaded: Loaded): Exports | null {
  const out: Exports = {};
  for (const [name, ref] of loaded.exports) {
    const value = ref.deref();
    if (value === undefined) return null;
    out[name] = value;
  }
  return out;
}

function weakly(exports: Exports): Map<string, WeakRef<WeakKey>> {
  const held = new Map<string, WeakRef<WeakKey>>();
  for (const [name, value] of Object.entries(exports)) {
    if (typeof value !== 'function') throw new Error(`a registered export must be a function: ${name} is ${typeof value}`);
    held.set(name, new WeakRef(value));
  }
  return held;
}

export type ResetRegistry = {
  loaded(key: string, exports: Exports, auto: readonly string[]): void;
  /** A source module with nothing to register finished loading. */
  settled(): void;
  /** This file is over: a module that loads from now on is held aside for
   *  the next registry, and the ones held so far are dropped (the module
   *  reset that follows discards those instances). */
  end(): void;
  /** The modules that loaded after this file ended, which the next
   *  registry adopts. */
  takeLate(): Array<readonly [string, Exports, readonly string[]]>;
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
  // vi.resetModules) is a second instance.
  let modules: Loaded[] = [];
  // POSTURE steps of modules that loaded, in load order, that have not run:
  // a step that reads a module still loading (an import cycle) waits for
  // the next module to load. Steps waiting when a test starts run with the
  // test's own POSTURE.
  let pending: { readonly loaded: Loaded; readonly step: Step }[] = [];
  // Whom an unanswered binding call made now is charged to.
  let during = 'while the test file loaded';
  let ended = false;
  let late: Array<readonly [string, Exports, readonly string[]]> = [];
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
        const exports = exportsOf(loaded);
        if (exports === null) continue;
        try {
          await call(entry, exports);
        } catch (error) {
          errors.push(error);
        }
      }
    }
  }

  function runPending(): void {
    while (pending.length > 0) {
      const { loaded, step } = pending[0];
      const exports = exportsOf(loaded);
      try {
        if (exports !== null) call(step, exports);
      } catch (error) {
        if (error instanceof ReferenceError) return;
        throw error;
      }
      pending.shift();
    }
  }

  beforeEach(async ({ task }) => {
    file = task.file.name;
    during = `in "${task.fullTestName}" (${file})`;
    pending = [];
    const errors: unknown[] = [];
    await runSteps(POSTURE, errors);
    fail(errors);
  });

  afterEach(async ({ task }) => {
    const errors: unknown[] = pending.map(({ loaded, step }) => new Error(
      `${loaded.key} loaded during the test, and its posture step ${step.name} could not run before the test ended: it read a module that was still loading`,
    ));
    pending = [];
    await runSteps(FIRST, errors);
    const ran = new Set<unknown>();
    // A copy: a reset may load a module.
    for (const loaded of modules.slice()) {
      const exports = exportsOf(loaded);
      if (exports === null) continue;
      for (const name of loaded.auto) {
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
    modules = modules.filter((loaded) => exportsOf(loaded) !== null);
    const calls = unansweredError(takeUnanswered());
    if (calls) errors.push(calls);
    during = `after "${task.fullTestName}" (${task.file.name}) ended`;
    fail(errors);
  });

  afterAll(() => {
    const calls = unansweredError(takeUnanswered());
    during = `after ${file} ended`;
    ended = true;
    if (calls) throw calls;
  });

  return {
    loaded(key, exports, auto) {
      if (ended) {
        late.push([key, exports, auto]);
        return;
      }
      const loaded = { key, exports: weakly(exports), auto };
      modules.push(loaded);
      for (const step of POSTURE) if (step.module === key) pending.push({ loaded, step });
      runPending();
    },
    settled() {
      if (!ended) runPending();
    },
    end() {
      ended = true;
      late = [];
    },
    takeLate() {
      const taken = late;
      late = [];
      return taken;
    },
    keys: () => [...new Set(modules.filter((loaded) => exportsOf(loaded) !== null).map((loaded) => loaded.key))],
    unansweredBinding(name) {
      unanswered.push({ binding: name, during });
    },
    takeUnansweredBindings: takeUnanswered,
  };
}

const worker = globalThis as { [key: symbol]: ResetRegistry | undefined };
const previous = worker[Symbol.for(RESET_REGISTRY_KEY)];
export const resetRegistry: ResetRegistry = install();
worker[Symbol.for(RESET_REGISTRY_KEY)] = resetRegistry;
for (const [key, exports, auto] of previous?.takeLate() ?? []) resetRegistry.loaded(key, exports, auto);
