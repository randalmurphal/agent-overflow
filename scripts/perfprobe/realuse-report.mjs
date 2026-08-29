// Summarize a live or completed real-use telemetry JSONL and optional WebView memory CSV.
// usage: probe realuse-report --telemetry <jsonl> [--memory <csv>]
// offline
import { readFileSync } from 'node:fs';
import {
  estimateMissedRefreshes,
  histogramCount,
  histogramCountAbove,
  histogramPercentile,
  mergeSparseHistograms,
  parseNumericCsv,
  percentile,
} from './lib/realuse.mjs';

const args = process.argv.slice(2);
const allowed = new Set(['--telemetry', '--memory']);
const options = new Map();
for (let index = 0; index < args.length; index += 2) {
  const name = args[index];
  if (!allowed.has(name)) throw new Error(`unknown realuse-report option ${name}`);
  if (index + 1 >= args.length) throw new Error(`${name} needs a value`);
  if (options.has(name)) throw new Error(`${name} may be specified only once`);
  options.set(name, args[index + 1]);
}
if (!options.has('--telemetry')) throw new Error('--telemetry is required');

function readJsonLines(file) {
  const text = readFileSync(file, 'utf8');
  const complete = text.endsWith('\n');
  const lines = text.split(/\r?\n/);
  if (lines.at(-1) === '') lines.pop();
  const rows = [];
  for (const [index, line] of lines.entries()) {
    if (!line.trim()) continue;
    try {
      rows.push(JSON.parse(line));
    } catch (error) {
      if (!complete && index === lines.length - 1) {
        console.warn(`realuse-report: ignored one incomplete final JSONL row in ${file}`);
        break;
      }
      throw new Error(`${file}:${index + 1}: invalid JSON: ${error.message}`);
    }
  }
  return rows;
}

const formatNumber = (value, digits = 1) => Number(value ?? 0).toFixed(digits);
const formatMs = (value) => `${formatNumber(value)}ms`;
const formatMB = (bytes) => `${formatNumber(Number(bytes) / 1048576)}MB`;
const formatPct = (value) => `${formatNumber(value)}%`;
const maxOf = (values) => Math.max(0, ...values.filter(Number.isFinite));
const sumOf = (values) => values.filter(Number.isFinite).reduce((sum, value) => sum + value, 0);

