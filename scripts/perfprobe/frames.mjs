// devtools.timeline trace of the renderer: where frame time goes, style recalcs, forced layouts.
// usage: probe frames [seconds=20] [label]  |  probe frames --file <wsl path to a saved trace.json>
// offline-with --file
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { pad, ms, fail } from './lib/format.mjs';
import { createFrameResolver } from './lib/sourcemap.mjs';

const args = process.argv.slice(2);
const fileIdx = args.indexOf('--file');
let data, LABEL, SECS = 0;

if (fileIdx >= 0) {
  const path = args[fileIdx + 1];
  if (!path) fail('probe: --file needs the path of a saved trace');
  try { data = readFileSync(path, 'utf8'); } catch (e) { fail(`probe: cannot read ${path}: ${e.message}`); }
  LABEL = path.split(/[\\/]/).pop();
} else {
  const { connectBrowser, readStream, sleep } = await import('./lib/cdp.mjs');
  SECS = +(args[0] || 20);
  LABEL = args[1] || 'run';
  const OUT = process.env.AO_PERFPROBE_OUT || '.';
  mkdirSync(OUT, { recursive: true });
  const b = await connectBrowser();
  const cats = ['devtools.timeline', 'disabled-by-default-devtools.timeline', 'disabled-by-default-devtools.timeline.frame', 'disabled-by-default-devtools.timeline.invalidationTracking', 'disabled-by-default-devtools.timeline.stack', 'blink.user_timing', 'v8.execute', 'disabled-by-default-v8.gc'];
  await b.send('Tracing.start', { traceConfig: { includedCategories: cats, excludedCategories: ['*'] }, transferMode: 'ReturnAsStream' });
  await sleep(SECS * 1000);
  const done = b.waitFor('Tracing.tracingComplete');
  await b.send('Tracing.end');
  const complete = await done;
  data = await readStream(b, complete.stream);
  b.close();
  const out = `${OUT}\\trace-${LABEL}.json`;
  writeFileSync(out, data);
  console.log(`raw trace saved as ${out}`);
}

const trace = JSON.parse(data);
const evs = trace.traceEvents || trace;
const perThread = new Map();
for (const e of evs) if (e.ph === 'X' && (e.name === 'UpdateLayoutTree' || e.name === 'Layout' || e.name === 'FunctionCall')) { const k = `${e.pid}:${e.tid}`; perThread.set(k, (perThread.get(k) || 0) + 1); }
const [mainKey] = [...perThread.entries()].sort((a, b) => b[1] - a[1])[0] || [];
if (!mainKey) fail('probe: the trace has no renderer main-thread events');
const [pid, tid] = mainKey.split(':').map(Number);
const main = evs.filter((e) => e.pid === pid && e.tid === tid && e.ph === 'X' && e.dur);
main.sort((a, b) => a.ts - b.ts);
let t0 = Infinity, t1 = 0;
for (const e of main) { if (e.ts < t0) t0 = e.ts; if (e.ts + e.dur > t1) t1 = e.ts + e.dur; }
// A saved trace carries no window length, so take it from the span of main-thread events.
if (!SECS) SECS = Math.max(1, (t1 - t0) / 1e6);
const sum = (xs) => xs.reduce((a, b) => a + b, 0);
const top = (map, n, by = (v) => v) => [...map.entries()].sort((a, b) => by(b[1]) - by(a[1])).slice(0, n);
// One resolver over every bundle URL in the trace (hidden maps served
// beside the assets; offline --file runs or builds without AO_SOURCEMAP
// resolve nothing and frames print raw). Unlike Profiler and HeapProfiler
// call frames, timeline stack frames and FunctionCall data use 1-based line
// AND column coordinates. Passing those through unchanged shifts every frame
// into an unrelated source-map segment on a minified multi-line bundle.
const traceUrls = new Set();
for (const e of main) {
  if (e.args?.data?.url) traceUrls.add(e.args.data.url);
  for (const f of e.args?.beginData?.stackTrace || []) if (f.url) traceUrls.add(f.url);
}
const resolve = await createFrameResolver([...traceUrls]);
const traceCoordinate0 = (value) => Math.max(0, (value || 1) - 1);
const label = (fn, url, line1, col1) => {
  const m = url
    ? resolve(url, traceCoordinate0(line1), traceCoordinate0(col1))
    : null;
  if (m) return `${fn || m.name || '(anon)'} ${m.source}:${m.line}`;
  return `${fn || '(anon)'} ${(url || '').split('/').pop()}:${line1}`;
};
const frameOf = (st) => (st && st[0] ? label(st[0].functionName, st[0].url, st[0].lineNumber, st[0].columnNumber) : '');

