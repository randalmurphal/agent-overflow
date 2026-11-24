import { describe, expect, it } from 'vitest';
import type { ReporterOptions } from 'knip';
import { dropCalledByName } from '../../scripts/knipResetExports';

const ROOT = '/repo/frontend';
const FILE = `${ROOT}/src/lib/stores/example.ts`;

/** The knip findings `names` in FILE, as the preprocessor receives them. */
function findings(names: string[]): ReporterOptions {
  const symbols = Object.fromEntries(names.map((symbol) => [symbol, { type: 'exports', filePath: FILE, workspace: '.', symbol, fixes: [] }]));
  return { issues: { exports: { [FILE]: symbols } }, counters: { exports: names.length } } as unknown as ReporterOptions;
}

describe('dropCalledByName', () => {
  it('exempts exactly the exports the reset transform registers', async () => {
    const source = [
      "export { resetElsewhereForTest } from './elsewhere';",
      'export function resetToForTest(value: number): void {}',
      'export function resetOptionalForTest(value?: number): void {}',
      'export function resetLocalForTest(): void {}',
      'export function count(): number { return 0; }',
    ].join('\n');
    const options = await dropCalledByName(
      findings(['resetElsewhereForTest', 'resetToForTest', 'resetOptionalForTest', 'resetLocalForTest', 'count']),
      ROOT,
      async () => source,
    );
    // A re-export has no local binding and a reset that requires an
    // argument never runs by name, so both stay reported; `value?` loses
    // its `?` when types are stripped, as it does for the transform.
    expect(Object.keys(options.issues.exports[FILE])).toEqual(['resetElsewhereForTest', 'resetToForTest', 'resetOptionalForTest', 'count']);
    expect(options.counters.exports).toBe(4);
  });

  it('exempts the export a between-test step names, whatever its arguments, and not a reset testResets.ts keeps out', async () => {
    const file = `${ROOT}/src/lib/stores/transportStatus.svelte.ts`;
    const names = ['__setTransportStatusForTest', 'resetTransportStatusForTest'];
    const options = {
      counters: { exports: 2 },
      issues: { exports: { [file]: Object.fromEntries(names.map((symbol) => [symbol, { symbol }])) } },
    } as unknown as ReporterOptions;
    await dropCalledByName(options, ROOT, async () => [
      'export function __setTransportStatusForTest(status: unknown): void {}',
      'export function resetTransportStatusForTest(): void {}',
    ].join('\n'));
    expect(Object.keys(options.issues.exports[file])).toEqual(['resetTransportStatusForTest']);
    expect(options.counters.exports).toBe(1);
  });
});