function durationLabel(rows) {
  if (rows.length < 2) return 'under one sample interval';
  const first = Date.parse(rows[0].utc);
  const last = Date.parse(rows.at(-1).utc);
  if (!Number.isFinite(first) || !Number.isFinite(last) || last < first) return 'unknown duration';
  const seconds = Math.round((last - first) / 1000);
  const hours = Math.floor(seconds / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  const remainder = seconds % 60;
  return `${hours}h ${minutes}m ${remainder}s`;
}

function millisecondsLabel(milliseconds) {
  const seconds = Math.round(milliseconds / 1000);
  const hours = Math.floor(seconds / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  const remainder = seconds % 60;
  return `${hours}h ${minutes}m ${remainder}s`;
}

function observerTotals(samples, name) {
  const entries = samples.map((sample) => sample.observers?.[name]).filter(Boolean);
  return {
    focusedCount: sumOf(entries.map((entry) => entry.focusedCount)),
    focusedMaxMs: maxOf(entries.map((entry) => entry.focusedMaxMs)),
    otherCount: sumOf(entries.map((entry) => entry.otherCount)),
    otherMaxMs: maxOf(entries.map((entry) => entry.otherMaxMs)),
  };
}

const telemetryFile = options.get('--telemetry');
const rows = readJsonLines(telemetryFile);
const sessions = rows.filter((row) => row.kind === 'session-start');
const samples = rows.filter((row) => row.kind === 'sample');
const errors = rows.filter((row) => row.kind === 'sample-error');
if (samples.length === 0) throw new Error(`${telemetryFile} has no complete samples`);

const focusedFrames = mergeSparseHistograms(
  samples.map((sample) => sample.frames?.focused?.sparseBuckets ?? []),
);
const otherFrames = mergeSparseHistograms(
  samples.map((sample) => sample.frames?.other?.sparseBuckets ?? []),
);
const busy = mergeSparseHistograms(samples.map((sample) => sample.busy?.sparseBuckets ?? []));
const focusedFrameCount = histogramCount(focusedFrames);
const otherFrameCount = histogramCount(otherFrames);
const baselineMs = histogramPercentile(focusedFrames, 0.5);
const missedRefreshes = estimateMissedRefreshes(focusedFrames, baselineMs);
const focusedFrameMax = maxOf(samples.map((sample) => sample.frames?.focused?.maxMs));
const busyMax = maxOf(samples.map((sample) => sample.busy?.maxMs));
const busyCount = histogramCount(busy);
const busyWithin = [0, 1, 2].map((index) =>
  sumOf(samples.map((sample) => sample.busy?.withinBudgets?.[index])),
);
const busyDropped = sumOf(samples.map((sample) => sample.busy?.dropped));
const busyDiscarded = sumOf(samples.map((sample) => sample.busy?.discarded));
const longTasks = observerTotals(samples, 'longTasks');
const loafs = observerTotals(samples, 'longAnimationFrames');
const slowEvents = observerTotals(samples, 'slowEvents');
const unavailable = [...new Set(samples.flatMap((sample) => sample.observers?.unavailable ?? []))];
const rearmed = samples.filter((sample) => sample.rearmed).length;
const suspensions = sumOf(samples.map((sample) => sample.frames?.suspensions));
const suspendedMs = sumOf(samples.map((sample) => sample.frames?.suspendedMs));

const cpu = samples.map((sample) => sample.cpu?.normalizedPercent).filter(Number.isFinite);
const cpuRaw = samples.map((sample) => sample.cpu?.rawPercent).filter(Number.isFinite);
const rendererCpu = samples.map((sample) => sample.cpu?.byGroupPercent?.renderer).filter(Number.isFinite);
const gpuCpu = samples.map((sample) => sample.cpu?.byGroupPercent?.gpu).filter(Number.isFinite);
const processTransitions = sumOf(samples.map((sample) =>
  (sample.cpu?.newProcesses ?? 0) + (sample.cpu?.disappearedProcesses ?? 0),
));
const taskPercent = samples
  .map((sample) => {
    const taskMs = sample.chromium?.taskMs;
    return Number.isFinite(taskMs) && sample.elapsedMs > 0 ? (taskMs / sample.elapsedMs) * 100 : null;
  })
  .filter(Number.isFinite);
const heap = samples.map((sample) => sample.chromium?.jsHeapUsedBytes).filter(Number.isFinite);
const nodes = samples.map((sample) => sample.chromium?.nodes).filter(Number.isFinite);

const measuredMs = sumOf(samples.map((sample) => sample.elapsedMs));
console.log(`real-use telemetry: ${millisecondsLabel(measuredMs)} measured  ${samples.length} samples  ${sessions.length} session(s)`);
console.log(`  source: ${telemetryFile}`);
console.log(
  `focused frame delivery: ${focusedFrameCount} gaps  baseline ${formatMs(baselineMs)}  `
  + `p95 ${formatMs(histogramPercentile(focusedFrames, 0.95))}  `
  + `p99 ${formatMs(histogramPercentile(focusedFrames, 0.99))}  max ${formatMs(focusedFrameMax)}`,
);
console.log(
  `  estimated missed refreshes ${missedRefreshes}  gaps >1.5x ${histogramCountAbove(focusedFrames, baselineMs * 1.5)}  `
  + `unfocused/visible gaps ${otherFrameCount}`,
);
console.log(
  `sampled main-thread busy: ${busyCount} ticks  p95 ${formatMs(histogramPercentile(busy, 0.95))}  `
  + `max ${formatMs(busyMax)}  fit 6/8/16ms ${busyWithin.map((count) => formatPct(busyCount > 0 ? count * 100 / busyCount : 0)).join('/')}`,
);
console.log(`  busy probe drops ${busyDropped}, suspend discards ${busyDiscarded}`);
console.log(
  `focused long tasks ${longTasks.focusedCount} (max ${formatMs(longTasks.focusedMaxMs)}), `
  + `long animation frames ${loafs.focusedCount} (max ${formatMs(loafs.focusedMaxMs)}), `
  + `slow input events ${slowEvents.focusedCount} (max ${formatMs(slowEvents.focusedMaxMs)})`,
);
console.log(
  `WebView CPU normalized: p50 ${formatPct(percentile(cpu, 0.5))}  p95 ${formatPct(percentile(cpu, 0.95))}  `
  + `max ${formatPct(percentile(cpu, 1))}  raw-core max ${formatPct(percentile(cpuRaw, 1))}`,
);
console.log(
  `  renderer p95/max ${formatPct(percentile(rendererCpu, 0.95))}/${formatPct(percentile(rendererCpu, 1))}  `
  + `GPU p95/max ${formatPct(percentile(gpuCpu, 0.95))}/${formatPct(percentile(gpuCpu, 1))}  `
  + `renderer-main task p95/max ${formatPct(percentile(taskPercent, 0.95))}/${formatPct(percentile(taskPercent, 1))}`,
);
console.log(
  `renderer levels: JS heap p50/max/last ${formatMB(percentile(heap, 0.5))}/${formatMB(percentile(heap, 1))}/${formatMB(heap.at(-1))}  `
  + `DOM nodes max ${Math.round(maxOf(nodes))}`,
);
console.log(
  `integrity: sample errors ${errors.length}, re-arms ${rearmed}, process transitions ${processTransitions}, `
  + `suspend gaps excluded ${suspensions} (${formatMs(suspendedMs)})`,
);
if (unavailable.length > 0) console.log(`  unavailable observers: ${unavailable.join(', ')}`);

if (options.has('--memory')) {
  const memoryFile = options.get('--memory');
  const memoryRows = parseNumericCsv(readFileSync(memoryFile, 'utf8'));
  const complete = memoryRows.filter((row) => row.censusMissingCount === 0);
  const incomplete = memoryRows.length - complete.length;
  if (complete.length === 0) throw new Error(`${memoryFile} has no complete memory samples`);
  const values = (field) => complete.map((row) => row[field]);
  const summary = (field) =>
    `${formatMB(percentile(values(field), 0.5))}/${formatMB(percentile(values(field), 0.95))}/${formatMB(percentile(values(field), 1))}`;
  console.log(`WebView memory: ${durationLabel(memoryRows)}  ${complete.length} complete sample(s), ${incomplete} incomplete`);
  console.log(`  source: ${memoryFile}`);
  console.log(`  group private working set p50/p95/max ${summary('groupWorkingSetPrivateBytes')}`);
  console.log(`  group private bytes p50/p95/max ${summary('groupPrivateBytes')}`);
  console.log(
    `  role private-working-set max GPU ${formatMB(percentile(values('gpuWorkingSetPrivateBytes'), 1))}  `
    + `renderer ${formatMB(percentile(values('rendererWorkingSetPrivateBytes'), 1))}  `
    + `browser ${formatMB(percentile(values('browserWorkingSetPrivateBytes'), 1))}`,
  );
}
