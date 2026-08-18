// Diagnostic driver for the activity-run expand flicker (2026-08-17).
// Boots a harness, seeds a thread whose tail is [thinking-run, long text],
// toggles the run while capturing a Chromium trace with paint-invalidation
// tracking, and writes the trace + page geometry for offline analysis.
//
// Run from e2e/:  node scripts/flicker-repro.mjs <out-dir>
import { launchHarness } from '../src/harness.ts';
import { chromium } from '@playwright/test';
import { writeFileSync, mkdirSync } from 'node:fs';
import * as path from 'node:path';

const outDir = process.argv[2];
if (!outDir) {
  console.error('usage: node scripts/flicker-repro.mjs <out-dir>');
  process.exit(1);
}
mkdirSync(outDir, { recursive: true });

const para = (n, tag) =>
  Array.from(
    { length: n },
    (_, i) =>
      `Paragraph ${tag}-${i + 1}: The quick brown fox jumps over the lazy dog while the ` +
      `compositor re-rasters tiles that moved in layer space. This line exists to give the ` +
      `timeline real text mass so the pane scrolls and the bottom-held toggle has content ` +
      `below the run to compensate.`,
  ).join('\n\n');

const harness = await launchHarness({});
console.log('[repro] harness up at', harness.bootstrap.url);

let browser;
try {
  const seed = await harness.rpc('HarnessSeed', {
    projects: [
      {
        name: 'flicker-app',
        repo: { commits: [{ message: 'init', files: { 'README.md': '# flicker\n' } }] },
        threads: [
          {
            title: 'Flicker thread',
            turns: [
              {
                userText: 'Give me a lot of context',
                items: [{ kind: 'assistant_text', summary: para(14, 'ctx') }],
              },
              {
                userText: 'Now think about it',
                items: [
                  {
                    kind: 'thinking',
                    summary: 'Pondered the approach',
                    payload: { kind: 'thinking', data: 'Considering the layout carefully.' },
                  },
                  { kind: 'assistant_text', summary: para(6, 'tail') },
                ],
              },
            ],
          },
        ],
      },
    ],
  });
  console.log('[repro] seeded thread', seed.projects[0].threadIds[0]);

  browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
  await page.goto(harness.url);
  await page.getByText('Flicker thread').click();

  const run = page.getByTestId('activity-run');
  const header = page.getByTestId('activity-run-header');
  await header.waitFor({ state: 'visible', timeout: 15_000 });
  // Let the timeline settle (restore, measurements, reveal) before tracing.
  await page.waitForTimeout(1_500);

  // Start collapsed so every cycle is expand -> collapse.
  if ((await run.getAttribute('data-collapsed')) === 'false') {
    await header.click();
    await page.waitForTimeout(600);
  }

  const geometry = await page.evaluate(() => {
    const S = [...document.querySelectorAll('[data-testid="message-timeline-scroll"]')].find(
      (el) => el.offsetParent && el.querySelector('[data-testid="activity-run"]'),
    );
    const rect = (el) => {
      const r = el.getBoundingClientRect();
      return { x: r.x, y: r.y, w: r.width, h: r.height };
    };
    const rows = [...S.querySelectorAll('[data-row-index]')].map((w) => ({
      i: +w.dataset.rowIndex,
      rect: rect(w.parentElement),
      styleTop: w.parentElement.style.top,
      isRun: !!w.querySelector('[data-testid="activity-run"]'),
      text: (w.textContent ?? '').slice(0, 60),
    }));
    return {
      dpr: window.devicePixelRatio,
      scroller: rect(S),
      scrollTop: S.scrollTop,
      scrollHeight: S.scrollHeight,
      rows,
    };
  });
  writeFileSync(path.join(outDir, 'geometry.json'), JSON.stringify(geometry, null, 2));
  console.log(
    '[repro] geometry: scrollTop',
    geometry.scrollTop,
    'rows',
    geometry.rows.map((r) => `${r.i}${r.isRun ? '*' : ''}@${Math.round(r.rect.y)}`).join(' '),
  );

  await browser.startTracing(page, {
    path: path.join(outDir, 'trace.json'),
    screenshots: false,
    categories: [
      'blink.user_timing',
      'devtools.timeline',
      'disabled-by-default-devtools.timeline',
      'disabled-by-default-devtools.timeline.frame',
      'disabled-by-default-devtools.timeline.invalidationTracking',
      'disabled-by-default-blink.invalidation',
    ],
  });

  for (let cycle = 1; cycle <= 3; cycle++) {
    await page.evaluate((c) => performance.mark(`expand-${c}`), cycle);
    await header.click();
    await page.waitForTimeout(700);
    await page.evaluate((c) => performance.mark(`collapse-${c}`), cycle);
    await header.click();
    await page.waitForTimeout(700);
  }

  await browser.stopTracing();
  console.log('[repro] trace written to', path.join(outDir, 'trace.json'));
} finally {
  await browser?.close();
  await harness.close();
}
