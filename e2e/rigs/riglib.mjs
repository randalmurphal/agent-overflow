// Shared helpers for the perf rigs in this directory. See README.md.
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import fs from 'node:fs';

const run = promisify(execFile);

/** Tiny flag parser: positionals plus --key value pairs. */
export function parseArgs(argv) {
  const positional = [];
  const flags = {};
  for (let i = 0; i < argv.length; i++) {
    if (argv[i].startsWith('--')) {
      flags[argv[i].slice(2)] = argv[i + 1];
      i++;
    } else {
      positional.push(argv[i]);
    }
  }
  return { positional, flags };
}

/** ao-harness runner bound to one instance (id or data root). */
export function makeCli(instance) {
  const bin = process.env.AO_HARNESS_BIN ?? new URL('../../bin/ao-harness', import.meta.url).pathname;
  const inst = instance ? ['--instance', instance] : [];
  return (...args) =>
    run(bin, [...args, ...inst], { timeout: 300_000, maxBuffer: 32 * 1024 * 1024 });
}

/** Comma-separated thread titles/ids from --threads or AO_RIG_THREADS. */
export function requireThreads(flags, rig) {
  const raw = flags.threads ?? process.env.AO_RIG_THREADS;
  if (!raw) {
    console.error(
      `${rig}: no threads given. Pass --threads "Title A,Title B,..." (or AO_RIG_THREADS). ` +
        'Titles/ids must exist on the target instance (a clone root carries the real ones).',
    );
    process.exit(2);
  }
  return raw.split(',').map((s) => s.trim()).filter(Boolean);
}

/**
 * Install a from-thread scenario for each thread, flipped to
 * afterTurns:repeatLast. from-thread emits afterTurns:silent by design (a
 * recreated thread must not replay forever), but a rig that sends `replay`
 * to the SAME live mock sessions repeatedly wedges on turn 2+ under
 * silent (2026-08-26: every later turn timed out at 240s). Scenario rules
 * are in-memory per boot, so rigs must reinstall on every run — and rules
 * only reach mocks that register after `scenario set`, so an already-live
 * mock session keeps its old script until the instance restarts.
 */
export async function installReplayScenarios(cli, threads, tmpPrefix) {
  for (let i = 0; i < threads.length; i++) {
    const src = `${tmpPrefix}-scen${i}-src.json`;
    const out = `${tmpPrefix}-scen${i}.json`;
    await cli('scenario', 'from-thread', '--thread', threads[i], '-out', src);
    const doc = JSON.parse(fs.readFileSync(src, 'utf8'));
    doc.afterTurns = 'repeatLast';
    fs.writeFileSync(out, JSON.stringify(doc));
    await cli('scenario', 'set', '-f', out);
  }
}

/** Open the given threads as panes: first in the current pane, rest new. */
export async function openPanes(cli, page, threads) {
  await cli('ui', 'open', '--thread', threads[0]);
  await page.waitForTimeout(2000);
  for (const t of threads.slice(1)) {
    const already = await page.getByTestId('chat-header-title').filter({ hasText: t }).count();
    if (already > 0) continue;
    await cli('ui', 'open', '--thread', t, '--new-pane');
    await page.waitForTimeout(1500);
  }
  console.log('panes:', JSON.stringify(await page.getByTestId('chat-header-title').allTextContents()));
}

/** Arm an in-page LoAF counter (read+reset via sampleHeapLoaf). */
export function armLoafCounter(page) {
  return page.evaluate(() => {
    window.__loafStats = { count: 0, maxMs: 0, over30: 0, over50: 0 };
    new PerformanceObserver((list) => {
      for (const e of list.getEntries()) {
        const s = window.__loafStats;
        s.count++;
        if (e.duration >= 30) s.over30++;
        if (e.duration >= 50) s.over50++;
        if (e.duration > s.maxMs) s.maxMs = e.duration;
      }
    }).observe({ type: 'long-animation-frame', buffered: false });
  });
}

/** One heap+LoAF+DOM sample; resets the LoAF counter. Needs --enable-precise-memory-info. */
export function sampleHeapLoaf(page) {
  return page.evaluate(() => {
    const m = performance.memory;
    const s = window.__loafStats ?? {};
    const out = {
      usedMb: m ? +(m.usedJSHeapSize / 1048576).toFixed(2) : null,
      totalMb: m ? +(m.totalJSHeapSize / 1048576).toFixed(2) : null,
      loafCount: s.count ?? 0,
      loafMax: +(s.maxMs ?? 0).toFixed(1),
      loafOver30: s.over30 ?? 0,
      loafOver50: s.over50 ?? 0,
      domNodes: document.getElementsByTagName('*').length,
    };
    window.__loafStats = { count: 0, maxMs: 0, over30: 0, over50: 0 };
    return out;
  });
}

/**
 * Wait until every pane's reveal queue has drained (the reader has seen
 * the stream), so a measurement window ends on visual completion rather
 * than wire completion. READ-ONLY — nothing here may skip or rush the
 * drain.
 */
export async function awaitRevealDrain(page, { settleMs = 1500, capMs = 120_000 } = {}) {
  const t0 = Date.now();
  for (;;) {
    const drain = await page.evaluate(() => window.__aoRevealDrain?.() ?? null).catch(() => null);
    if (drain === null) { await page.waitForTimeout(10_000); return; }
    if (drain.draining === 0 && Date.now() - t0 > settleMs) return;
    if (Date.now() - t0 > capMs) return;
    await page.waitForTimeout(500);
  }
}

/** Forced memory-reducing GC with timing: splits live vs collectable. */
export async function forcedGcSplit(page, cdp) {
  const before = await page.evaluate(() => performance.memory?.usedJSHeapSize ?? 0);
  const gcStart = Date.now();
  await cdp.send('HeapProfiler.collectGarbage');
  const gcMs = Date.now() - gcStart;
  const after = await page.evaluate(() => performance.memory?.usedJSHeapSize ?? 0);
  return { gcMs, beforeMb: +(before / 1048576).toFixed(2), afterMb: +(after / 1048576).toFixed(2) };
}

export function appendJsonl(outfile) {
  return (obj) => fs.appendFileSync(outfile, JSON.stringify(obj) + '\n');
}
