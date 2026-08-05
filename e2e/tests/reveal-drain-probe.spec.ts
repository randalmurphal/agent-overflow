// Instrument for the late-thinking-summary reveal pileup: the ticker
// pauses (summary still being generated provider-side), a multi-KB
// summary burst lands, and prose + a successor tool call arrive right
// behind it — so the reveal gate withholds them behind the think
// frontier. Nothing is rushed and nothing is skipped: the frontier
// animates every character at MAX_ADAPTIVE_CHARS_PER_SEC for as long as
// that takes (~12.5s for this scenario's ~4KB burst), and the withheld
// rows release only afterward. What the probe demonstrates: no rush
// plateau in the observed reveal rate, a drain whose duration tracks the
// backlog size, and the release landing at the end of it. Samples per
// animation frame: the think ticker's rendered text length (observed
// reveal rate), the mounted row count (the release), and the rAF gap
// (jank), then dumps everything next to the scroll trace for offline
// analysis.
//
// OPT-IN ONLY, same family as boundary-probe:
//   make harness-build UI_TRACE=1
//   cd e2e && pnpm exec playwright install webkit
//   BOUNDARY_PROBE=1 pnpm exec playwright test reveal-drain-probe
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

// ~500 chars per burst chunk; 8 chunks ≈ 4KB of late summary.
function burstChunk(n: number): string {
  return (
    `Late summary segment ${n}: the provider finished summarizing this part of the reasoning well after the ` +
    'main agent had already moved on to producing its answer and queueing the next command, so the whole ' +
    'segment lands at once instead of streaming word by word the way the opening of the thinking block did. ' +
    'The reveal machinery now holds a multi-kilobyte backlog on a row whose collapsed body only ever shows ' +
    'the last three rendered lines, which is the shape under test here. '
  );
}

