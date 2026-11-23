#!/usr/bin/env node
// Repeatable check of the stalled-page watchdog (src/test/stalledPageWatchdog.ts)
// against a real browser page whose main thread is blocked:
//
//   cd frontend && ../bin/ao-netns pnpm run check:stall-watchdog
//
// It runs src/test/stalledPage/vitest.config.ts, whose one browser file
// blocks its page with synchronous requests the dev server holds, so the
// page's own test timeout never fires. The check passes when that run ends
// by itself with a failing exit that names the file and the bound. It cannot
// live in the suite: the watchdog ends the whole Vitest process it runs in.
import { spawn } from 'node:child_process';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const frontend = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const fixture = path.join(frontend, 'src/test/stalledPage/stall.fixture.ts');
// The 8 s bound, Vitest's 10 s teardownTimeout, and startup, with room.
const DEADLINE_MS = 60_000;

const startedAt = Date.now();
const child = spawn(
  process.execPath,
  [path.join(frontend, 'node_modules/vitest/vitest.mjs'), 'run', '--config', 'src/test/stalledPage/vitest.config.ts'],
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
const elapsedS = ((Date.now() - startedAt) / 1000).toFixed(1);

const failures = [];
if (signal) failures.push(`the run did not end by itself within ${DEADLINE_MS / 1000} s (killed with ${signal})`);
else if (code === 0) failures.push('the run exited 0');
if (!output.includes('stalled-page watchdog:')) failures.push('the output has no watchdog report');
if (!output.includes(fixture)) failures.push(`the output does not name ${fixture}`);
if (!output.includes('reported nothing for 8 s')) failures.push('the output does not state the 8 s bound');

if (failures.length > 0) {
  console.error(`${output}\ncheck-stalled-page-watchdog FAILED after ${elapsedS} s:\n  ${failures.join('\n  ')}`);
  process.exit(1);
}
console.log(`check-stalled-page-watchdog passed: the blocked run ended with exit ${code} in ${elapsedS} s naming the file`);
