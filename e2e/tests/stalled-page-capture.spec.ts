// The fixtures' stalled-page capture (src/page-stall.ts, wired in
// tests/fixtures.ts) against real pages whose main thread is blocked.
//
// The capture runs only when a test fails, and the test that fails cannot
// read its own teardown attachments, so this runs a nested Playwright run of
// tests/stalled-page/blocked-page.fixture.ts and reads its JSON report. Each
// nested test blocks its page with synchronous requests the server holds (no
// spinning) and times out waiting on it. In Chromium the attachment names the
// blocking functions from a paused stack with their source, or says the pause
// did not land for a loop V8 does not interrupt (tests/stalled-page/blocked-page.ts);
// in WebKit it carries the probe result and says there is no CDP.
import { spawn } from 'node:child_process';
import { createRequire } from 'node:module';
import path from 'node:path';
import { expect, test } from './base-test.js';
import { STALL_CAPTURE_BOUNDS } from '../src/page-stall.js';

const E2E = path.resolve(import.meta.dirname, '..');
const CONFIG = path.join(E2E, 'tests', 'stalled-page', 'playwright.config.ts');
const NESTED_TEST_TIMEOUT_MS = 5_000;
const CAPTURE_BOUND_MS = Object.values(STALL_CAPTURE_BOUNDS).reduce((sum, ms) => sum + ms, 0);

interface JsonAttachment {
  name: string;
  body?: string;
}
interface JsonResult {
  status: string;
  duration: number;
  attachments: JsonAttachment[];
}
interface JsonSpec {
  title: string;
  tests: Array<{ projectName: string; results: JsonResult[] }>;
}
interface JsonSuite {
  specs?: JsonSpec[];
  suites?: JsonSuite[];
}

function specs(suite: JsonSuite): JsonSpec[] {
  return [...(suite.specs ?? []), ...(suite.suites ?? []).flatMap(specs)];
}

