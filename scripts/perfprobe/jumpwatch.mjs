// Scroll-discontinuity watch: in-page rAF sampler over every pane scroller
// ([data-testid="message-timeline-scroll"]) and every activity-run clip
// ([data-testid="activity-run-clip"]). Flags any single-frame scrollTop step
// that breaks the glide shape (big delta after small ones) and records the
// surrounding frames so the jump's owner and shape are readable offline.
// Also records the frame a tail clip leaves the DOM (the ActivityRun
// teardown handover) with the pane's scrollTop across that frame — the
// leading suspect for the run->prose half-spring jump.
// usage: probe jumpwatch [secs=120] [threshold=60]
import { writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { connectPage, done, evaluate, sleep } from './lib/cdp.mjs';

const SECS = +(process.argv[2] || 120);
const THRESH = +(process.argv[3] || 60);

const page = await connectPage();

const install = `(() => {
  if (window.__aoJumpWatch) { window.__aoJumpWatch.stop(); }
  const PANE = '[data-testid="message-timeline-scroll"]';
  const CLIP = '[data-testid="activity-run-clip"]';
  const THRESH = ${THRESH};
  const HIST = 10;               // frames of history kept per element
  const MAX_EVENTS = 300;
  const state = { frames: 0, events: [], input: 0, start: performance.now() };
  // reader-input guard: on the rig nothing touches the window, but record anyway
  const onInput = () => { state.input = performance.now(); };
  for (const ev of ['wheel', 'keydown', 'pointerdown', 'touchstart'])
    window.addEventListener(ev, onInput, { capture: true, passive: true });

  // element -> { hist: [{t, top, sh, ch}...], kind, paneIdx, owner }
  const tracked = new Map();
  let raf = 0;
  const push = (e) => { if (state.events.length < MAX_EVENTS) state.events.push(e); };

  const sampleOne = (el, kind, paneIdx, now) => {
    let rec = tracked.get(el);
    if (!rec) { rec = { hist: [], kind, paneIdx, seen: now }; tracked.set(el, rec); }
    rec.alive = now;
    rec.owner = kind === 'clip' ? (el.getAttribute('data-scroll-owner') || '') : '';
    const s = { t: now, top: el.scrollTop, sh: el.scrollHeight, ch: el.clientHeight };
    const h = rec.hist;
    const prev = h[h.length - 1];
    if (prev) {
      const d = s.top - prev.top;
      const dh = s.sh - prev.sh;
      if (Math.abs(d) >= THRESH) {
        // glide-shape check: median of the prior deltas
        const deltas = [];
        for (let i = 1; i < h.length; i++) deltas.push(Math.abs(h[i].top - h[i - 1].top));
        deltas.sort((a, b) => a - b);
        const med = deltas.length ? deltas[deltas.length >> 1] : 0;
        push({
          t: +(now - state.start).toFixed(0), kind, paneIdx, ev: 'step',
          delta: +d.toFixed(1), medPrev: +med.toFixed(1), dh: +dh.toFixed(1),
          top: +s.top.toFixed(1), bottomGap: +(s.sh - s.ch - s.top).toFixed(1),
          owner: rec.owner,
          sinceInputMs: state.input ? +(now - state.input).toFixed(0) : -1,
          hist: h.slice(-8).map((x) => +x.top.toFixed(1)),
        });
      }
    }
    h.push(s);
    if (h.length > HIST) h.shift();
  };

  const tick = () => {
    const now = performance.now();
    state.frames++;
    const panes = document.querySelectorAll(PANE);
    panes.forEach((p, i) => sampleOne(p, 'pane', i, now));
    document.querySelectorAll(CLIP).forEach((c) => {
      let paneIdx = -1;
      panes.forEach((p, i) => { if (p.contains(c)) paneIdx = i; });
      sampleOne(c, 'clip', paneIdx, now);
    });
    // teardown detection: a tracked clip that produced no sample this frame is gone
    for (const [el, rec] of tracked) {
      if (rec.alive === now) continue;
      if (rec.kind === 'clip') {
        const last = rec.hist[rec.hist.length - 1] || {};
        const pane = rec.paneIdx >= 0 ? tracked.get(panes[rec.paneIdx]) : null;
        push({
          t: +(now - state.start).toFixed(0), kind: 'clip', paneIdx: rec.paneIdx, ev: 'gone',
          lastTop: +(last.top ?? -1).toFixed(1),
          lastBottomGap: last.sh != null ? +(last.sh - last.ch - last.top).toFixed(1) : -1,
          owner: rec.owner,
          paneHist: pane ? pane.hist.slice(-8).map((x) => +x.top.toFixed(1)) : [],
          clipHist: rec.hist.slice(-8).map((x) => +x.top.toFixed(1)),
        });
      }
      tracked.delete(el);
    }
    raf = requestAnimationFrame(tick);
  };
  raf = requestAnimationFrame(tick);
  window.__aoJumpWatch = {
    state,
    stop() {
      cancelAnimationFrame(raf);
      for (const ev of ['wheel', 'keydown', 'pointerdown', 'touchstart'])
        window.removeEventListener(ev, onInput, { capture: true });
      delete window.__aoJumpWatch;
    },
    dump() {
      return JSON.stringify({ frames: state.frames, secs: +((performance.now() - state.start) / 1000).toFixed(1), events: state.events });
    },
  };
  return 'installed';
})()`;

try {
  console.log(await evaluate(page, install), `— watching ${SECS}s, threshold ${THRESH}px/frame`);
  await sleep(SECS * 1000);
  const raw = await evaluate(page, `window.__aoJumpWatch ? window.__aoJumpWatch.dump() : '{"gone":true}'`);
  await evaluate(page, `window.__aoJumpWatch && window.__aoJumpWatch.stop()`);
  const data = JSON.parse(raw);
  if (data.gone) { console.error('probe: watcher vanished (page reloaded?)'); process.exit(1); }

  const outDir = process.env.AO_PERFPROBE_OUT || process.env.TEMP || '.';
  const out = join(outDir, `ao-jumpwatch-${Date.now()}.json`);
  writeFileSync(out, JSON.stringify(data, null, 1));

  const steps = data.events.filter((e) => e.ev === 'step');
  const gones = data.events.filter((e) => e.ev === 'gone');
  console.log(`${data.frames} frames over ${data.secs}s; ${steps.length} steps >= ${THRESH}px, ${gones.length} clip teardowns`);
  const byKind = {};
  for (const s of steps) byKind[`${s.kind}`] = (byKind[`${s.kind}`] || 0) + 1;
  console.log('steps by element:', JSON.stringify(byKind));
  for (const s of steps.slice(0, 40)) {
    console.log(` step  t=${s.t}ms ${s.kind}#${s.paneIdx} delta=${s.delta} medPrev=${s.medPrev} dh=${s.dh} bottomGap=${s.bottomGap} owner=${s.owner || '-'} hist=[${s.hist.join(',')}]`);
  }
  for (const g of gones.slice(0, 40)) {
    console.log(` gone  t=${g.t}ms clip@pane${g.paneIdx} lastBottomGap=${g.lastBottomGap} owner=${g.owner || '-'} clipHist=[${g.clipHist.join(',')}] paneHist=[${g.paneHist.join(',')}]`);
  }
  console.log(`full JSON: ${out}`);
} finally {
  await done([page]);
}
