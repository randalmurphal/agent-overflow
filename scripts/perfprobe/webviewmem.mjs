// Sample one CDP-identified WebView2 process group without forcing GC or taking a memory dump.
// usage: probe webviewmem [--for <sec>] [--every-ms <ms>] [--out <csv>] [--append] [--kill-at-private-working-set-mb <MB>] [--require-browser-arg <arg>]
import { spawn } from 'node:child_process';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { connectBrowser, loadInstanceManifest, PORT, sleep } from './lib/cdp.mjs';
import { acquireProbeLease } from './lib/lease.mjs';
import { samplerIdentity } from './lib/webviewmem.mjs';

const args = process.argv.slice(2);
const valueOptions = new Set([
  '--for',
  '--every-ms',
  '--out',
  '--kill-at-private-working-set-mb',
  '--require-browser-arg',
]);
const booleanOptions = new Set(['--append']);
const seenOptions = new Set();
for (let index = 0; index < args.length;) {
  const name = args[index];
  if (booleanOptions.has(name)) {
    if (seenOptions.has(name)) throw new Error(`${name} may be specified only once`);
    seenOptions.add(name);
    index += 1;
    continue;
  }
  if (!valueOptions.has(name)) throw new Error(`unknown webviewmem option ${name}`);
  if (index + 1 >= args.length) throw new Error(`${name} needs a value`);
  if (seenOptions.has(name)) throw new Error(`${name} may be specified only once`);
  seenOptions.add(name);
  index += 2;
}
function numberArg(name, fallback) {
  const index = args.indexOf(name);
  if (index < 0) return fallback;
  const value = Number(args[index + 1]);
  if (!Number.isFinite(value)) throw new Error(`${name} needs a number`);
  return value;
}
function stringArg(name, fallback) {
  const index = args.indexOf(name);
  return index < 0 ? fallback : args[index + 1];
}

const seconds = numberArg('--for', 600);
const everyMs = numberArg('--every-ms', 1000);
const killAtPrivateWorkingSetMB = numberArg('--kill-at-private-working-set-mb', 0);
const requiredBrowserArg = stringArg('--require-browser-arg', '');
const append = seenOptions.has('--append');
if (seconds < 1) throw new Error(`--for must be at least 1 second, got ${seconds}`);
if (everyMs < 1000 || everyMs % 1000 !== 0) {
  throw new Error(`--every-ms must be a whole number of seconds and at least 1000, got ${everyMs}`);
}
if (!Number.isInteger(killAtPrivateWorkingSetMB) || killAtPrivateWorkingSetMB < 0) {
  throw new Error(
    `--kill-at-private-working-set-mb must be a non-negative whole number, got ${killAtPrivateWorkingSetMB}`,
  );
}
if (killAtPrivateWorkingSetMB > 0 && PORT !== '9226') {
  throw new Error('--kill-at-private-working-set-mb is restricted to the isolated perf profile on CDP 9226');
}

const stamp = new Date().toISOString().replaceAll(':', '').replaceAll('.', '-');
const out = path.resolve(stringArg('--out', `webview-memory-${stamp}.csv`));
const browser = await connectBrowser();
let processInfo;
try {
  ({ processInfo } = await browser.send('SystemInfo.getProcessInfo'));
} finally {
  browser.close();
  await sleep(100);
}

const tracked = processInfo.map(({ id, type }) => ({ id, type }));
for (const required of ['browser', 'GPU', 'renderer']) {
  if (!tracked.some(({ type }) => type.toLowerCase() === required.toLowerCase())) {
    throw new Error(`CDP process list has no ${required}`);
  }
}

const browserPid = Number(tracked.find(({ type }) => type.toLowerCase() === 'browser').id);
const script = path.join(path.dirname(fileURLToPath(import.meta.url)), 'webview-memory.ps1');
const powershell = path.join(
  process.env.SystemRoot || 'C:\\Windows',
  'System32',
  'WindowsPowerShell',
  'v1.0',
  'powershell.exe',
);
const childArgs = [
  '-NoProfile',
  '-NonInteractive',
  '-ExecutionPolicy', 'Bypass',
  '-File', script,
  '-PidTypesJson', JSON.stringify(tracked),
  '-ExpectedBrowserPid', String(browserPid),
  '-Out', out,
  '-Seconds', String(Math.ceil(seconds)),
  '-EveryMs', String(Math.round(everyMs)),
  '-KillAtPrivateWorkingSetMB', String(Math.round(killAtPrivateWorkingSetMB)),
];
if (requiredBrowserArg) {
  childArgs.push('-RequiredBrowserArg', requiredBrowserArg);
}
if (append) childArgs.push('-Append');