const agg = new Map();
for (const e of main) { const a = agg.get(e.name) || { n: 0, us: 0, max: 0 }; a.n++; a.us += e.dur; a.max = Math.max(a.max, e.dur); agg.set(e.name, a); }
console.log(`== frame trace ${LABEL} ${SECS.toFixed(0)}s: main thread ${mainKey}, ${main.length} X events`);
console.log('-- time by event name');
for (const [name, a] of top(agg, 22, (v) => v.us)) console.log(`${pad(ms(a.us))}ms ${pad(a.n, 6)}x  max ${pad(ms(a.max), 7)}ms  ${name}`);

const perSec = new Map();
for (const e of main) if (e.name === 'RunTask') { const s = Math.floor((e.ts - t0) / 1e6); perSec.set(s, (perSec.get(s) || 0) + e.dur); }
const secs = [...perSec.entries()].sort((a, b) => b[1] - a[1]);
console.log(`-- main-thread busy: mean ${ms(sum([...perSec.values()]) / Math.max(1, SECS))}ms/s; busiest ${secs.slice(0, 4).map(([s, us]) => `t+${s}s=${ms(us)}`).join('  ')}`);
const frames = main.filter((e) => e.name === 'BeginMainThreadFrame').length;
console.log(`-- main-thread frames: ${frames} (${(frames / SECS).toFixed(1)}/s)`);

const recalcs = main.filter((e) => e.name === 'UpdateLayoutTree' && e.args?.elementCount);
const enclosingJs = (e) => {
  let best = null;
  for (const j of main) {
    if (j.ts > e.ts) break;
    if (!['FunctionCall', 'TimerFire', 'FireAnimationFrame', 'EventDispatch', 'RunMicrotasks', 'FireIdleCallback'].includes(j.name)) continue;
    if (j.ts <= e.ts && j.ts + j.dur >= e.ts + e.dur && (!best || j.dur < best.dur)) best = j;
  }
  return best;
};
const jsLabel = (j) => {
  if (!j) return '(frame-time, no JS on stack)';
  const d = j.args?.data || {};
  if (j.name === 'FunctionCall') return `FunctionCall ${label(d.functionName, d.url, d.lineNumber, d.columnNumber)}`;
  if (j.name === 'EventDispatch') return `EventDispatch ${d.type}`;
  if (j.name === 'TimerFire') return `TimerFire #${d.timerId}`;
  return j.name;
};
console.log(`-- style recalc: ${recalcs.length} passes, ${sum(recalcs.map((e) => e.args.elementCount))} elements, ${ms(sum(recalcs.map((e) => e.dur)))}ms; biggest:`);
for (const e of [...recalcs].sort((a, b) => b.args.elementCount - a.args.elementCount).slice(0, 8)) console.log(`  ${pad(e.args.elementCount, 6)} el ${pad(ms(e.dur), 6)}ms t+${((e.ts - t0) / 1e6).toFixed(2)}s  ${frameOf(e.args.beginData?.stackTrace) || jsLabel(enclosingJs(e))}`);
const recalcByCause = new Map();
for (const e of recalcs) { const k = frameOf(e.args.beginData?.stackTrace) || jsLabel(enclosingJs(e)); const a = recalcByCause.get(k) || { n: 0, el: 0, us: 0 }; a.n++; a.el += e.args.elementCount; a.us += e.dur; recalcByCause.set(k, a); }
console.log('-- style recalc by cause (passes, elements, time)');
for (const [k, a] of top(recalcByCause, 12, (v) => v.us)) console.log(`  ${pad(a.n, 5)}x ${pad(a.el, 7)} el ${pad(ms(a.us), 7)}ms  ${k}`);

