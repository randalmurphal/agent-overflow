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
import { test, expect, type HarnessMockEvent, type SeedResult } from './fixtures.js';

const OUT_DIR = process.env.BOUNDARY_PROBE_OUT ?? path.resolve(import.meta.dirname, '..', 'test-results');

function line(obj: unknown): string {
  return JSON.stringify(obj);
}

function toolPair(turnVar: string, n: number, output: string): string[] {
  return [
    line({
      type: 'assistant',
      message: {
        id: `msg-tool-${turnVar}-${n}`,
        role: 'assistant',
        content: [
          { type: 'tool_use', id: `tu-${turnVar}-${n}`, name: 'Bash', input: { command: `echo step-${n}` } },
        ],
      },
    }),
    line({
      type: 'user',
      message: {
        role: 'user',
        content: [{ type: 'tool_result', tool_use_id: `tu-${turnVar}-${n}`, content: output }],
      },
    }),
  ];
}

function streamEvent(event: string, data: Record<string, unknown>): string {
  return line({ type: 'stream_event', event, data: { type: event, ...data } });
}

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
          // A long mid-turn gap: the content lease's real 5s release
          // window elapses INSIDE the turn, which is what made the next
          // burst's first spring tick create the layer (the 2026-08-05
          // promote-at-glide-start). 7s rather than 6 for margin over the
          // 5s deadline + the 500ms live-content hold + a 250ms recheck.
          { delayMs: 7000 },
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

interface TraceRecord {
  seq: number;
  at: number;
  label: string;
  data: Record<string, unknown> | null;
}

async function runProbe(
  browserType: BrowserType,
  engine: string,
  harness: {
    rpc<T = unknown>(method: string, ...args: unknown[]): Promise<T>;
    waitForEvent<T = unknown>(channel: string, match?: (data: T) => boolean): Promise<T>;
    url: string;
  },
): Promise<TraceRecord[]> {
  await harness.rpc('HarnessSetScenario', { scenario: buildScenario() });
  // Enough seeded history that the pane genuinely scrolls.
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: `probe-${engine}`,
        repo: {},
        threads: [
          {
            title: `Boundary probe ${engine}`,
            turns: Array.from({ length: 8 }, (_, i) => ({
              userText: `history question ${i}`,
              items: [
                {
                  kind: 'assistant_text',
                  summary:
                    `History answer ${i}. ` +
                    'This paragraph pads the transcript so the pane scrolls well past one viewport. '.repeat(6),
                },
              ],
            })),
          },
        ],
      },
    ],
  });
  const threadId = seed.projects[0].threadIds[0];

  const browser = await browserType.launch();
  try {
    const page = await browser.newPage({ viewport: { width: 960, height: 1200 } });
    await page.goto(harness.url);
    await page.getByText(`Boundary probe ${engine}`).click();
    await expect(page.getByText('history question 7')).toBeVisible();
    // Let the thread-switch restore settle (warm gate) before streaming.
    await page.waitForTimeout(1500);

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
  const leases: Record<string, unknown>[] = [];
  let leasePromotes = 0;
  let leaseDemotes = 0;
  // The lease invariant's tripwire: a transition may only happen while
  // the surface is at rest, so a promote recorded mid-motion is a
  // surface that produced programmatic motion without holding the lease.
  let leasePromotesMidMotion = 0;
  for (const r of records) {
    const d = (r.data ?? {}) as Record<string, unknown>;
    if (r.label === 'scroll.lease') {
      leases.push(d);
      if (d.action === 'promote') {
        leasePromotes += 1;
        if (d.midMotion === true) leasePromotesMidMotion += 1;
      } else if (d.action === 'demote') {
        leaseDemotes += 1;
      }
    } else if (r.label === 'scroll.write') {
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
    leasePromotes,
    leaseDemotes,
    leasePromotesMidMotion,
    leases,
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
      // The assertions this instrument carries, all one claim: the turn
      // HOLDS its content-layer lease across the 7s gap.
      //   - promotes > 0 kills the vacuous pass where the trace carries
      //     no lease records at all and every count is trivially 0;
      //   - demotes === 0 is the direct statement of the hold;
      //   - midMotion === 0 is the invariant's tripwire.
      // On a build that emits the trace field but has the hold removed,
      // midMotion runs non-zero on both engines (the promote lands on
      // the first spring tick of the burst after the gap).
      expect(summary.leasePromotes).toBeGreaterThan(0);
      expect(summary.leaseDemotes).toBe(0);
      expect(summary.leasePromotesMidMotion).toBe(0);

      // No presented frame advances more than ONE velocity-capped step.
      // 30 = SPRING_MAX_VELOCITY_PX_PER_FRAME (27, in frontend
      // utils/scroll/spring.ts — e2e cannot import frontend src) plus
      // slack for the integration epsilon. This is the direct statement
      // of SPRING_MAX_CATCHUP_STEPS = 1: before that change a stalled
      // frame integrated up to three steps (~81px) in a single write,
      // which is routine on WebKit and is the "fast jump" mechanism.
      const springTicks = (summary.writesByCaller as Record<
        string,
        { maxJump: number } | undefined
      >)['spring.tick'];
      if (springTicks) expect(springTicks.maxJump).toBeLessThanOrEqual(30);
    });
  }
});
