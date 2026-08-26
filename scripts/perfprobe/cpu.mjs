// V8 CPU profile of the page plus Performance.getMetrics deltas over the same window.
// usage: probe cpu [seconds=60] [label]
import { mkdirSync, writeFileSync } from 'node:fs';
import { connectPage, done, sleep } from './lib/cdp.mjs';
import { pad, ms } from './lib/format.mjs';
import { createFrameResolver } from './lib/sourcemap.mjs';

const SECS = +(process.argv[2] || 60);
const LABEL = process.argv[3] || 'run';
const OUT = process.env.AO_PERFPROBE_OUT || '.';
mkdirSync(OUT, { recursive: true });

const c = await connectPage();
await c.send('Performance.enable');
await c.send('Profiler.enable');
await c.send('Profiler.setSamplingInterval', { interval: 500 });
const m0 = Object.fromEntries((await c.send('Performance.getMetrics')).metrics.map((m) => [m.name, m.value]));
await c.send('Profiler.start');
await sleep(SECS * 1000);
const { profile } = await c.send('Profiler.stop');
const file = `${OUT}\\cpu-${LABEL}.cpuprofile`;
writeFileSync(file, JSON.stringify(profile));
const m1 = Object.fromEntries((await c.send('Performance.getMetrics')).metrics.map((m) => [m.name, m.value]));
c.close();

const byId = new Map(profile.nodes.map((n) => [n.id, n]));
const parent = new Map();
for (const n of profile.nodes) for (const ch of n.children || []) parent.set(ch, n.id);
// One resolver over every bundle URL in the profile (hidden maps; a build
// without AO_SOURCEMAP resolves nothing and frames print raw).
const resolve = await createFrameResolver(profile.nodes.map((n) => n.callFrame.url).filter(Boolean));
const selfUs = new Map();
for (let i = 0; i < profile.samples.length; i++) {
  const id = profile.samples[i];
  selfUs.set(id, (selfUs.get(id) || 0) + (profile.timeDeltas[i] || 0));
}
const total = [...selfUs.values()].reduce((a, b) => a + b, 0);
const mappedOf = (n) => (n.callFrame.url ? resolve(n.callFrame.url, n.callFrame.lineNumber, n.callFrame.columnNumber) : null);
const file_ = (n) => {
  const m = mappedOf(n);
  if (m) return m.source;
  return (n.callFrame.url || '').split('/').pop() || '(native)';
};
const key = (n) => {
  const m = mappedOf(n);
  if (m) return `${n.callFrame.functionName || m.name || '(anon)'} ${m.source}:${m.line}`;
  return `${n.callFrame.functionName || '(anon)'} ${file_(n)}:${n.callFrame.lineNumber + 1}`;
};
const selfBy = new Map(), inclBy = new Map(), fileBy = new Map();
let idle = 0, gc = 0, program = 0;
for (const [id, us] of selfUs) {
  const n = byId.get(id);
  const fn = n.callFrame.functionName;
  if (fn === '(idle)') { idle += us; continue; }
  if (fn === '(garbage collector)') gc += us; else if (fn === '(program)') program += us;
  const k = key(n);
  selfBy.set(k, (selfBy.get(k) || 0) + us);
  fileBy.set(file_(n), (fileBy.get(file_(n)) || 0) + us);
  const seen = new Set();
  let cur = id;
  while (cur !== undefined) {
    const nn = byId.get(cur);
    const kk = key(nn);
    if (!seen.has(kk)) { seen.add(kk); inclBy.set(kk, (inclBy.get(kk) || 0) + us); }
    cur = parent.get(cur);
  }
}
const busy = total - idle;
console.log(`== cpu profile ${LABEL} ${SECS}s: wall ${ms(total)}ms, busy ${ms(busy)}ms (${(100 * busy / total).toFixed(1)}%), gc ${ms(gc)}ms, program ${ms(program)}ms; raw profile saved as ${file}`);
const top = (map, n) => [...map.entries()].sort((a, b) => b[1] - a[1]).slice(0, n);
console.log('-- self time by function');
for (const [k, us] of top(selfBy, 35)) console.log(`${pad(ms(us))}ms ${pad((100 * us / busy).toFixed(1), 5)}%  ${k}`);
console.log('-- inclusive time by function');
for (const [k, us] of top(inclBy, 35)) console.log(`${pad(ms(us))}ms ${pad((100 * us / busy).toFixed(1), 5)}%  ${k}`);
console.log('-- callers of the hot native reads (self time attributed to the JS frame that called them)');
for (const hot of ['get scrollTop', 'get scrollHeight', 'getBoundingClientRect', 'get offsetHeight', 'setProperty', 'get style', 'querySelectorAll', 'requestAnimationFrame']) {
  const callers = new Map();
  for (const [id, us] of selfUs) {
    const n = byId.get(id);
    if (n.callFrame.functionName !== hot) continue;
    let p = parent.get(id);
    let pn = p !== undefined ? byId.get(p) : null;
    while (pn && !pn.callFrame.url) { p = parent.get(p); pn = p !== undefined ? byId.get(p) : null; }
    const k = pn ? key(pn) : '(none)';
    callers.set(k, (callers.get(k) || 0) + us);
  }
  const rows = top(callers, 6);
  if (!rows.length) continue;
  console.log(`  ${hot}: ` + rows.map(([k, us]) => `${ms(us)}ms ${k}`).join(' | '));
}
console.log('-- self time by file');
for (const [k, us] of top(fileBy, 15)) console.log(`${pad(ms(us))}ms ${pad((100 * us / busy).toFixed(1), 5)}%  ${k}`);
console.log('-- Performance.getMetrics deltas over the window');
const d = (k) => (m1[k] ?? 0) - (m0[k] ?? 0);
const perMin = (v) => (v * 60 / SECS);
console.log(`  TaskDuration ${d('TaskDuration').toFixed(2)}s  ScriptDuration ${d('ScriptDuration').toFixed(2)}s  LayoutDuration ${d('LayoutDuration').toFixed(2)}s  RecalcStyleDuration ${d('RecalcStyleDuration').toFixed(2)}s`);
console.log(`  LayoutCount ${d('LayoutCount')} (${perMin(d('LayoutCount')).toFixed(0)}/min)  RecalcStyleCount ${d('RecalcStyleCount')} (${perMin(d('RecalcStyleCount')).toFixed(0)}/min)`);
console.log(`  Nodes ${m0.Nodes} -> ${m1.Nodes}  LayoutObjects ${m0.LayoutObjects} -> ${m1.LayoutObjects}  JSEventListeners ${m0.JSEventListeners} -> ${m1.JSEventListeners}  JSHeapUsed ${(m0.JSHeapUsedSize / 1048576).toFixed(1)} -> ${(m1.JSHeapUsedSize / 1048576).toFixed(1)}MB`);
await done([c]);
