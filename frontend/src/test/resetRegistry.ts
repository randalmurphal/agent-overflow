// Runs the between-test steps of config/testResets.ts for the modules a test
// file loaded. Each module registers itself as it loads (the registration
// config/testResetsPlugin.ts appends to its test transform), so the setup
// file imports no app module. Import this first in a setup file: a module
// that loads before it fails its registration.
import { afterEach, beforeEach } from 'vitest';
import { FIRST, POSTURE, RESET_REGISTRY_KEY, STORAGE, type Exports, type Step } from './config/testResets';

type Loaded = { readonly key: string; readonly exports: Exports; readonly auto: readonly string[] };

export type ResetRegistry = {
  loaded(key: string, exports: Exports, auto: readonly string[]): void;
  /** A source module with nothing to register finished loading. */
  settled(): void;
  /** Keys of the modules this file loaded, in load order. */
  keys(): string[];
};

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
  // vi.resetModules) is a second instance, and both are reset: the test
  // may still hold the first.
  const modules: Loaded[] = [];
  // Set from the first step before a test to the last step after it.
  let inTest = false;
  // POSTURE steps of modules loaded during a test, in load order, that have
  // not run: a step that reads a module still loading (an import cycle)
  // waits for the next module to load.
  let pending: { readonly loaded: Loaded; readonly step: Step }[] = [];

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

  beforeEach(async () => {
    pending = [];
    const errors: unknown[] = [];
    await runSteps(POSTURE, errors);
    fail(errors);
    inTest = true;
  });

  afterEach(async () => {
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
    fail(errors);
  });

  return {
    loaded(key, exports, auto) {
      const loaded = { key, exports, auto };
      modules.push(loaded);
      if (!inTest) return;
      for (const step of POSTURE) if (step.module === key) pending.push({ loaded, step });
      runPending();
    },
    settled() {
      if (inTest) runPending();
    },
    keys: () => [...new Set(modules.map((loaded) => loaded.key))],
  };
}

export const resetRegistry: ResetRegistry = install();
(globalThis as { [key: symbol]: ResetRegistry })[Symbol.for(RESET_REGISTRY_KEY)] = resetRegistry;
