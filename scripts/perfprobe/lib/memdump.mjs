// memory-infra dumps over CDP: one Tracing round trip parsed into per-process allocator rows.
// Only one tracing session can exist on the browser target at a time, so a memory dump and a
// devtools.timeline trace (probe frames) cannot run together. Run them one after the other.
import { readStream, sleep } from './cdp.mjs';

const hex = (v) => parseInt(v || '0', 16);

// The dump is filled in asynchronously by every process, so wait before ending the session.
export async function takeMemoryDump(browser, level = 'detailed') {
  browser.events.length = 0;
  await browser.send('Tracing.start', {
    traceConfig: { includedCategories: ['disabled-by-default-memory-infra'], memoryDumpConfig: { triggers: [] } },
    transferMode: 'ReturnAsStream',
  });
  await browser.send('Tracing.requestMemoryDump', { levelOfDetail: level });
  await sleep(level === 'detailed' ? 1500 : 800);
  const done = new Promise((res) => {
    const t = setInterval(() => {
      const e = browser.events.find((x) => x.method === 'Tracing.tracingComplete');
      if (e) { clearInterval(t); res(e.params); }
    }, 100);
  });
  await browser.send('Tracing.end');
  const complete = await done;
  const data = await readStream(browser, complete.stream);
  return parseDump(data);
}

export function parseDump(text) {
  const trace = JSON.parse(text);
  const byPid = {};
  for (const e of (trace.traceEvents || [])) {
    if (e.ph !== 'v') continue;
    const d = e.args?.dumps;
    if (!d) continue;
    const proc = byPid[e.pid] || (byPid[e.pid] = { pid: e.pid, privMB: null, allocators: {} });
    if (d.process_totals) proc.privMB = +(hex(d.process_totals.private_footprint_bytes) / 1048576).toFixed(1);
    for (const [name, a] of Object.entries(d.allocators || {})) {
      const at = a.attrs || {};
      proc.allocators[name] = {
        size: at.size ? hex(at.size.value) : null,
        effectiveSize: at.effective_size ? hex(at.effective_size.value) : null,
        allocatedObjects: at.allocated_objects_size ? hex(at.allocated_objects_size.value) : null,
        objectCount: at.object_count ? hex(at.object_count.value) : null,
        raw: at, // every provider attr (committed/resident/fragmentation live only here)
      };
    }
  }
  return { byPid };
}

// Prefers size and falls back to effective_size, which is what the memory-infra UI shows per node.
export function allocatorMB(proc, name) {
  const a = proc.allocators[name];
  if (!a) return null;
  const b = a.size ?? a.effectiveSize;
  return b == null ? null : +(b / 1048576).toFixed(1);
}

// A light dump omits blink_gc/main/allocated_objects, but blink_gc/main/heap carries the same
// number as its allocated_objects_size attribute, so poll loops can read it at either level.
export function allocatedObjectsMB(proc) {
  const direct = proc.allocators['blink_gc/main/allocated_objects'];
  const b = direct ? (direct.size ?? direct.effectiveSize) : proc.allocators['blink_gc/main/heap']?.allocatedObjects;
  return b == null ? null : +(b / 1048576).toFixed(1);
}

export const isRenderer = (proc) => 'blink_gc' in proc.allocators;
export const isGpu = (proc) => !isRenderer(proc)
  && ('gpu/shared_images' in proc.allocators || 'cc/resource_memory' in proc.allocators);
export const role = (proc) => (isRenderer(proc) ? 'renderer' : isGpu(proc) ? 'gpu' : 'other');

const CLASS_PREFIX = 'blink_objects/blink_gc/main/';

// Live objects plus whatever garbage has not been swept yet, so counts read high right after churn.
export function blinkClassRows(proc) {
  const rows = [];
  for (const [name, a] of Object.entries(proc.allocators)) {
    if (!name.startsWith(CLASS_PREFIX)) continue;
    const b = a.size ?? a.effectiveSize;
    if (b == null) continue;
    rows.push({ bytes: b, count: a.objectCount, name: name.slice(CLASS_PREFIX.length).replace(/ \(0x[0-9a-f]+\)$/, '') });
  }
  rows.sort((x, y) => y.bytes - x.bytes);
  return rows;
}

export function ccTileTotal(proc) {
  let count = 0, bytes = 0;
  for (const [name, a] of Object.entries(proc.allocators)) {
    if (!/^cc\/tile_memory\/provider_\d+\/resource_/.test(name)) continue;
    const b = a.size ?? a.effectiveSize;
    if (b == null) continue;
    count++;
    bytes += b;
  }
  return { count, bytes };
}