const manifest = loadInstanceManifest();
childArgs.push('-IdentityJson', JSON.stringify(samplerIdentity(manifest, browserPid)));
const releaseLease = acquireProbeLease(manifest, 'webviewmem', 'counter');
let exitCode;
try {
  exitCode = await new Promise((resolve, reject) => {
    const child = spawn(powershell, childArgs, { stdio: 'inherit', windowsHide: true });
    child.once('error', reject);
    child.once('exit', (code, signal) => {
      if (signal) reject(new Error(`PowerShell sampler ended from signal ${signal}`));
      else resolve(code ?? 1);
    });
  });
} finally {
  releaseLease();
}
if (exitCode !== 0) throw new Error(`PowerShell sampler exited with code ${exitCode}`);

const lines = (await readFile(out, 'utf8')).trim().split(/\r?\n/);
const headers = lines[0].split(',');
const rows = lines.slice(1).map((line) => {
  const values = line.split(',');
  return Object.fromEntries(headers.map((header, index) => [header, values[index]]));
});
if (rows.length < 2) throw new Error(`sampler wrote only ${rows.length} row(s)`);

function percentile(values, fraction) {
  const sorted = values.toSorted((left, right) => left - right);
  return sorted[Math.min(sorted.length - 1, Math.ceil(sorted.length * fraction) - 1)];
}
function mb(bytes) {
  return `${(bytes / 1048576).toFixed(1)}MB`;
}

const fields = [
  ['group', 'groupPrivateBytes', 'groupWorkingSetBytes', 'groupWorkingSetPrivateBytes'],
  ['GPU', 'gpuPrivateBytes', 'gpuWorkingSetBytes', 'gpuWorkingSetPrivateBytes'],
  ['renderer', 'rendererPrivateBytes', 'rendererWorkingSetBytes', 'rendererWorkingSetPrivateBytes'],
  ['browser', 'browserPrivateBytes', 'browserWorkingSetBytes', 'browserWorkingSetPrivateBytes'],
  ['utilities', 'utilityPrivateBytes', 'utilityWorkingSetBytes', 'utilityWorkingSetPrivateBytes'],
  ['crashpad', 'crashpadPrivateBytes', 'crashpadWorkingSetBytes', 'crashpadWorkingSetPrivateBytes'],
];
const firstAt = Date.parse(rows[0].utc);
const lastAt = Date.parse(rows.at(-1).utc);
const sampledSeconds = Number.isFinite(firstAt) && Number.isFinite(lastAt) && lastAt >= firstAt
  ? (lastAt - firstAt) / 1000
  : Number(rows.at(-1).elapsedMs) / 1000;
console.log(`webview-memory: ${rows.length} samples over ${sampledSeconds.toFixed(1)}s`);
const incompleteSamples = rows.filter((row) => Number(row.censusMissingCount) > 0).length;
const completeRows = rows.filter((row) => Number(row.censusMissingCount) === 0);
if (completeRows.length < 2) {
  throw new Error(
    `sampler wrote only ${completeRows.length} complete row(s); ${incompleteSamples} lacked counters for a census process`,
  );
}
const processCounts = rows.map((row) => Number(row.processCount));
console.log(
  `webview-memory: process count ${Math.min(...processCounts)}..${Math.max(...processCounts)}` +
  (incompleteSamples > 0
    ? `; excluded ${incompleteSamples} sample(s) that lacked counters for a census process`
    : ''),
);
console.log('process      private p50/p95/max       working-set p50/p95/max   private-working-set p50/p95/max');
for (const [label, privateField, workingSetField, workingSetPrivateField] of fields) {
  const privateValues = completeRows.map((row) => Number(row[privateField]));
  const workingSetValues = completeRows.map((row) => Number(row[workingSetField]));
  const workingSetPrivateValues = completeRows.map((row) => Number(row[workingSetPrivateField]));
  const privateSummary = [0.5, 0.95, 1].map((part) => mb(percentile(privateValues, part))).join('/');
  const workingSetSummary = [0.5, 0.95, 1]
    .map((part) => mb(percentile(workingSetValues, part)))
    .join('/');
  const workingSetPrivateSummary = [0.5, 0.95, 1]
    .map((part) => mb(percentile(workingSetPrivateValues, part)))
    .join('/');
  console.log(
    `${label.padEnd(12)} ${privateSummary.padEnd(25)} ${workingSetSummary.padEnd(25)} ${workingSetPrivateSummary}`,
  );
}
