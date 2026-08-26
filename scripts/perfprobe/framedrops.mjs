// Passive frame-delivery sampler: rAF timestamp gaps + LoAF entries with
// script attribution, over N seconds (default 60). Read-only.
import { connectPage, done, evaluate, sleep } from './lib/cdp.mjs';
const secs = Number(process.argv[2] ?? 60);
const page = await connectPage();
try {
  await evaluate(page, `(()=>{
    const S = window.__frameDrops = { deltas: [], gaps: [], loafs: [], t0: performance.now(), running: true };
    let last = 0;
    const tick = (ts) => {
      if (last) S.deltas.push(ts - last);
      last = ts;
      if (S.running) requestAnimationFrame(tick);
    };
    requestAnimationFrame(tick);
    try {
      S.obs = new PerformanceObserver((list) => {
        for (const e of list.getEntries()) {
          S.loafs.push({ t: e.startTime, dur: e.duration, block: e.blockingDuration,
            scripts: e.scripts.map(s => ({ inv: s.invoker, src: (s.sourceURL||'').split('/').pop()+':'+s.sourceCharPosition, dur: s.duration })) });
        }
      });
      S.obs.observe({ type: 'long-animation-frame', buffered: false });
    } catch (e) { S.loafErr = String(e); }
    return 'armed';
  })()`);
  await sleep(secs * 1000);
  const out = JSON.parse(await evaluate(page, `(()=>{
    const S = window.__frameDrops; S.running = false; S.obs?.disconnect();
    const d = S.deltas.slice().sort((a,b)=>a-b);
    const med = d[Math.floor(d.length/2)] ?? 0;
    const gaps = [];
    let t = S.t0;
    for (const delta of S.deltas) { t += delta; if (delta > med*1.6) gaps.push({ at: Math.round(t - S.t0), ms: Math.round(delta*10)/10 }); }
    gaps.sort((a,b)=>b.ms-a.ms);
    delete window.__frameDrops;
    return JSON.stringify({ frames: S.deltas.length, medianMs: Math.round(med*100)/100,
      p99: Math.round((d[Math.floor(d.length*0.99)]??0)*10)/10, max: Math.round((d[d.length-1]??0)*10)/10,
      gapsOver1_6x: gaps.length, worstGaps: gaps.slice(0,12), loafErr: S.loafErr,
      loafs: S.loafs.slice(0,15) });
  })()`));
  console.log(JSON.stringify(out, null, 1));
} finally { await done([page]); }
