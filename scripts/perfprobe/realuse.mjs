// Low-overhead real-use telemetry: visible frame delivery, sampled busy time,
// Chromium task/heap/DOM levels, and per-process CPU. No trace, profile, GC, or content capture.
// usage: probe realuse [--for <sec>] [--every-ms <ms>] [--out <jsonl>] [--append]
import { randomUUID } from 'node:crypto';
import { appendFileSync, mkdirSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import {
  BASE,
  PORT,
  connectBrowser,
  connectTarget,
  evaluate,
  loadInstanceManifest,
  sleep,
} from './lib/cdp.mjs';
import { metricDelta, metricMap, processCpuDelta } from './lib/realuse.mjs';

const valueOptions = new Set([
  '--for',
  '--every-ms',
  '--out',
  '--require-title',
  '--require-browser-arg',
]);
const booleanOptions = new Set(['--append']);
const options = new Map();
const args = process.argv.slice(2);
for (let index = 0; index < args.length;) {
  const name = args[index];
  if (booleanOptions.has(name)) {
    if (options.has(name)) throw new Error(`${name} may be specified only once`);
    options.set(name, true);
    index += 1;
    continue;
  }
  if (!valueOptions.has(name)) throw new Error(`unknown realuse option ${name}`);
  if (index + 1 >= args.length) throw new Error(`${name} needs a value`);
  if (options.has(name)) throw new Error(`${name} may be specified only once`);
  options.set(name, args[index + 1]);
  index += 2;
}

function numberOption(name, fallback) {
  if (!options.has(name)) return fallback;
  const value = Number(options.get(name));
  if (!Number.isFinite(value)) throw new Error(`${name} needs a number`);
  return value;
}

function stringOption(name, fallback = '') {
  return options.has(name) ? String(options.get(name)) : fallback;
}

const seconds = numberOption('--for', 86_400);
const everyMs = numberOption('--every-ms', 10_000);
const output = path.resolve(stringOption('--out', `realuse-${Date.now()}.jsonl`));
const append = options.has('--append');
const requiredTitle = stringOption('--require-title');
const requiredBrowserArg = stringOption('--require-browser-arg');

if (seconds < 10) throw new Error(`--for must be at least 10 seconds, got ${seconds}`);
if (!Number.isInteger(everyMs) || everyMs < 5_000) {
  throw new Error(`--every-ms must be a whole number at least 5000, got ${everyMs}`);
}
function exactArgument(commandLine, argument) {
  const escaped = argument.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  return new RegExp(`(?:^|\\s)${escaped}(?:\\s|$)`).test(commandLine);
}

function pageOrigin(rawUrl) {
  const url = new URL(rawUrl);
  if (url.protocol !== 'http:' || !['127.0.0.1', 'localhost'].includes(url.hostname)) {
    throw new Error(`refusing non-loopback page target ${url.origin}`);
  }
  return url.origin;
}

function round(value, digits = 4) {
  if (value === null || value === undefined || !Number.isFinite(value)) return null;
  const scale = 10 ** digits;
  return Math.round(value * scale) / scale;
}

function installPageMonitor() {
  const KEY = '__agentOverflowRealUseMonitor_8f4f25b1';
  const VERSION = 1;
  const BUCKET_MS = 0.25;
  const BUCKET_COUNT = 1001;
  const BUSY_EVERY_FRAMES = 16;
  const WATCHDOG_MS = 120_000;
  const SUSPEND_GAP_MS = 2_000;

  const previous = window[KEY];
  if (previous?.version === VERSION && typeof previous.stop === 'function') {
    previous.stop('replaced');
  }

  const histogram = () => ({
    buckets: new Uint32Array(BUCKET_COUNT),
    count: 0,
    sumMs: 0,
    maxMs: 0,
  });
  const observers = () => ({
    focusedCount: 0,
    focusedSumMs: 0,
    focusedMaxMs: 0,
    otherCount: 0,
    otherSumMs: 0,
    otherMaxMs: 0,
  });
  const state = {
    version: VERSION,
    running: true,
    startedAtMs: performance.now(),
    lastCollectAtMs: performance.now(),
    visible: document.visibilityState === 'visible',
    focused: document.hasFocus(),
    lastFrameAtMs: null,
    frameSequence: 0,
    focusedFrames: histogram(),
    otherFrames: histogram(),
    busy: histogram(),
    busyPending: false,
    busyStartedAtMs: 0,
    busyWithinBudgets: [0, 0, 0],
    busyDropped: 0,
    busyDiscarded: 0,
    suspensions: 0,
    suspendedMs: 0,
    longTasks: observers(),
    loafs: observers(),
    slowEvents: observers(),
    observerErrors: [],
    performanceObservers: [],
    rafId: 0,
    watchdogId: 0,
    stopReason: '',
    channel: new MessageChannel(),
    onVisibility: null,
    onFocus: null,
    onBlur: null,
    touch: null,
    collect: null,
    stop: null,
  };

  const recordHistogram = (hist, durationMs) => {
    if (!Number.isFinite(durationMs) || durationMs < 0) return;
    const bucket = Math.min(BUCKET_COUNT - 1, Math.floor(durationMs / BUCKET_MS));
    hist.buckets[bucket] += 1;
    hist.count += 1;
    hist.sumMs += durationMs;
    if (durationMs > hist.maxMs) hist.maxMs = durationMs;
  };
  const recordObserver = (target, durationMs) => {
    const focused = state.visible && state.focused;
    const prefix = focused ? 'focused' : 'other';
    target[`${prefix}Count`] += 1;
    target[`${prefix}SumMs`] += durationMs;
    if (durationMs > target[`${prefix}MaxMs`]) target[`${prefix}MaxMs`] = durationMs;
  };
  const drainHistogram = (hist) => {
    const sparseBuckets = [];
    for (let index = 0; index < hist.buckets.length; index += 1) {
      const count = hist.buckets[index];
      if (count !== 0) sparseBuckets.push([index, count]);
    }
    const result = {
      count: hist.count,
      sumMs: hist.sumMs,
      maxMs: hist.maxMs,
      sparseBuckets,
    };
    hist.buckets.fill(0);
    hist.count = 0;
    hist.sumMs = 0;
    hist.maxMs = 0;
    return result;
  };
  const drainObserver = (target) => {
    const result = { ...target };
    Object.assign(target, observers());
    return result;
  };
  const observe = (type, target, init = { type, buffered: false }) => {
    try {
      if (!PerformanceObserver.supportedEntryTypes?.includes(type)) {
        state.observerErrors.push(`${type}: unavailable`);
        return;
      }
      const observer = new PerformanceObserver((list) => {
        for (const entry of list.getEntries()) recordObserver(target, entry.duration);
      });
      observer.observe(init);
      state.performanceObservers.push(observer);
    } catch (error) {
      state.observerErrors.push(`${type}: ${String(error)}`);
    }
  };

  state.touch = () => {
    clearTimeout(state.watchdogId);
    state.watchdogId = setTimeout(() => state.stop('watchdog'), WATCHDOG_MS);
  };
  state.onVisibility = () => {
    state.visible = document.visibilityState === 'visible';
    state.lastFrameAtMs = null;
  };
  state.onFocus = () => {
    state.focused = true;
    state.lastFrameAtMs = null;
  };
  state.onBlur = () => {
    state.focused = false;
    state.lastFrameAtMs = null;
  };
  document.addEventListener('visibilitychange', state.onVisibility, { passive: true });
  window.addEventListener('focus', state.onFocus, { passive: true });
  window.addEventListener('blur', state.onBlur, { passive: true });

  state.channel.port1.onmessage = () => {
    if (!state.busyPending) return;
    const durationMs = performance.now() - state.busyStartedAtMs;
    state.busyPending = false;
    if (durationMs >= SUSPEND_GAP_MS) {
      state.busyDiscarded += 1;
      return;
    }
    recordHistogram(state.busy, durationMs);
    for (const [index, budgetMs] of [6, 8, 16].entries()) {
      if (durationMs <= budgetMs) state.busyWithinBudgets[index] += 1;
    }
  };

  const tick = (timestamp) => {
    if (!state.running) return;
    if (state.visible) {
      if (state.lastFrameAtMs !== null) {
        const deltaMs = timestamp - state.lastFrameAtMs;
        if (deltaMs >= SUSPEND_GAP_MS) {
          state.suspensions += 1;
          state.suspendedMs += deltaMs;
        } else {
          recordHistogram(state.focused ? state.focusedFrames : state.otherFrames, deltaMs);
        }
      }
      state.frameSequence += 1;
      if (state.focused && state.frameSequence % BUSY_EVERY_FRAMES === 0) {
        if (state.busyPending) {
          state.busyDropped += 1;
        } else {
          state.busyPending = true;
          state.busyStartedAtMs = performance.now();
          state.channel.port2.postMessage(0);
        }
      }
      state.lastFrameAtMs = timestamp;
    } else {
      state.lastFrameAtMs = null;
    }
    state.rafId = requestAnimationFrame(tick);
  };

  observe('longtask', state.longTasks);
  observe('long-animation-frame', state.loafs);
  observe('event', state.slowEvents, { type: 'event', buffered: false, durationThreshold: 16 });

  state.collect = () => {
    state.touch();
    const now = performance.now();
    const result = {
      version: VERSION,
      atMs: now,
      intervalMs: now - state.lastCollectAtMs,
      visible: state.visible,
      focused: state.focused,
      hardwareConcurrency: navigator.hardwareConcurrency || 1,
      focusedFrames: drainHistogram(state.focusedFrames),
      otherFrames: drainHistogram(state.otherFrames),
      busy: drainHistogram(state.busy),
      busyWithinBudgets: [...state.busyWithinBudgets],
      busyDropped: state.busyDropped,
      busyDiscarded: state.busyDiscarded,
      suspensions: state.suspensions,
      suspendedMs: state.suspendedMs,
      longTasks: drainObserver(state.longTasks),
      loafs: drainObserver(state.loafs),
      slowEvents: drainObserver(state.slowEvents),
      observerErrors: [...state.observerErrors],
    };
    state.lastCollectAtMs = now;
    state.busyDropped = 0;
    state.busyDiscarded = 0;
    state.busyWithinBudgets.fill(0);
    state.suspensions = 0;
    state.suspendedMs = 0;
    return result;
  };
  state.stop = (reason = 'requested') => {
    if (!state.running) return;
    state.running = false;
    state.stopReason = reason;
    cancelAnimationFrame(state.rafId);
    clearTimeout(state.watchdogId);
    for (const observer of state.performanceObservers) observer.disconnect();
    state.channel.port1.close();
    state.channel.port2.close();
    document.removeEventListener('visibilitychange', state.onVisibility);
    window.removeEventListener('focus', state.onFocus);
    window.removeEventListener('blur', state.onBlur);
    if (window[KEY] === state) delete window[KEY];
  };

  Object.defineProperty(window, KEY, {
    value: state,
    configurable: true,
    enumerable: false,
    writable: false,
  });
  state.touch();
  state.rafId = requestAnimationFrame(tick);
  return state.collect();
}

const installExpression = `(${installPageMonitor.toString()})()`;
const collectExpression = `(() => {
  const state = window.__agentOverflowRealUseMonitor_8f4f25b1;
  return state?.version === 1 && state.running ? state.collect() : null;
})()`;
const stopExpression = `(() => {
  const state = window.__agentOverflowRealUseMonitor_8f4f25b1;
  if (!state?.running) return false;
  state.stop('collector stopped');
  return true;
})()`;

async function selectPageTarget() {
  const manifest = loadInstanceManifest();
  const response = await fetch(`${BASE}/json/list`);
  if (!response.ok) throw new Error(`CDP target list returned HTTP ${response.status}`);
  const targets = await response.json();
  let pages = targets.filter((target) => target.type === 'page');
  if (requiredTitle) pages = pages.filter((target) => target.title === requiredTitle);
  if (pages.length !== 1) {
    throw new Error(
      `expected exactly one${requiredTitle ? ` ${JSON.stringify(requiredTitle)}` : ''} page on CDP ${PORT}, got ${pages.length}`,
    );
  }
  if (pages[0].id !== manifest.target.targetId) {
    throw new Error(`selected page ${pages[0].id} is not the supervisor target ${manifest.target.targetId}`);
  }
  pageOrigin(pages[0].url);
  if (!pages[0].webSocketDebuggerUrl) throw new Error('selected page has no debugger URL');
  return pages[0];
}

function pageMetrics(previous, current) {
  return {
    taskMs: round(metricDelta(previous, current, 'TaskDuration', 1000)),
    scriptMs: round(metricDelta(previous, current, 'ScriptDuration', 1000)),
    layoutMs: round(metricDelta(previous, current, 'LayoutDuration', 1000)),
    recalcStyleMs: round(metricDelta(previous, current, 'RecalcStyleDuration', 1000)),
    layoutCount: metricDelta(previous, current, 'LayoutCount'),
    recalcStyleCount: metricDelta(previous, current, 'RecalcStyleCount'),
    jsHeapUsedBytes: current.JSHeapUsedSize ?? null,
    jsHeapTotalBytes: current.JSHeapTotalSize ?? null,
    nodes: current.Nodes ?? null,
    layoutObjects: current.LayoutObjects ?? null,
    jsEventListeners: current.JSEventListeners ?? null,
    documents: current.Documents ?? null,
  };
}

mkdirSync(path.dirname(output), { recursive: true });
if (!append) writeFileSync(output, '');
const sessionId = randomUUID();
const writeRow = (row) => appendFileSync(output, `${JSON.stringify(row)}\n`);
let page;
let browser;
let stopping = false;
const requestStop = () => { stopping = true; };
process.once('SIGINT', requestStop);
process.once('SIGTERM', requestStop);

try {
  const target = await selectPageTarget();
  const manifest = loadInstanceManifest();
  page = await connectTarget(target.webSocketDebuggerUrl);
  browser = await connectBrowser();
  const system = await browser.send('SystemInfo.getInfo');
  if (requiredBrowserArg && !exactArgument(system.commandLine ?? '', requiredBrowserArg)) {
    throw new Error(`selected CDP browser is missing required argument ${requiredBrowserArg}`);
  }
  const initialProcesses = (await browser.send('SystemInfo.getProcessInfo')).processInfo;
  for (const process of initialProcesses) {
    if (!Number.isFinite(process.cpuTime)) {
      throw new Error(`CDP process ${process.id}/${process.type} has no cumulative CPU counter`);
    }
  }
  if (!initialProcesses.some((process) => process.type === 'browser')) {
    throw new Error('CDP process list has no browser process');
  }
  if (!initialProcesses.some((process) => process.type === 'renderer')) {
    throw new Error('CDP process list has no renderer process');
  }
  await page.send('Performance.enable');
  let monitorSample = await evaluate(page, installExpression);
  let previousMetrics = metricMap(await page.send('Performance.getMetrics'));
  let previousProcesses = initialProcesses;
  let previousAt = Date.now();
  let nextAt = previousAt + everyMs;
  let consecutiveErrors = 0;
  const origin = pageOrigin(target.url);
  writeRow({
    v: 1,
    kind: 'session-start',
    sessionId,
    utc: new Date().toISOString(),
    cdpPort: Number(PORT),
    targetId: target.id,
    instanceId: manifest.instanceId,
    pageMarker: manifest.target.pageMarker,
    title: target.title,
    origin,
    browserPid: initialProcesses.find((process) => process.type === 'browser')?.id ?? null,
    logicalProcessors: monitorSample.hardwareConcurrency,
    sampleMs: everyMs,
    monitor: { busyEveryFrames: 16, histogramBucketMs: 0.25, suspendGapMs: 2000 },
  });
  console.log(
    `realuse: session=${sessionId} target=${JSON.stringify(target.title)} origin=${origin} `
    + `browser=${initialProcesses.find((process) => process.type === 'browser')?.id} out=${output}`,
  );

  const endAt = previousAt + seconds * 1000;
  while (!stopping && Date.now() < endAt) {
    await sleep(Math.max(0, nextAt - Date.now()));
    if (stopping) break;
    const now = Date.now();
    try {
      let rearmed = false;
      monitorSample = await evaluate(page, collectExpression);
      if (monitorSample === null) {
        monitorSample = await evaluate(page, installExpression);
        rearmed = true;
      }
      const currentMetrics = metricMap(await page.send('Performance.getMetrics'));
      const currentProcesses = (await browser.send('SystemInfo.getProcessInfo')).processInfo;
      const elapsedMs = now - previousAt;
      const cpu = processCpuDelta(
        previousProcesses,
        currentProcesses,
        elapsedMs,
        monitorSample.hardwareConcurrency,
      );
      writeRow({
        v: 1,
        kind: 'sample',
        sessionId,
        utc: new Date(now).toISOString(),
        elapsedMs,
        rearmed,
        pageState: {
          visible: monitorSample.visible,
          focused: monitorSample.focused,
        },
        frames: {
          focused: monitorSample.focusedFrames,
          other: monitorSample.otherFrames,
          suspensions: monitorSample.suspensions,
          suspendedMs: round(monitorSample.suspendedMs),
        },
        busy: {
          ...monitorSample.busy,
          budgetsMs: [6, 8, 16],
          withinBudgets: monitorSample.busyWithinBudgets,
          dropped: monitorSample.busyDropped,
          discarded: monitorSample.busyDiscarded,
        },
        observers: {
          longTasks: monitorSample.longTasks,
          longAnimationFrames: monitorSample.loafs,
          slowEvents: monitorSample.slowEvents,
          unavailable: monitorSample.observerErrors,
        },
        chromium: pageMetrics(previousMetrics, currentMetrics),
        cpu: {
          ...cpu,
          cpuSeconds: round(cpu.cpuSeconds),
          rawPercent: round(cpu.rawPercent),
          normalizedPercent: round(cpu.normalizedPercent),
          byGroupPercent: Object.fromEntries(
            Object.entries(cpu.byGroupPercent).map(([name, value]) => [name, round(value)]),
          ),
        },
      });
      previousMetrics = currentMetrics;
      previousProcesses = currentProcesses;
      previousAt = now;
      consecutiveErrors = 0;
    } catch (error) {
      consecutiveErrors += 1;
      writeRow({
        v: 1,
        kind: 'sample-error',
        sessionId,
        utc: new Date().toISOString(),
        consecutiveErrors,
        error: String(error?.message ?? error),
      });
      if (consecutiveErrors >= 3) throw error;
    }
    nextAt += everyMs;
    if (nextAt < Date.now()) nextAt = Date.now() + everyMs;
  }
  writeRow({
    v: 1,
    kind: 'session-end',
    sessionId,
    utc: new Date().toISOString(),
    reason: stopping ? 'signal' : 'duration',
  });
} finally {
  if (page) {
    try {
      await evaluate(page, stopExpression);
    } catch (error) {
      console.warn(`realuse: page monitor cleanup failed: ${error.message}`);
    }
  }
  for (const connection of [page, browser]) {
    try {
      connection?.close();
    } catch (error) {
      console.warn(`realuse: CDP cleanup failed: ${error.message}`);
    }
  }
  await sleep(50);
}
