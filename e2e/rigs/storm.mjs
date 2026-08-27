// Resize-storm capture: N panes on real threads, concurrent from-thread
// replays, CPU profile + timeline trace with stacks + in-page busy meter.
// The heaviest normal-operation load the app has: every pane streams at
// once while the reveal drain, spring, and virtualizer all run.
//
//   node storm.mjs <url> <outPrefix> --instance <id> --threads "A,B,C"
//
// Scenarios must be installed first (installReplayScenarios or the CLI);
// this script assumes each thread answers `replay` with a full turn.
// Replay density is 15ms/line — denser than real provider streaming
// (~100ms flush windows), so quote budget-fit percentages with that
// caveat and compare storm-to-storm, not storm-to-live.
import { chromium } from '@playwright/test';
import { writeFile } from 'node:fs/promises';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { parseArgs, makeCli, requireThreads, openPanes, awaitRevealDrain } from './riglib.mjs';

const { positional: [url, outPrefix], flags } = parseArgs(process.argv.slice(2));
if (!url || !outPrefix) {
  console.error('usage: node storm.mjs <url> <outPrefix> --instance <id> --threads "A,B,C"');
  process.exit(2);
}
const cli = makeCli(flags.instance);
const threads = requireThreads(flags, 'storm');
const run = promisify(execFile);
const bin = process.env.AO_HARNESS_BIN ?? new URL('../../bin/ao-harness', import.meta.url).pathname;

const browser = await chromium.launch({ headless: true });
const page = await browser.newPage({ viewport: { width: 2560, height: 1400 } });
await page.goto(url, { waitUntil: 'load' });
await page.waitForTimeout(3000);
await openPanes(cli, page, threads);
await page.waitForTimeout(2000);

await cli('perf', 'start', '--budgets', '6,8,16');
const cdp = await page.context().newCDPSession(page);
await cdp.send('Profiler.enable');
await cdp.send('Profiler.setSamplingInterval', { interval: 100 });
await cdp.send('Profiler.start');
await browser.startTracing(page, {
  categories: [
    'devtools.timeline',
    'disabled-by-default-devtools.timeline',
    'disabled-by-default-devtools.timeline.stack',
  ],
});

const inst = flags.instance ? ['--instance', flags.instance] : [];
const sends = threads.map((t) =>
  run(bin, ['send', '--thread', t, '--wait', '--timeout', '240s', 'replay', ...inst], {
    timeout: 300_000, maxBuffer: 32 * 1024 * 1024,
  }).then(() => 'ok', (e) => 'fail: ' + (e.message?.split('\n')[0] ?? '').slice(0, 120)),
);
console.log('sends:', JSON.stringify(await Promise.all(sends)));
await awaitRevealDrain(page);

const trace = await browser.stopTracing();
const { profile } = await cdp.send('Profiler.stop');
await writeFile(`${outPrefix}.json`, trace);
await writeFile(`${outPrefix}.cpuprofile`, JSON.stringify(profile));
const { stdout } = await cli('perf', 'stop', '-o', 'json');
await writeFile(`${outPrefix}-perf.json`, stdout);
console.log('busy:', JSON.stringify(JSON.parse(stdout).frontend.busy));
await browser.close();