const layouts = main.filter((e) => e.name === 'Layout');
const forced = layouts.filter((e) => e.args?.beginData?.stackTrace?.length);
const forcedBy = new Map();
for (const e of forced) { const k = frameOf(e.args.beginData.stackTrace); const a = forcedBy.get(k) || { n: 0, us: 0 }; a.n++; a.us += e.dur; forcedBy.set(k, a); }
console.log(`-- layout: ${layouts.length} passes ${ms(sum(layouts.map((e) => e.dur)))}ms, of which ${forced.length} forced from JS ${ms(sum(forced.map((e) => e.dur)))}ms; forced by:`);
for (const [k, a] of top(forcedBy, 10, (v) => v.us)) console.log(`  ${pad(a.n, 5)}x ${pad(ms(a.us), 7)}ms  ${k}`);

const fnBy = new Map();
for (const e of main) if (e.name === 'FunctionCall' && e.args?.data) { const d = e.args.data; const k = label(d.functionName, d.url, d.lineNumber, d.columnNumber); const a = fnBy.get(k) || { n: 0, us: 0, max: 0 }; a.n++; a.us += e.dur; a.max = Math.max(a.max, e.dur); fnBy.set(k, a); }
console.log('-- JS entry points by inclusive main-thread time (incl. forced style/layout inside)');
for (const [k, a] of top(fnBy, 18, (v) => v.us)) console.log(`  ${pad(ms(a.us), 7)}ms ${pad(a.n, 6)}x max ${pad(ms(a.max), 6)}ms  ${k}`);
const evBy = new Map();
for (const e of main) if (e.name === 'EventDispatch' && e.args?.data) { const k = e.args.data.type; const a = evBy.get(k) || { n: 0, us: 0 }; a.n++; a.us += e.dur; evBy.set(k, a); }
console.log('-- DOM events by time');
for (const [k, a] of top(evBy, 8, (v) => v.us)) console.log(`  ${pad(ms(a.us), 7)}ms ${pad(a.n, 6)}x  ${k}`);

const threadNames = new Map();
for (const e of evs) if (e.ph === 'M' && e.name === 'thread_name') threadNames.set(`${e.pid}:${e.tid}`, e.args.name);
const byThread = new Map();
for (const e of evs) {
  if (e.ph !== 'X' || !e.dur) continue;
  const k = `${e.pid}:${e.tid}`;
  const t = byThread.get(k) || { us: 0, n: 0, top: new Map() };
  t.us += e.dur; t.n++; t.top.set(e.name, (t.top.get(e.name) || 0) + e.dur);
  byThread.set(k, t);
}
console.log('-- threads by X-event time');
for (const [k, t] of [...byThread.entries()].sort((a, b) => b[1].us - a[1].us).slice(0, 12)) {
  const tt = [...t.top.entries()].sort((a, b) => b[1] - a[1]).slice(0, 4).map(([n, us]) => `${n}=${(us / 1000).toFixed(0)}ms`).join(' ');
  console.log(`  ${pad((t.us / 1000).toFixed(0), 7)}ms ${pad(t.n, 7)}x  ${threadNames.get(k) || k}  [${tt}]`);
}
const raster = evs.filter((e) => e.ph === 'X' && /RasterTask|RasterizerTaskImpl|ImageDecodeTask/.test(e.name));
console.log(`-- raster tasks: ${raster.length}, ${(raster.reduce((a, e) => a + e.dur, 0) / 1000).toFixed(0)}ms total`);
process.exit(0);
