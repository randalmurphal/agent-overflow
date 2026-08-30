// Counts compositor frames with missing raster tiles or checkerboarded content.
// usage: probe checkerboard [seconds=15] [label=run]

import { mkdirSync, writeFileSync } from 'node:fs';
import { connectBrowser, done, readStream, sleep } from './lib/cdp.mjs';

const MAX_SECONDS = 15;
const seconds = Number(process.argv[2] ?? MAX_SECONDS);
const label = process.argv[3] ?? 'run';
if (!Number.isFinite(seconds) || seconds <= 0) {
  throw new Error(`probe: checkerboard seconds must be positive, got ${process.argv[2]}`);
}
if (seconds > MAX_SECONDS) {
  throw new Error(
    `probe: checkerboard is capped at ${MAX_SECONDS}s because Chromium retains the tracing buffer inside the WebView2 group; sample one segment of a longer workload`,
  );
}

function numericMetrics(value, path = '', depth = 0, result = []) {
  if (value === null || typeof value !== 'object' || depth > 8) return result;
  for (const [key, child] of Object.entries(value)) {
    const childPath = path ? `${path}.${key}` : key;
    const normalized = key.toLowerCase().replaceAll('_', ' ');
    if (
      typeof child === 'number' &&
      (normalized.includes('missing tile') || normalized.includes('checkerboard'))
    ) {
      result.push([childPath, child]);
    } else if (child !== null && typeof child === 'object') {
      numericMetrics(child, childPath, depth + 1, result);
    }
  }
  return result;
}

const browser = await connectBrowser();
let exitCode = 0;
try {
  browser.events.length = 0;
  await browser.send('Tracing.start', {
    traceConfig: {
      includedCategories: ['cc', 'benchmark'],
      excludedCategories: ['*'],
    },
    transferMode: 'ReturnAsStream',
  });
  await sleep(seconds * 1000);
  const complete = browser.waitFor('Tracing.tracingComplete');
  await browser.send('Tracing.end');
  const { stream } = await complete;
  const data = await readStream(browser, stream);
  const outDir = process.env.AO_PERFPROBE_OUT || '.';
  mkdirSync(outDir, { recursive: true });
  const out = `${outDir}\\trace-checkerboard-${label}.json`;
  writeFileSync(out, data);

  const trace = JSON.parse(data);
  const events = trace.traceEvents || trace;
  const renderPasses = events.filter(
    (event) => event.name === 'LayerTreeHostImpl::CalculateRenderPasses',
  );
  const totals = new Map();
  const positiveEvents = [];
  for (const event of events) {
    const metrics = numericMetrics(event.args);
    if (metrics.length === 0) continue;
    let positive = false;
    for (const [name, value] of metrics) {
      const aggregate = totals.get(name) || { samples: 0, positive: 0, total: 0, max: 0 };
      aggregate.samples++;
      aggregate.total += value;
      aggregate.max = Math.max(aggregate.max, value);
      if (value > 0) {
        aggregate.positive++;
        positive = true;
      }
      totals.set(name, aggregate);
    }
    if (positive && positiveEvents.length < 20) {
      positiveEvents.push({
        name: event.name,
        phase: event.ph,
        atUs: event.ts,
        metrics: Object.fromEntries(metrics.filter(([, value]) => value > 0)),
      });
    }
  }

  const positiveMetrics = [...totals.entries()]
    .filter(([, aggregate]) => aggregate.positive > 0)
    .sort((left, right) => right[1].total - left[1].total)
    .map(([name, aggregate]) => ({ name, ...aggregate }));
  const result = {
    seconds,
    events: events.length,
    calculateRenderPasses: renderPasses.length,
    positiveMetrics,
    positiveEvents,
    rawTrace: out,
  };
  console.log(JSON.stringify(result, null, 2));
  if (renderPasses.length === 0) exitCode = 2;
  else if (positiveMetrics.length > 0) exitCode = 3;
} finally {
  await done([browser], exitCode);
}
