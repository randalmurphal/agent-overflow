// Engine-comparison probe for the think→separator→prose boundary jump.
// Streams the same scripted turn (tool-call run → fast thinking → gap →
// prose → successor tool call) through the real app under Chromium AND
// WebKit, pulls the in-page UI render trace (UI_TRACE=1 build), and
// writes each engine's records to disk for offline diffing. The
// assertion surface is deliberately thin — this is an instrument, the
// analysis happens on the dumped JSON.
//
// OPT-IN ONLY. It needs a trace-enabled harness and a webkit browser,
// neither of which the standard `make e2e` gate provides, so the whole
// describe skips unless BOUNDARY_PROBE is set.
//
// Run:
//   make harness-build UI_TRACE=1
//   cd e2e && pnpm exec playwright install webkit
//   BOUNDARY_PROBE=1 pnpm exec playwright test boundary-probe
import { mkdir, writeFile } from 'node:fs/promises';
import * as path from 'node:path';
import { chromium, webkit, type BrowserType } from '@playwright/test';
import { test, expect, type HarnessMockEvent } from './fixtures.js';
import {
  OUT_DIR,
  line,
  openProbeThread,
  seedProbeThread,
  streamEvent,
  toolPair,
  type ProbeHarness,
  type TraceRecord,
} from './probe-wire.js';

function buildScenario(): Record<string, unknown> {
  const T = '${TURN}';
  const runOutput = Array.from({ length: 5 }, (_, i) => `line ${i} of mock output padding padding`).join('\n');

  const thinkChunks = [
    'The bash step finished cleanly. ',
    'Looking at the output there are a few things to check. ',
    'The counts line up with what the harness seeded earlier on. ',
    'Nothing unexpected in the listing so the plan holds. ',
    'Next the prose summary should describe the outcome. ',
    'Keep it short and continue to the follow-up command. ',
  ];
  const proseChunks = [
    'The command completed and every check passed. ',
    'The listing matches the seeded fixture exactly, ',
    'with all five files present and no strays. ',
    'That means the migration step is safe to run, ',
    'and the follow-up verification can start now. ',
    'Kicking off the verification pass next, ',
    'which re-reads each file and compares hashes. ',
    'This is the part that previously felt stepped, ',
    'so the pacing here mirrors the real incident: ',
    'prose streaming while a successor tool call queues. ',
    'A few more sentences give the reveal gate a real ',
    'backlog to drain across the boundary release. ',
  ];

  const thinkFull = thinkChunks.join('');
  const proseFull = proseChunks.join('');

  return {
    version: 1,
    name: 'boundary-probe',
    description: 'bash run -> fast thinking -> gap -> prose -> successor tool call, paced like the incident',
    provider: 'claude',
    turns: [
      {
        label: 'boundary',
        steps: [
          // The activity run: six quick tool pairs so a run clip forms.
          {
            emit: {
              delayBetweenMs: 60,
              lines: [1, 2, 3, 4, 5, 6].flatMap((n) => toolPair(T, n, runOutput)),
            },
          },
          // Fast thinking block (the "think text came quickly" phase).
          {
            emit: {
              delayBetweenMs: 25,
              lines: [
                streamEvent('message_start', { message: { id: `msg-${T}`, role: 'assistant' } }),
                streamEvent('content_block_start', { index: 0, content_block: { type: 'thinking', thinking: '' } }),
                ...thinkChunks.map((c) =>
                  streamEvent('content_block_delta', { delta: { type: 'thinking_delta', thinking: c } }),
                ),
                streamEvent('content_block_stop', { index: 0 }),
              ],
            },
          },
          // The model gap: think settles, reveal drains, outer pane goes still.
          { delayMs: 700 },
          // Prose streams…
          {
            emit: {
              delayBetweenMs: 35,
              lines: [
                streamEvent('content_block_start', { index: 1, content_block: { type: 'text', text: '' } }),
                ...proseChunks.map((c) =>
                  streamEvent('content_block_delta', { delta: { type: 'text_delta', text: c } }),
                ),
                streamEvent('content_block_stop', { index: 1 }),
                streamEvent('message_stop', {}),
                line({
                  type: 'assistant',
                  message: {
                    id: `msg-${T}`,
                    role: 'assistant',
                    content: [
                      { type: 'thinking', thinking: thinkFull },
                      { type: 'text', text: proseFull },
                    ],
                  },
                }),
              ],
            },
          },
          // …with a successor tool call right behind it — the row the reveal
          // gate withholds until the frontier drains.
          {
            emit: {
              delayBetweenMs: 60,
              lines: toolPair(T, 7, runOutput),
            },
          },
          // A mid-turn gap long enough that the 500ms live-content hold
          // expires INSIDE the turn, so the next burst restarts the
          // spring from rest and the restart lands in the dumped trace.
          // This pacing is what used to trip the 2026-08-05 lease
          // promotion; the lease is gone, the restart shape is still
          // worth dumping. 2000ms = the 500ms hold plus settle margin
          // (was 7000ms, sized against the removed lease deadlines).
          { delayMs: 2000 },
          {
            emit: {
              delayBetweenMs: 30,
              lines: [
                streamEvent('message_start', { message: { id: `msg-done-${T}`, role: 'assistant' } }),
                streamEvent('content_block_start', { index: 0, content_block: { type: 'text', text: '' } }),
                streamEvent('content_block_delta', { delta: { type: 'text_delta', text: 'Verification passed. BOUNDARY_PROBE_DONE' } }),
                streamEvent('content_block_stop', { index: 0 }),
                streamEvent('message_stop', {}),
                line({
                  type: 'assistant',
                  message: {
                    id: `msg-done-${T}`,
                    role: 'assistant',
                    content: [{ type: 'text', text: 'Verification passed. BOUNDARY_PROBE_DONE' }],
                  },
                }),
                line({ type: 'result', subtype: 'success', is_error: false }),
              ],
            },
          },
        ],
      },
    ],
    afterTurns: 'repeatLast',
  };
}

