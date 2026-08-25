// A/B the active-use memory-trim candidates: main-thread stall + memory returned per intervention.
//
// The idle trim (app_webview_trim.go) only fires between turns. This probe measures what each
// candidate would COST if fired during active use, and what it RETURNS, so the active-use
// decision is made on numbers:
//   gc        HeapProfiler.collectGarbage       — full memory-reducing GC (the idle-trim mechanism)
//   moderate  Memory.simulatePressureNotification level=moderate
//   critical  Memory.simulatePressureNotification level=critical
//   purge     Memory.forciblyPurgeJavaScriptMemory
//
// For each: renderer memdump before/after (private, blink_gc, v8, malloc, partition_alloc),
// intervention wall time, and a 10s stall window bracketing it — a 4ms-interval timer records
// main-thread gaps (robust to an occluded window, where rAF throttles) plus LoAF entries.
// A control window with no intervention runs first so the soak rig's own baseline jank is known.
//
// usage: AO_CDP_PORT=9224 scripts/perfprobe/probe activetrim [--only gc,critical] [--window 10]
import { connectPage, connectBrowser, sleep } from './lib/cdp.mjs';
import { takeMemoryDump, isRenderer, allocatorMB } from './lib/memdump.mjs';

const args = process.argv.slice(2);
const arg = (name, dflt) => {
  const i = args.indexOf(name);
  return i >= 0 ? args[i + 1] : dflt;
};
const WINDOW_S = +arg('--window', 10);
const only = arg('--only', '').split(',').filter(Boolean);

const page = await connectPage();
const browser = await connectBrowser();
await page.send('Runtime.enable');

const evalIn = async (expr) => {
  const r = await page.send('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true });
  if (r.exceptionDetails) throw new Error('page eval failed: ' + JSON.stringify(r.exceptionDetails.exception));
  return r.result.value;
};

// Stall recorder: a 4ms-cadence timer whose observed gap above cadence is main-thread occupancy.
// Buffers into a global; startStallWindow/endStallWindow bracket one measurement.
await evalIn(`(() => {
  if (globalThis.__aoStall) return 'already-installed';
  const s = globalThis.__aoStall = { gaps: [], loaf: [], running: false, last: 0, timer: 0 };
  s.start = () => {
    s.gaps.length = 0; s.loaf.length = 0; s.running = true; s.last = performance.now();
    s.timer = setInterval(() => {
      const now = performance.now();
      const gap = now - s.last;
      s.last = now;
      if (gap > 16) s.gaps.push(Math.round(gap));
    }, 4);
  };
  s.stop = () => {
    s.running = false; clearInterval(s.timer);
    const gaps = s.gaps.slice().sort((a, b) => b - a);
    return {
      maxGapMs: gaps[0] || 0,
      gapsOver50: gaps.filter((g) => g > 50).length,
      gapsOver100: gaps.filter((g) => g > 100).length,
      top5: gaps.slice(0, 5),
      loaf: s.loaf.slice(),
    };
  };
  try {
    new PerformanceObserver((list) => {
      for (const e of list.getEntries()) {
        if (s.running) s.loaf.push({ dur: Math.round(e.duration), block: Math.round(e.blockingDuration || 0) });
      }
    }).observe({ type: 'long-animation-frame', buffered: false });
  } catch { /* LoAF unsupported: gaps still carry the verdict */ }
  return 'installed';
})()`);

const rendererRow = (dump) => {
  const r = Object.values(dump.byPid).find(isRenderer);
  if (!r) throw new Error('no renderer row in dump');
  const mb = (name) => allocatorMB(r, name) ?? 0;
  return {
    privMB: r.privMB,
    blink_gc: mb('blink_gc'),
    v8: mb('v8'),
    malloc: mb('malloc'),
    partition_alloc: mb('partition_alloc'),
    cc: mb('cc'),
  };
};

const fmtRow = (m) =>
  `priv ${m.privMB}MB  blink_gc ${m.blink_gc}  v8 ${m.v8}  malloc ${m.malloc}  pa ${m.partition_alloc}  cc ${m.cc}`;

const interventions = [
  { name: 'control', fire: async () => {} },
  {
    name: 'gc',
    fire: async () => {
      await page.send('HeapProfiler.enable');
      await page.send('HeapProfiler.collectGarbage');
      await page.send('HeapProfiler.disable');
    },
  },
  {
    name: 'moderate',
    fire: () => page.send('Memory.simulatePressureNotification', { level: 'moderate' }),
  },
  {
    name: 'critical',
    fire: () => page.send('Memory.simulatePressureNotification', { level: 'critical' }),
  },
  {
    name: 'purge',
    fire: () => page.send('Memory.forciblyPurgeJavaScriptMemory'),
  },
];

for (const iv of interventions) {
  if (only.length && iv.name !== 'control' && !only.includes(iv.name)) continue;

  const before = rendererRow(await takeMemoryDump(browser));
  await evalIn('__aoStall.start()');
  await sleep((WINDOW_S / 2) * 1000);
  const t0 = Date.now();
  let fireErr = null;
  try {
    await iv.fire();
  } catch (e) {
    fireErr = e.message || String(e);
  }
  const fireMs = Date.now() - t0;
  await sleep((WINDOW_S / 2) * 1000);
  const stall = await evalIn('JSON.stringify(__aoStall.stop())').then(JSON.parse);
  const after = rendererRow(await takeMemoryDump(browser));

  console.log(`== ${iv.name}${fireErr ? `  (FIRE FAILED: ${fireErr})` : ''}`);
  console.log(`   before  ${fmtRow(before)}`);
  console.log(`   after   ${fmtRow(after)}`);
  console.log(
    `   delta   priv ${(after.privMB - before.privMB).toFixed(1)}MB  blink_gc ${(after.blink_gc - before.blink_gc).toFixed(1)}  v8 ${(after.v8 - before.v8).toFixed(1)}  malloc ${(after.malloc - before.malloc).toFixed(1)}  pa ${(after.partition_alloc - before.partition_alloc).toFixed(1)}`,
  );
  console.log(
    `   cost    call ${fireMs}ms  maxGap ${stall.maxGapMs}ms  gaps>50 ${stall.gapsOver50}  gaps>100 ${stall.gapsOver100}  top5 [${stall.top5.join(', ')}]  loaf ${JSON.stringify(stall.loaf.slice(0, 5))}`,
  );
  await sleep(15000); // recovery: let the soak's own churn resettle before the next candidate
}

process.exit(0);
