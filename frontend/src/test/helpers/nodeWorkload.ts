// A workload run in a bare `node --expose-gc` process, for a measurement
// the test worker's own heap is too noisy for (a whole-heap delta). The
// test waits on the child while it works, so the child's run time is
// credited to the real-time budget (src/test/realTimeBudget.ts) as work.
import { execFileSync } from 'node:child_process';
import { creditChildProcessTime } from '../realTimeBudget';

// Captured at import: fake timers can replace process.hrtime.
const wallNs = process.hrtime.bigint.bind(process.hrtime);

/** Runs the ES module `source` in a fresh `node --expose-gc` with `env`
 * added, and returns what it wrote to stdout. */
export function runNodeWorkload(source: string, env: Record<string, string>): string {
  const started = wallNs();
  try {
    return execFileSync(
      process.execPath,
      ['--expose-gc', '--input-type=module', '--eval', source],
      { encoding: 'utf8', env: { ...process.env, ...env }, timeout: 120_000 },
    );
  } finally {
    creditChildProcessTime(Number(wallNs() - started) / 1e6);
  }
}