function buildScenario(): Record<string, unknown> {
  const T = '${TURN}';
  const runOutput = Array.from({ length: 5 }, (_, i) => `line ${i} of mock output padding padding`).join('\n');

  const openingThink = [
    'Starting to look at the command output now. ',
    'The listing seems consistent with the seed. ',
    'Checking a couple of edge cases before answering. ',
  ];
  const lateBurst = [1, 2, 3, 4, 5, 6, 7, 8].map(burstChunk);
  const proseChunks = [
    'Everything checks out. ',
    'The listing matches the fixture, ',
    'and the follow-up verification can run now. ',
    'Starting it next. ',
    'REVEAL_PROBE_DONE ',
  ];

  const thinkFull = openingThink.join('') + lateBurst.join('');
  const proseFull = proseChunks.join('');

  return {
    version: 1,
    name: 'reveal-drain-probe',
    description: 'think opens -> pause -> multi-KB late summary burst -> short prose -> successor tool call',
    provider: 'claude',
    turns: [
      {
        label: 'late-summary',
        steps: [
          // A small activity run so the think row lands inside a run clip.
          {
            emit: {
              delayBetweenMs: 60,
              lines: [1, 2, 3].flatMap((n) => toolPair(T, n, runOutput)),
            },
          },
          // Think opens at a readable pace — the phase the user watches.
          {
            emit: {
              delayBetweenMs: 100,
              lines: [
                streamEvent('message_start', { message: { id: `msg-${T}`, role: 'assistant' } }),
                streamEvent('content_block_start', { index: 0, content_block: { type: 'thinking', thinking: '' } }),
                ...openingThink.map((c) =>
                  streamEvent('content_block_delta', { delta: { type: 'thinking_delta', thinking: c } }),
                ),
              ],
            },
          },
          // The pause: the summary is still being generated provider-side.
          { delayMs: 2500 },
          // The late summary lands as a burst (~4KB in ~120ms).
          {
            emit: {
              delayBetweenMs: 15,
              lines: [
                ...lateBurst.map((c) =>
                  streamEvent('content_block_delta', { delta: { type: 'thinking_delta', thinking: c } }),
                ),
                streamEvent('content_block_stop', { index: 0 }),
              ],
            },
          },
          // Short prose right behind it — the successor the gate withholds
          // behind the draining think frontier, with the tool pair after it
          // withheld in turn behind the prose.
          {
            emit: {
              delayBetweenMs: 30,
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
          {
            emit: {
              delayBetweenMs: 60,
              lines: toolPair(T, 4, runOutput),
            },
          },
          {
            emit: {
              delayBetweenMs: 30,
              lines: [line({ type: 'result', subtype: 'success', is_error: false })],
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

interface FrameSample {
  t: number;
  gap: number;
  tick: number;
  rows: number;
}

async function runProbe(
  browserType: BrowserType,
  engine: string,
  harness: {
    rpc<T = unknown>(method: string, ...args: unknown[]): Promise<T>;
    waitForEvent<T = unknown>(channel: string, match?: (data: T) => boolean): Promise<T>;
    url: string;
  },
): Promise<{ records: TraceRecord[]; samples: FrameSample[] }> {
  await harness.rpc('HarnessSetScenario', { scenario: buildScenario() });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: `reveal-probe-${engine}`,
        repo: {},
        threads: [
          {
            title: `Reveal probe ${engine}`,
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
    await page.getByText(`Reveal probe ${engine}`).click();
    await expect(page.getByText('history question 7')).toBeVisible();
    await page.waitForTimeout(1500);

    // Per-frame sampler: ticker text length (observed reveal rate), row
    // count (release burst timing), rAF gap (jank). Reading textContent
    // of the windowed ticker (≤ ~8k chars) per frame is cheap enough for
    // an instrument.
    await page.evaluate(() => {
      const probe = { samples: [] as unknown[], stop: false };
      (window as unknown as { __revealProbe: typeof probe }).__revealProbe = probe;
      let prev = performance.now();
      const loop = (t: number) => {
        if (probe.stop) return;
        const bodies = document.querySelectorAll('[data-testid="thinking-body"]');
        const body = bodies.length > 0 ? bodies[bodies.length - 1] : null;
        probe.samples.push({
          t: Math.round(t * 10) / 10,
          gap: Math.round((t - prev) * 10) / 10,
          tick: body?.textContent?.length ?? -1,
          rows: document.querySelectorAll('[data-row-index]').length,
        });
        prev = t;
        requestAnimationFrame(loop);
      };
      requestAnimationFrame(loop);
    });

    await harness.rpc('StartSession', threadId);
    await harness.waitForEvent<HarnessMockEvent>(
      'harness:mock',
      (ev) => ev.report.kind === 'registered',
    );
    await harness.rpc('SendMessage', threadId, 'run the reveal probe', null);
    await harness.waitForEvent('provider:turn_completed');
    // The turn's wire completes early; DONE only appears after the FULL
    // ordered drain — ~13s for this scenario's ~4.1KB think backlog at the
    // 320cps ceiling, plus the prose behind it. Nothing shortens that any
    // more, so the timeout carries the whole readable drain with headroom.
    await expect(page.getByText('REVEAL_PROBE_DONE')).toBeVisible({ timeout: 60_000 });
    // Let the drain tail finish before sampling — the whole point is to
    // watch the post-wire reveal, so this wait is generous.
    await page.waitForTimeout(4000);

    const collected = await page.evaluate(() => {
      const w = window as unknown as {
        __revealProbe?: { samples: unknown[]; stop: boolean };
        __agentOverflowUiTrace?: { records(): unknown[] };
      };
      if (w.__revealProbe) w.__revealProbe.stop = true;
      return {
        samples: w.__revealProbe?.samples ?? null,
        records: w.__agentOverflowUiTrace ? w.__agentOverflowUiTrace.records() : null,
      };
    });
    if (!collected.records) throw new Error('UI trace API missing — was the harness built with UI_TRACE=1?');
    if (!collected.samples) throw new Error('frame sampler missing');
    return {
      records: collected.records as TraceRecord[],
      samples: collected.samples as FrameSample[],
    };
  } finally {
    await browser.close();
  }
}

function summarize(records: TraceRecord[], samples: FrameSample[]): Record<string, unknown> {
  // Observed ticker reveal rate over 250ms windows (chars/sec), from the
  // sampled rendered length. Window swaps (TailClampedText advancing its
  // cut) make the length DROP — count those separately and rate only the
  // growth.
  const rates: { t: number; cps: number }[] = [];
  let windowStart = 0;
  let windowStartLen = -1;
  let windowGrowth = 0;
  let cuts = 0;
  let prevTick = -1;
  for (const s of samples) {
    if (s.tick >= 0 && prevTick >= 0) {
      if (s.tick >= prevTick) windowGrowth += s.tick - prevTick;
      else cuts += 1;
    }
    if (windowStartLen < 0 && s.tick >= 0) {
      windowStart = s.t;
      windowStartLen = s.tick;
    }
    if (windowStartLen >= 0 && s.t - windowStart >= 250) {
      const cps = Math.round((windowGrowth * 1000) / (s.t - windowStart));
      if (cps > 0) rates.push({ t: Math.round(windowStart), cps });
      windowStart = s.t;
      windowGrowth = 0;
    }
    if (s.tick >= 0) prevTick = s.tick;
  }
  const maxCps = rates.reduce((m, r) => Math.max(m, r.cps), 0);

  // Row-count releases: how many rows mounted between adjacent samples.
  const releases: { t: number; from: number; to: number; rafGapMs: number }[] = [];
  for (let i = 1; i < samples.length; i++) {
    if (samples[i].rows > samples[i - 1].rows) {
      releases.push({
        t: Math.round(samples[i].t),
        from: samples[i - 1].rows,
        to: samples[i].rows,
        rafGapMs: samples[i].gap,
      });
    }
  }

  // rAF gap stats over the streaming portion (first tick sample onward).
  const firstTickAt = samples.find((s) => s.tick >= 0)?.t ?? 0;
  const gaps = samples.filter((s) => s.t >= firstTickAt).map((s) => s.gap);
  gaps.sort((a, b) => a - b);
  const gapP99 = gaps.length > 0 ? gaps[Math.floor(gaps.length * 0.99)] : 0;
  const gapMax = gaps.length > 0 ? gaps[gaps.length - 1] : 0;
  const gapsOver25 = gaps.filter((g) => g > 25).length;
  const gapsOver50 = gaps.filter((g) => g > 50).length;

  const writesByCaller = new Map<string, { count: number; maxJump: number }>();
  const chases: unknown[] = [];
  for (const r of records) {
    const d = (r.data ?? {}) as Record<string, unknown>;
    if (r.label === 'scroll.write') {
      const caller = String(d.caller ?? 'unknown');
      const before = Number(d.beforeTop ?? Number.NaN);
      const after = Number(d.afterTop ?? Number.NaN);
      const jump = Number.isFinite(before) && Number.isFinite(after) ? Math.abs(after - before) : 0;
      const entry = writesByCaller.get(caller) ?? { count: 0, maxJump: 0 };
      entry.count += 1;
      entry.maxJump = Math.max(entry.maxJump, jump);
      writesByCaller.set(caller, entry);
    } else if (r.label === 'scroll.spring.chase') {
      chases.push(d);
    }
  }

  return {
    sampleCount: samples.length,
    tickerRates: rates,
    tickerMaxCps: maxCps,
    tickerWindowCuts: cuts,
    releases,
    rafGapP99: gapP99,
    rafGapMax: gapMax,
    rafGapsOver25: gapsOver25,
    rafGapsOver50: gapsOver50,
    writesByCaller: Object.fromEntries(writesByCaller),
    chases,
  };
}

test.describe('reveal drain probe', () => {
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
    test(`streams the late-summary turn under ${engine} and dumps reveal + scroll traces`, async ({
      harness,
    }) => {
      const { records, samples } = await runProbe(browserType, engine, harness);
      const summary = summarize(records, samples);
      await mkdir(OUT_DIR, { recursive: true });
      await writeFile(
        path.join(OUT_DIR, `reveal-drain-${engine}.json`),
        JSON.stringify({ summary, samples, records }, null, 1),
      );
      console.log(`[reveal-drain:${engine}]`, JSON.stringify(summary));
      expect(samples.length).toBeGreaterThan(0);
      expect(records.length).toBeGreaterThan(0);
    });
  }
});