async function runNested(
  outputDir: string,
  project: string,
): Promise<{ code: number | null; report: JsonSuite; stderr: string }> {
  const cli = createRequire(import.meta.url).resolve('@playwright/test/cli');
  // The parent run's worker identity must not leak into the nested runner.
  const env = Object.fromEntries(
    Object.entries(process.env).filter(([key]) => !key.startsWith('TEST_') && !key.startsWith('PW_')),
  );
  const args = [cli, 'test', '--config', CONFIG, '--project', project, '--output', outputDir];
  const child = spawn(process.execPath, args, {
    cwd: E2E,
    env: { ...env, FORCE_COLOR: '0' },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let stdout = '';
  let stderr = '';
  child.stdout.on('data', (chunk: Buffer) => (stdout += chunk));
  child.stderr.on('data', (chunk: Buffer) => (stderr += chunk));
  const code = await new Promise<number | null>((resolve, reject) => {
    child.on('error', reject);
    child.on('exit', resolve);
  });
  let report: JsonSuite;
  try {
    report = JSON.parse(stdout) as JsonSuite;
  } catch (err) {
    throw new Error(`the nested run printed no JSON report (exit ${code}):\n${stdout}\n${stderr}`, { cause: err });
  }
  return { code, report, stderr };
}

interface NestedCase {
  /** Status the original failure must keep. */
  status: 'timedOut' | 'failed';
  /** Stalled pages the test leaves open. */
  pages: number;
  /** Time the test spends besides its timeout and the capture. */
  extraMs: number;
}

const CASES: Record<string, NestedCase> = {
  'a page from the page fixture blocks': { status: 'timedOut', pages: 1, extraMs: 0 },
  'a page in a context the test opens blocks': { status: 'timedOut', pages: 1, extraMs: 0 },
  'a page blocked where a pause cannot land': { status: 'timedOut', pages: 1, extraMs: 0 },
  'two pages block in one test': { status: 'timedOut', pages: 2, extraMs: 0 },
  'a test that passes until its page-error check fails': { status: 'failed', pages: 1, extraMs: 0 },
  'a page blocks before the afterEach runs': { status: 'timedOut', pages: 1, extraMs: 12_000 },
};

/** Runs the nested project and returns each test's report bodies by title. */
async function nestedReports(project: string, outputDir: string): Promise<Map<string, string[]>> {
  const { code, report, stderr } = await runNested(outputDir, project);
  expect(code, stderr).toBe(1);
  const bodies = new Map<string, string[]>();
  for (const spec of specs(report)) {
    for (const run of spec.tests) {
      const result = run.results[0]!;
      const where = `${run.projectName}: ${spec.title}`;
      const expected = CASES[spec.title];
      expect(expected, `${where} is a case this spec knows`).toBeDefined();
      // The capture never replaces the original failure.
      expect(result.status, where).toBe(expected!.status);
      const reports = result.attachments
        .filter((attachment) => attachment.name === 'stalled page')
        .map((attachment) => Buffer.from(attachment.body ?? '', 'base64').toString('utf8'));
      expect(reports, where).toHaveLength(expected!.pages);
      for (const body of reports) {
        expect(body, where).toContain(
          `did not answer a trivial evaluate (page.evaluate) within ${STALL_CAPTURE_BOUNDS.probeMs} ms`,
        );
        // The held requests reach the test without the page's main thread.
        expect(body, where).toMatch(
          /Last thing the page did .*: request GET http:\/\/127\.0\.0\.1:\d+\/hold, \d+ ms before the probe gave up/,
        );
      }
      // The capture adds at most its bound to a failure.
      expect(result.duration, where).toBeLessThan(NESTED_TEST_TIMEOUT_MS + expected!.extraMs + CAPTURE_BOUND_MS + 5_000);
      bodies.set(spec.title, reports);
    }
  }
  expect([...bodies.keys()].sort()).toEqual(Object.keys(CASES).sort());
  return bodies;
}

const STACK = [
  // Each frame carries the source around its position: the pause lands in
  // the loop, under the timer that started it.
  /#0 holdMainThread @ http:\/\/127\.0\.0\.1:\d+\/app\.js:\d+:\d+\n.*(location\.hash|xhr\.)/,
  /#1 blockedTimer @ .*\n.*function blockedTimer\(\)/,
];

test('a failed test attaches the stack of each Chromium page that stopped running JavaScript', async ({}, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'the desktop project runs the Chromium case');
  test.setTimeout(240_000);
  const bodies = await nestedReports('chromium', testInfo.outputPath('nested'));
  for (const [title, reports] of bodies) {
    if (title === 'a page blocked where a pause cannot land') continue;
    for (const body of reports) for (const frame of STACK) expect(body, title).toMatch(frame);
  }
  expect(bodies.get('two pages block in one test')!.map((body) => body.split('\n')[0]!.replace(/:\d+\/$/, ''))).toEqual([
    'page 1 of 2: http://127.0.0.1',
    'page 2 of 2: http://127.0.0.1',
  ]);
  const [unpaused] = bodies.get('a page blocked where a pause cannot land')!;
  expect(unpaused).toContain(`Debugger.pause did not land within ${STALL_CAPTURE_BOUNDS.pauseMs} ms`);
  expect(unpaused).toMatch(/ {2}Debugger\.pause: reply (answered|failed|not answered)/);
  expect(unpaused).toContain(
    `  Debugger.paused event: Debugger.paused did not answer within ${STALL_CAPTURE_BOUNDS.pauseMs} ms`,
  );
  expect(unpaused).not.toContain('#0 ');
});

test('a failed test reports each WebKit page that stopped running JavaScript without CDP', async ({}, testInfo) => {
  test.skip(testInfo.project.name !== 'mobile-webkit', 'the mobile-webkit project runs the WebKit case');
  test.setTimeout(240_000);
  const bodies = await nestedReports('webkit', testInfo.outputPath('nested'));
  for (const [title, reports] of bodies) {
    for (const body of reports) expect(body, title).toContain('has no CDP, so its stack cannot be read');
  }
});
