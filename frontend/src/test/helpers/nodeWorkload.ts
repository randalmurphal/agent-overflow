// A workload run in a bare `node --expose-gc` process, for a measurement
// the test worker's own heap is too noisy for (a whole-heap delta).
import { execFileSync } from 'node:child_process';

/** Runs the ES module `source` in a fresh `node --expose-gc` with `env`
 * added, and returns what it wrote to stdout. */
export function runNodeWorkload(source: string, env: Record<string, string>): string {
  return execFileSync(
    process.execPath,
    ['--expose-gc', '--input-type=module', '--eval', source],
    { encoding: 'utf8', env: { ...process.env, ...env }, timeout: 120_000 },
  );
}
