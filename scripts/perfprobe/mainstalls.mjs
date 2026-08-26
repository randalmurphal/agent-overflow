// Passive main-thread stall watch: a 4ms timer chain measures scheduling
// drift (any task > ~6ms shows as drift) without forcing compositor frames;
// LoAF observer catches >50ms frames with attribution. Args: seconds.
import { connectPage, done, evaluate, sleep } from './lib/cdp.mjs';
const secs = Number(process.argv[2] ?? 600);
const page = await connectPage();
try {
  await evaluate(page, `(()=>{
    const S = window.__stalls = { t0: performance.now(), wall0: Date.now(), stalls: [], loafs: [], running: true, n: 0 };
    let expected = performance.now() + 4;
    const tick = () => {
      if (!S.running) return;
      const now = performance.now();
      const drift = now - expected;
      S.n++;
      if (drift > 6) S.stalls.push({ at: Math.round(now - S.t0), ms: Math.round(drift*10)/10 });
      expected = now + 4;
      setTimeout(tick, 4);
    };
    setTimeout(tick, 4);
    try {
      S.obs = new PerformanceObserver((l) => { for (const e of l.getEntries())
        S.loafs.push({ at: Math.round(e.startTime - S.t0), dur: Math.round(e.duration), block: Math.round(e.blockingDuration),
          scripts: e.scripts.map(s => s.invoker + '@' + (s.sourceURL||'').split('/').pop()) }); });
      S.obs.observe({ type: 'long-animation-frame', buffered: false });
    } catch (e) { S.loafErr = String(e); }
    return 'armed ' + new Date().toISOString();
  })()`);
  await sleep(secs * 1000);
  const out = await evaluate(page, `(()=>{
    const S = window.__stalls; S.running = false; S.obs?.disconnect();
    const b = { over6: 0, over12: 0, over24: 0, over40: 0 };
    for (const s of S.stalls) { if (s.ms > 6) b.over6++; if (s.ms > 12) b.over12++; if (s.ms > 24) b.over24++; if (s.ms > 40) b.over40++; }
    const worst = S.stalls.slice().sort((a,b2)=>b2.ms-a.ms).slice(0,20);
    const r = JSON.stringify({ ticks: S.n, wallStart: new Date(S.wall0).toISOString(), buckets: b, worst, loafs: S.loafs }, null, 1);
    delete window.__stalls;
    return r;
  })()`);
  console.log(out);
} finally { await done([page]); }
