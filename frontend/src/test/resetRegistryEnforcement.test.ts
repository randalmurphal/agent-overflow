import { execFile } from 'node:child_process';
import { resolve } from 'node:path';
import { expect, it } from 'vitest';
import './resetRegistryEnforcement.fixture.test';

// The registry's checks, run as Vitest runs them: a child Vitest runs
// the fixture, whose cases make binding calls with no mock that the code
// under test catches, and each check fails the run. About 2 s, most of it
// the child's start.
const FRONTEND = resolve(import.meta.dirname, '../..');
const FIXTURE = 'src/test/resetRegistryEnforcement.fixture.test.ts';

function runFixture(): Promise<{ code: number | null; output: string }> {
  return new Promise((done) => {
    execFile(
      resolve(FRONTEND, 'node_modules/.bin/vitest'),
      ['run', '--project', 'unit', '--reporter', 'verbose', FIXTURE],
      { cwd: FRONTEND, env: { ...process.env, AO_REGISTRY_ENFORCEMENT_FIXTURE: '1', NO_COLOR: '1', FORCE_COLOR: '0' }, maxBuffer: 16 * 1024 * 1024 },
      (error, stdout, stderr) => done({ code: error ? (typeof error.code === 'number' ? error.code : 1) : 0, output: `${stdout}\n${stderr}` }),
    );
  });
}

it('fails the test that made an unanswered call, the next test, and the file', async () => {
  const { code, output } = await runFixture();
  expect(code, output).not.toBe(0);
  // After the test that made the call (afterEach).
  expect(output).toContain(`GitCommit, in "unanswered calls > calls GitCommit during the test" (${FIXTURE})`);
  expect(output).toMatch(/×[^\n]*calls GitCommit during the test/);
  // Made after a test ended, at the end of the next one.
  expect(output).toContain(`GitPush, after "unanswered calls > calls GitPush after the test ended" (${FIXTURE}) ended`);
  expect(output).toMatch(/×[^\n]*runs after the test that called GitPush/);
  // Made after the last test, at the end of the file (afterAll).
  expect(output).toContain(`GitPull, after "unanswered calls > runs after the test that called GitPush" (${FIXTURE}) ended`);
}, 120_000);
