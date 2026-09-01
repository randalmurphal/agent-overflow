// The sync gate for `bindings-app.ts`, the fake behind the generated
// `bindings/agent-overflow/app.js` module.
//
// The unit (happy-dom) project resolves a missing named export to
// `undefined`, so a binding added to the app without a matching mock export
// breaks nothing in any gated suite — the strict ESM loader that catches it
// lives only in the `browser` vitest project, which the gates don't run.
// ForgetTailnetNode drifted exactly that way for four waves. This test moves
// the check into the gated suite: the mock must export every function the
// generated module exports (superset rule, stated at the block comment in
// the mock), and must export nothing the generated module lost, so a deleted
// binding takes its mock line with it.
import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

import { SRC_ROOT } from '../sourceScan';
import * as mockBindings from './bindings-app';

// Exports that are the mock's own machinery, not fakes of generated bindings.
const mockMachinery = new Set([
  'setBindingMock',
  'getBindingMock',
  'resetBindingMocks',
  '__bindingMocksInternal',
]);

function generatedFunctionNames(): Set<string> {
  const path = resolve(SRC_ROOT, '..', 'bindings', 'agent-overflow', 'app.ts');
  const source = readFileSync(path, 'utf8');
  // The generator emits every binding as a top-level `export function Name(`.
  return new Set(
    [...source.matchAll(/^export function (\w+)\(/gm)].map((m) => m[1]),
  );
}

describe('bindings-app mock stays in sync with the generated bindings', () => {
  const generated = generatedFunctionNames();
  const mocked = new Set(
    Object.entries(mockBindings)
      .filter(([name]) => !mockMachinery.has(name))
      // Class exports are model-shape mirrors (TerminalOpenOptions,
      // WSLDistro, …) that tests `new` against, not RPC fakes — the
      // generated counterparts live in models.ts files, not app.ts.
      .filter(
        ([, value]) =>
          !(
            typeof value === 'function' &&
            value.toString().startsWith('class')
          ),
      )
      .map(([name]) => name),
  );

  it('parses the generated module', () => {
    expect(generated.size).toBeGreaterThan(100);
  });

  it('mocks every generated binding', () => {
    const missing = [...generated].filter((name) => !mocked.has(name)).sort();
    expect(missing, 'add these to bindings-app.ts').toEqual([]);
  });

  it('exports no binding the generated module lost', () => {
    const vanished = [...mocked].filter((name) => !generated.has(name)).sort();
    expect(vanished, 'remove these from bindings-app.ts').toEqual([]);
  });
});