async function runProbe(
  browserType: BrowserType,
  engine: string,
  harness: ProbeHarness,
): Promise<TraceRecord[]> {
  await harness.rpc('HarnessSetScenario', { scenario: buildScenario() });
  const threadId = await seedProbeThread(harness, {
    project: `probe-${engine}`,
    thread: `Boundary probe ${engine}`,
  });

  const browser = await browserType.launch();
  try {
    const page = await browser.newPage({ viewport: { width: 960, height: 1200 } });
    await openProbeThread(page, harness, `Boundary probe ${engine}`);

    await harness.rpc('StartSession', threadId);
    await harness.waitForEvent<HarnessMockEvent>(
      'harness:mock',
      (ev) => ev.report.kind === 'registered',
    );
    await harness.rpc('SendMessage', threadId, 'run the boundary probe', null);
    await harness.waitForEvent('provider:turn_completed');
    await expect(page.getByText('BOUNDARY_PROBE_DONE')).toBeVisible({ timeout: 20_000 });
    // Let the drain tail + settle passes finish before sampling.
    await page.waitForTimeout(2500);

    const records = await page.evaluate(() => {
      const api = (window as unknown as {
        __agentOverflowUiTrace?: { records(): unknown[] };
      }).__agentOverflowUiTrace;
      return api ? (api.records() as unknown[]) : null;
    });
    if (!records) throw new Error('UI trace API missing — was the harness built with UI_TRACE=1?');
    return records as TraceRecord[];
  } finally {
    await browser.close();
  }
}

function summarize(records: TraceRecord[]): Record<string, unknown> {
  const writesByCaller = new Map<string, { count: number; maxJump: number; jumps: number[] }>();
  let willSpringFalse = 0;
  let willSpringTrue = 0;
  const chases: unknown[] = [];
  for (const r of records) {
    const d = (r.data ?? {}) as Record<string, unknown>;
    if (r.label === 'scroll.write') {
      const caller = String(d.caller ?? 'unknown');
      const before = Number(d.beforeTop ?? Number.NaN);
      const after = Number(d.afterTop ?? Number.NaN);
      const jump = Number.isFinite(before) && Number.isFinite(after) ? Math.abs(after - before) : 0;
      const entry = writesByCaller.get(caller) ?? { count: 0, maxJump: 0, jumps: [] };
      entry.count += 1;
      entry.maxJump = Math.max(entry.maxJump, jump);
      if (jump >= 40) entry.jumps.push(Math.round(jump));
      writesByCaller.set(caller, entry);
    } else if (r.label === 'scroll.notifyLiveContentMaybeGrew') {
      if (d.willSpring === false && d.canPin === true) willSpringFalse += 1;
      else if (d.willSpring === true) willSpringTrue += 1;
    } else if (r.label === 'scroll.spring.chase') {
      chases.push(d);
    }
  }
  return {
    totalRecords: records.length,
    writesByCaller: Object.fromEntries(writesByCaller),
    willSpringTrue,
    willSpringFalse,
    chases,
  };
}

test.describe('boundary probe', () => {
  test.describe.configure({ timeout: 180_000 });
  test.skip(
    !process.env.BOUNDARY_PROBE,
    'opt-in instrument: needs `make harness-build UI_TRACE=1` and '
      + '`pnpm exec playwright install webkit`; run with BOUNDARY_PROBE=1',
  );

  for (const [engine, browserType] of [
    ['chromium', chromium],
    ['webkit', webkit],
  ] as const) {
    test(`streams the boundary turn under ${engine} and dumps the scroll trace`, async ({
      harness,
    }) => {
      const records = await runProbe(browserType, engine, harness);
      const summary = summarize(records);
      await mkdir(OUT_DIR, { recursive: true });
      await writeFile(
        path.join(OUT_DIR, `boundary-trace-${engine}.json`),
        JSON.stringify({ summary, records }, null, 1),
      );
      console.log(`[boundary-probe:${engine}]`, JSON.stringify(summary));
      expect(records.length).toBeGreaterThan(0);
      // The scripted turn always streams enough to spring — a trace with
      // no spring.tick writes means the instrument lost its subject, not
      // that there was nothing to measure. This is what kills the
      // vacuous pass.
      const springTicks = (summary.writesByCaller as Record<
        string,
        { maxJump: number } | undefined
      >)['spring.tick'];
      expect(springTicks, 'no spring.tick writes captured').toBeDefined();
      // No presented frame advances more than ONE velocity-capped step.
      // 30 = SPRING_MAX_VELOCITY_PX_PER_FRAME (27, in frontend
      // utils/scroll/spring.ts — e2e cannot import frontend src) plus
      // slack for the integration epsilon. This is the direct statement
      // of SPRING_MAX_CATCHUP_STEPS = 1: before that change a stalled
      // frame integrated up to three steps (~81px) in a single write,
      // which is routine on WebKit and is the "fast jump" mechanism.
      expect(springTicks!.maxJump).toBeLessThanOrEqual(30);
    });
  }
});
