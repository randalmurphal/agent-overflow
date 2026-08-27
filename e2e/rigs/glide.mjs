// Mid-glide tool-completion stutter probe: per-frame scrollTop sampler over
// N replay rounds of a scenario, reporting frame gaps and position jumps
// during motion.
//   node rigs/glide.mjs <url> <outPrefix> --instance <id> --thread "T" --scenario <file> --rounds 4
import { chromium } from '@playwright/test';
import { writeFile } from 'node:fs/promises';
import { parseArgs, makeCli, awaitRevealDrain } from './riglib.mjs';

const { positional: [url, outPrefix], flags } = parseArgs(process.argv.slice(2));
const cli = makeCli(flags.instance);
const thread = flags.thread;
const rounds = Number(flags.rounds ?? 4);
if (!url || !outPrefix || !thread || !flags.scenario) {
  console.error('usage: node rigs/glide.mjs <url> <outPrefix> --instance <id> --thread "T" --scenario <file> [--rounds N]');
  process.exit(2);
}

await cli('scenario', 'clear');
await cli('scenario', 'set', '-f', flags.scenario);

const browser = await chromium.launch({ headless: true });
const page = await browser.newPage({ viewport: { width: 1720, height: 1160 } });
await page.goto(url, { waitUntil: 'load' });
await page.waitForTimeout(3000);
await cli('ui', 'open', '--thread', thread);
await page.waitForTimeout(2500);

await page.evaluate(() => {
  window.__glideStart = () => {
    const el = document.querySelector('[data-testid="message-timeline-scroll"]');
    const s = (window.__glide = { samples: [], run: true, el });
    const loop = () => {
      if (!s.run) return;
      s.samples.push([performance.now(), s.el.scrollTop, s.el.scrollHeight]);
      requestAnimationFrame(loop);
    };
    requestAnimationFrame(loop);
    return !!el;
  };
  window.__glideStop = () => {
    const s = window.__glide;
    if (!s) return [];
    s.run = false;
    return s.samples;
  };
});

const allRounds = [];
for (let r = 1; r <= rounds; r++) {
  const armed = await page.evaluate(() => window.__glideStart());
  if (!armed) { console.error('no scroller found'); process.exit(1); }
  const send = await cli('send', '--thread', thread, '--wait', '--timeout', '240s', 'glide ' + r)
    .then(() => 'ok', (e) => 'fail: ' + (e.message?.split('\n')[0] ?? ''));
  await awaitRevealDrain(page, { capMs: 150_000 });
  await page.waitForTimeout(800);
  const samples = await page.evaluate(() => window.__glideStop());
  // analyze: motion = |dTop| > 0.1 between consecutive samples
  const deltas = [];
  for (let i = 1; i < samples.length; i++) {
    deltas.push({
      gap: samples[i][0] - samples[i - 1][0],
      dTop: samples[i][1] - samples[i - 1][1],
      t: samples[i][0],
    });
  }
  const motion = deltas.filter((d) => Math.abs(d.dTop) > 0.1);
  const speeds = motion.map((d) => Math.abs(d.dTop)).sort((a, b) => a - b);
  const median = speeds[Math.floor(speeds.length / 2)] ?? 0;
  const gapsInMotion = motion.filter((d) => d.gap > 28);
  const jumps = motion.filter((d) => median > 0 && Math.abs(d.dTop) > 6 * median && d.gap > 20);
  const worstGap = motion.reduce((m, d) => Math.max(m, d.gap), 0);
  const row = {
    round: r, send, frames: samples.length, motionFrames: motion.length,
    medianStep: +median.toFixed(1), worstGapMs: +worstGap.toFixed(1),
    gapsOver28: gapsInMotion.length,
    gapList: gapsInMotion.slice(0, 8).map((d) => +d.gap.toFixed(1)),
    jumps: jumps.length,
  };
  allRounds.push({ ...row, samples });
  console.log(JSON.stringify(row));
}
await writeFile(outPrefix + '-glide.json', JSON.stringify(allRounds));
await browser.close();
