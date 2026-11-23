import { execFile } from 'node:child_process';
import { resolve } from 'node:path';
import { expect, it } from 'vitest';
import './sharedWorkerEndPrevious.fixture.test';
import './sharedWorkerLateImport.fixture.test';

// A child Vitest runs, in one worker, a file that leaves the next file in
// its worker with module instances it did not register, and then
// openThreadPanes.test.ts. That file's second test fails when its setup
// reuses such an instance: the instance's between-test step belongs to
// another file's reset registry and never runs in this one. About 3 s per
// case, most of it the child's start.
const FRONTEND = resolve(import.meta.dirname, '../..');
const NEXT = 'src/test/openThreadPanes.test.ts';

function runChild(first: string, env: Record<string, string> = {}): Promise<{ code: number | null; output: string }> {
  return new Promise((done) => {
    execFile(
      resolve(FRONTEND, 'node_modules/.bin/vitest'),
      // One worker, both files in it. Without the results cache Vitest runs
      // the larger file first.
      ['run', '--project', 'unit', '--reporter', 'verbose', '--maxWorkers', '1', '--no-file-parallelism', '--no-cache', first, NEXT],
      { cwd: FRONTEND, env: { ...process.env, ...env, NO_COLOR: '1', FORCE_COLOR: '0' }, maxBuffer: 16 * 1024 * 1024 },
      (error, stdout, stderr) => done({ code: error ? (typeof error.code === 'number' ? error.code : 1) : 0, output: `${stdout}\n${stderr}` }),
    );
  });
}

async function expectNextFileClean(first: string, env?: Record<string, string>): Promise<void> {
  const { code, output } = await runChild(first, env);
  const firstAt = output.indexOf(`${first} >`);
  expect(firstAt, output).toBeGreaterThanOrEqual(0);
  expect(output.indexOf(`${NEXT} >`), `the child ran ${NEXT} first: it runs the larger file first, so ${first} must stay the larger\n${output}`).toBeGreaterThan(firstAt);
  expect(code, output).toBe(0);
}

// Vitest runs no afterAll in such a file, so its own end step never runs.
it('ends a file whose tests are all skipped before the next file in its worker starts', async () => {
  await expectNextFileClean('src/test/sharedWorkerEndPrevious.fixture.test.ts');
}, 120_000);

// Work the previous file left running loads a module after its end.
it('gives the next file the modules that work left running loads after the previous file ended', async () => {
  await expectNextFileClean('src/test/sharedWorkerLateImport.fixture.test.ts', { AO_LATE_IMPORT_FIXTURE: '1' });
}, 120_000);
