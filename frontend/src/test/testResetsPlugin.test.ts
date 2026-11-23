import { describe, expect, it } from 'vitest';
import { moduleKey, registration } from './config/testResetsPlugin';
import { RESET_REGISTRY_KEY } from './config/testResets';

const STORE = 'src/lib/stores/example.svelte.ts';

/** Runs a registration line against `registry`, with the module's locals. */
function evaluate(line: string, locals: Record<string, unknown>, registry: unknown): void {
  const key = Symbol.for(RESET_REGISTRY_KEY);
  const holder = globalThis as { [key: symbol]: unknown };
  const installed = holder[key];
  holder[key] = registry;
  try {
    new Function(...Object.keys(locals), line)(...Object.values(locals));
  } finally {
    holder[key] = installed;
  }
}

describe('registration', () => {
  it('runs by name only the reset exports that take no required argument', () => {
    const line = registration(STORE, [
      'export function resetCountForTest() {}',
      'export function resetToForTest(value) {}',
      'export function clearWithDefaultForTest(value = 1, ...rest) {}',
      'const resetLocalForTest = () => {};',
      'export const clearAliasForTest = resetLocalForTest;',
      'export function count() {}',
    ].join('\n'));
    const seen: unknown[] = [];
    evaluate(line!, { resetCountForTest: 1, resetToForTest: 2, clearWithDefaultForTest: 3, clearAliasForTest: 4 }, {
      loaded: (...args: unknown[]) => seen.push(args),
    });
    expect(seen).toEqual([[STORE, { resetCountForTest: 1, clearWithDefaultForTest: 3, clearAliasForTest: 4 }, ['resetCountForTest', 'clearWithDefaultForTest', 'clearAliasForTest']]]);
  });

  it('follows an export list to the local it names', () => {
    const line = registration(STORE, 'function reset() {}\nexport { reset as resetAllForTest };');
    const seen: unknown[] = [];
    evaluate(line!, { reset: 'fn' }, { loaded: (...args: unknown[]) => seen.push(args) });
    expect(seen).toEqual([[STORE, { resetAllForTest: 'fn' }, ['resetAllForTest']]]);
  });

  it('fails loudly when the registry is not installed', () => {
    const line = registration(STORE, 'export function resetCountForTest() {}');
    expect(() => evaluate(line!, { resetCountForTest: 1 }, undefined)).toThrow(
      `the test reset registry is not installed: the setup file must import src/test/resetRegistry.ts before ${STORE} loads`,
    );
  });

  it('rejects a reset export whose arguments it cannot see', () => {
    expect(() => registration(STORE, 'export const resetCountForTest = makeReset();')).toThrow(
      'cannot tell whether resetCountForTest takes an argument: declare it as a function',
    );
  });

  it('rejects an export testResets.ts names that the module does not have', () => {
    expect(() => registration('src/test/helpers/backends.ts', 'export function stageBackend() {}')).toThrow(
      'testResets.ts names resetStagedBackends in src/test/helpers/backends.ts, which does not export it',
    );
  });

  it('passes only the exports a step calls and the resets that run by name', () => {
    const key = 'src/lib/stores/transportStatus.svelte.ts';
    const line = registration(key, [
      'export function __setTransportStatusForTest(status) {}',
      'export function resetTransportStatusForTest() {}',
      'export function resetOtherForTest() {}',
      'export function transportStatus() {}',
    ].join('\n'));
    const seen: unknown[] = [];
    evaluate(line!, { __setTransportStatusForTest: 1, resetTransportStatusForTest: 2, resetOtherForTest: 3, transportStatus: 4 }, {
      loaded: (...args: unknown[]) => seen.push(args),
    });
    // The step's export, and the reset that runs by name; not the reset
    // listed as not run between tests, nor the module's other exports.
    expect(seen).toEqual([[key, { resetOtherForTest: 3, __setTransportStatusForTest: 1 }, ['resetOtherForTest']]]);
  });

  it('rejects a reset that calls another module\'s reset', () => {
    const code = [
      "import { count, resetForTest as resetLeafForTest } from './leaf';",
      'export function resetCountForTest() { count(); resetLeafForTest(); }',
    ].join('\n');
    expect(() => registration(STORE, code)).toThrow(
      `${STORE}: resetCountForTest calls resetLeafForTest from ./leaf; the registry runs that module's reset itself, so a reset resets its own module only`,
    );
    expect(registration(STORE, "import { count } from './leaf';\nexport function resetCountForTest() { count(); }")).not.toBeNull();
  });

  it('leaves a module without resets untouched', () => {
    expect(registration(STORE, 'export function count() {}')).toBeNull();
  });
});

describe('moduleKey', () => {
  const root = '/repo/frontend';
  it('names source modules from frontend/ and packages from node_modules/', () => {
    expect(moduleKey(root, '/repo/frontend/src/lib/stores/a.svelte.ts')).toBe('src/lib/stores/a.svelte.ts');
    expect(moduleKey(root, '/repo/frontend/node_modules/.pnpm/x@1/node_modules/@scope/x/src/pure.js')).toBe('@scope/x/src/pure.js');
  });

  it('never registers a test file or a file outside src/', () => {
    expect(moduleKey(root, '/repo/frontend/src/lib/a.test.ts')).toBeNull();
    expect(moduleKey(root, '/repo/frontend/src/lib/a.browser.test.ts')).toBeNull();
    expect(moduleKey(root, '/repo/frontend/vitest.config.ts')).toBeNull();
  });
});
