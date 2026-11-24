#!/usr/bin/env node
// Repeatable check of the stalled-page watchdog (src/test/stalledPageWatchdog.ts):
//
//   cd frontend && ../bin/ao-netns pnpm run check:stall-watchdog
//
// It first checks that vitest.config.ts registers the watchdog, then runs
// the real watchdog, under src/test/stalledPage/vitest.config.ts and its
// 8 s bound, against real browser pages:
//   - a test whose page blocks: the run ends, failing, naming the file;
//   - a page that blocks after its file's last result, during Vitest's
//     cleanup: the run ends, failing, saying no file was running;
//   - live files that report for longer than the bound: the run passes.
// The blocked pages wait on synchronous requests the dev server holds, so
// they burn no CPU. It cannot live in the suite: the watchdog ends the whole
// Vitest process it runs in.
import { spawn } from 'node:child_process';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const frontend = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const fixtures = path.join(frontend, 'src/test/stalledPage');
// The 8 s bound, Vitest's 10 s teardownTimeout, and startup, with room.
const DEADLINE_MS = 60_000;
const CHECK_BOUND = '8 s';

const failures = [];

const { default: config } = await import(pathToFileURL(path.join(frontend, 'vitest.config.ts')).href);
const { StalledPageWatchdog } = await import(pathToFileURL(path.join(frontend, 'src/test/stalledPageWatchdog.ts')).href);
const registered = (config.test?.reporters ?? []).filter((reporter) => reporter instanceof StalledPageWatchdog);
if (registered.length !== 1) failures.push(`vitest.config.ts registers ${registered.length} stalled-page watchdogs, not 1`);

async function run(files) {
  const startedAt = Date.now();
  const child = spawn(
    process.execPath,
    [path.join(frontend, 'node_modules/vitest/vitest.mjs'), 'run', '--config', 'src/test/stalledPage/vitest.config.ts', ...files],
    { cwd: frontend, stdio: ['ignore', 'pipe', 'pipe'], env: { ...process.env, FORCE_COLOR: '0' } },
  );
  let output = '';
  child.stdout.on('data', (chunk) => (output += chunk));
  child.stderr.on('data', (chunk) => (output += chunk));
  // SIGTERM first so a run without the watchdog still closes its browser.
  let killTimer;
  const deadline = setTimeout(() => {
    child.kill('SIGTERM');
    killTimer = setTimeout(() => child.kill('SIGKILL'), 10_000);
  }, DEADLINE_MS);
  const [code, signal] = await new Promise((resolve) => child.on('exit', (c, s) => resolve([c, s])));
  clearTimeout(deadline);
  clearTimeout(killTimer);
  return { code, signal, output, seconds: ((Date.now() - startedAt) / 1000).toFixed(1) };
}

const scenarios = [
  {
    name: 'a test whose page blocks',
    files: ['blocked-test.fixture.ts'],
    expect: (result) => [
      result.code === 1 || `exited ${result.signal ?? result.code}, not 1`,
      result.output.includes(path.join(fixtures, 'blocked-test.fixture.ts')) || 'does not name the file',
      result.output.includes(`reported nothing for at least ${CHECK_BOUND}`) || `does not state the ${CHECK_BOUND} bound`,
    ],
  },
  {
    name: 'a page that blocks after its file ended',
    files: ['blocked-after-end.fixture.ts'],
    expect: (result) => [
      result.code === 1 || `exited ${result.signal ?? result.code}, not 1`,
      result.output.includes(`(bound ${CHECK_BOUND}) with no test file running, and has not ended`) ||
        'does not report a run with no file running',
    ],
  },
  {
    name: 'live files that report for longer than the bound',
    files: ['quick.fixture.ts', 'live-long.fixture.ts'],
    expect: (result) => [
      result.code === 0 || `exited ${result.signal ?? result.code}, not 0`,
      !result.output.includes('stalled-page watchdog:') || 'the watchdog fired',
    ],
  },
];

// The child config includes the fixtures by name, which is what makes knip
// report one no scenario runs; the scenarios run exactly those.
const { default: childConfig } = await import(pathToFileURL(path.join(fixtures, 'vitest.config.ts')).href);
const included = [...childConfig.test.include].map((file) => path.basename(file)).sort();
const named = [...new Set(scenarios.flatMap((scenario) => scenario.files))].sort();
if (included.join() !== named.join()) {
  failures.push(`src/test/stalledPage/vitest.config.ts includes ${included.join(', ')}; the scenarios run ${named.join(', ')}`);
}

for (const scenario of scenarios) {
  const result = await run(scenario.files.map((file) => path.join('src/test/stalledPage', file)));
  const problems = scenario.expect(result).filter((check) => check !== true);
  if (problems.length > 0) {
    failures.push(`${scenario.name}: ${problems.join('; ')}\n${result.output}`);
  } else {
    console.log(`passed: ${scenario.name} (exit ${result.code} in ${result.seconds} s)`);
  }
}

if (failures.length > 0) {
  console.error(`check-stalled-page-watchdog FAILED:\n  ${failures.join('\n  ')}`);
  process.exit(1);
}
console.log('check-stalled-page-watchdog passed');
