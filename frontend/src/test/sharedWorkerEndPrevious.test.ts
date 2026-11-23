import { execFile } from 'node:child_process';
import { resolve } from 'node:path';
import { expect, it } from 'vitest';
import './sharedWorkerEndPrevious.fixture.test';

// A child Vitest runs, in one worker, a file whose tests are all skipped and
// then openThreadPanes.test.ts. That file's second test fails when the file
// starts without the skipped file ended: its setup reuses the skipped file's
// module instances, so its own between-test steps never register. About 3 s,
// most of it the child's start.
const FRONTEND = resolve(import.meta.dirname, '../..');
const SKIPPED = 'src/test/sharedWorkerEndPrevious.fixture.test.ts';
const NEXT = 'src/test/openThreadPanes.test.ts';

function runChild(): Promise<{ code: number | null; output: string }> {
  return new Promise((done) => {
    execFile(
      resolve(FRONTEND, 'node_modules/.bin/vitest'),
      // One worker, both files in it. Without the results cache Vitest runs
      // the larger file first.
      ['run', '--project', 'unit', '--reporter', 'verbose', '--maxWorkers', '1', '--no-file-parallelism', '--no-cache', SKIPPED, NEXT],
      { cwd: FRONTEND, env: { ...process.env, NO_COLOR: '1', FORCE_COLOR: '0' }, maxBuffer: 16 * 1024 * 1024 },
      (error, stdout, stderr) => done({ code: error ? (typeof error.code === 'number' ? error.code : 1) : 0, output: `${stdout}\n${stderr}` }),
    );
  });
}

it('ends a file whose tests are all skipped before the next file in its worker starts', async () => {
  const { code, output } = await runChild();
  const skipped = output.indexOf(`${SKIPPED} >`);
  expect(skipped, output).toBeGreaterThanOrEqual(0);
  expect(output.indexOf(`${NEXT} >`), `the child ran ${NEXT} first: it runs the larger file first, so ${SKIPPED} must stay the larger\n${output}`).toBeGreaterThan(skipped);
  expect(code, output).toBe(0);
}, 120_000);
