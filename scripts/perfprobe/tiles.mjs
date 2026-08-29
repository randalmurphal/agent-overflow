// Raster tiles per composited layer from a short cc.debug trace: bytes, priority bin, layer bounds.
// usage: probe tiles
import { connectBrowser, connectPage, done, evaluate, readStream, sleep } from './lib/cdp.mjs';
import { pad, mb, fail } from './lib/format.mjs';
import { SCROLL_SURFACE_SELECTOR } from './lib/dom.mjs';

const b = await connectBrowser();
const p = await connectPage();

// DOM-side scroll-surface rects taken in the same second, so layer bounds can be matched by height.
const surfaces = await evaluate(p, `[...document.querySelectorAll(${JSON.stringify(SCROLL_SURFACE_SELECTOR)})].map((el) => {
  const r = el.getBoundingClientRect();
  return { x: Math.round(r.x), w: Math.round(r.width), h: Math.round(r.height),
    pane: el.closest('[data-pane-id]')?.getAttribute('data-pane-id') || '',
    run: el.matches('[data-testid="activity-run-clip"]') };
})`);

b.events.length = 0;
await b.send('Tracing.start', { traceConfig: { includedCategories: ['disabled-by-default-cc.debug'] }, transferMode: 'ReturnAsStream' });
await sleep(1200);
const complete = b.waitFor('Tracing.tracingComplete');
await b.send('Tracing.end');
const { stream } = await complete;
const trace = JSON.parse(await readStream(b, stream));

// cc emits one LayerTreeHostImpl snapshot per frame on this category; the last one is current.
const snaps = (trace.traceEvents || []).filter((e) => e.name === 'LayerTreeHostImpl:snapshot' && e.args?.snapshot?.active_tiles);
if (!snaps.length) fail('probe: no LayerTreeHostImpl snapshot in the trace (the compositor produced no frame in 1.2s; move the mouse over the window and rerun)');
const snap = snaps[snaps.length - 1].args.snapshot;
const state = snap.tile_manager_basic_state?.global_state || snap.activation_state?.tile_manager?.global_state || {};

const layers = {};
(function walk(o, depth) {
  if (!o || typeof o !== 'object' || depth > 8) return;
  if (Array.isArray(o)) { for (const x of o) walk(x, depth + 1); return; }
  if (o.layer_id !== undefined && o.bounds) layers[o.layer_id] = o;
  for (const v of Object.values(o)) walk(v, depth + 1);
})(snap, 0);

const per = new Map();
for (const t of snap.active_tiles) {
  const row = per.get(t.layer_id) || { n: 0, bytes: 0, bins: {}, occluded: 0 };
  row.n++;
  row.bytes += t.gpu_memory_usage || 0;
  const bin = t.combined_priority?.priority_bin || '?';
  row.bins[bin] = (row.bins[bin] || 0) + 1;
  if (t.combined_priority?.is_occluded) row.occluded++;
  per.set(t.layer_id, row);
}

const describe = (L) => {
  if (!L) return '';
  const hit = surfaces.find((surface) => surface.w === L.bounds.width && Math.abs(surface.h - L.bounds.height) <= 2);
  if (hit) return `${hit.run ? 'activity run' : 'scroll surface'} ${hit.pane} at x=${hit.x}`;
  if (L.bounds.width >= 2000) return 'root / full-width';
  return (L.compositing_reasons || []).join(',');
};

console.log(`tile budget: soft ${mb(state.soft_memory_limit_in_bytes || 0)}MB policy ${state.memory_limit_policy || '?'} viewport ${state.viewport_size?.width}x${state.viewport_size?.height}`);
let total = 0;
for (const [id, row] of [...per.entries()].sort((x, y) => y[1].bytes - x[1].bytes)) {
  const L = layers[id];
  total += row.bytes;
  const bins = Object.entries(row.bins).map(([k, v]) => `${v} ${k}`).join(', ');
  console.log(`${pad(mb(row.bytes) + 'MB', 8)} ${pad(row.n, 3)} tiles  ${pad(L ? L.bounds.width + 'x' + L.bounds.height : '?', 10)}  opaque=${L?.contents_opaque ? 'y' : 'n'}  [${bins}]${row.occluded ? ` occluded ${row.occluded}` : ''}  ${describe(L)}`);
}
console.log(`total ${mb(total)}MB in ${snap.active_tiles.length} active tiles (memory-infra cc/tile_memory also counts pooled, recently freed tiles)`);
await done([b, p]);
