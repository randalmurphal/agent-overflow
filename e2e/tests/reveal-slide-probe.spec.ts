// Incident-exact repro of bug-report-20260806T011635Z: a ~361-char
// thinking summary arrives in four bursts (1 / 162 / 120 / 78 chars with
// ~430ms and ~210ms gaps — faster than the reveal's 320cps ceiling), with
// short prose and a successor tool call right behind it. The field trace
// showed the reveal contract holding exactly (drain + release times match
// theory to ~20ms); what remained was the collapsed think ticker's 3-line
// clamp. While the row GROWS to 3 lines each new line is spring-glided,
// but at max height the flex-end packing re-packs every visible line up
// one line-height — which TailClampedText now ANIMATES (a 140ms
// translateY FLIP on the inner wrapper). So this instrument measures the
// clamp geometry THROUGH the line-slide animation.
//
// What it samples, per animation frame, on the live think body:
//   tick  — rendered text length (reveal rate; validates ≤ ceiling, no dump)
//   bh    — clamp box height; growth = the pre-clamp regime the scroll
//           spring glides
//   clip  — pixels of text clipped above the box, from Range rects, so it
//           carries the wrapper's in-flight transform
//   ty    — the wrapper's current translateY (the FLIP's live value)
//   clipL — layout-truth clip (clip + ty), transform removed
//   rows  — mounted row count (release timing)
//   gap   — rAF gap (frame health)
//
// Reading it: on the fixed build a line advance shows as `clipL` stepping
// one line-height in ONE frame (the layout boundary) while `clip` ramps
// to it over ~140ms as `ty` decays to zero. On a pre-fix build `ty` is
// always zero and both step together in a single frame — the teleport the
// report described. slideEvents / repackEvents carry each step's pixel
// delta and the char advance in the same frame, so "a whole line moved
// while only a word of text was revealed" stays directly observable.
//
// OPT-IN ONLY, same family as boundary-probe / reveal-drain-probe:
//   make harness-build UI_TRACE=1
//   cd e2e && pnpm exec playwright install webkit
//   BOUNDARY_PROBE=1 pnpm exec playwright test reveal-slide-probe
import { mkdir, writeFile } from 'node:fs/promises';
import * as path from 'node:path';
import { chromium, webkit, type BrowserType } from '@playwright/test';
import { test, expect, type HarnessMockEvent } from './fixtures.js';
import {
  OUT_DIR,
  collectRevealProbe,
  line,
  openProbeThread,
  seedProbeThread,
  streamEvent,
  summarizeGaps,
  summarizeReleases,
  summarizeWriteTrace,
  toolPair,
  type ProbeFrame,
  type ProbeHarness,
  type TraceRecord,
} from './probe-wire.js';

// The real thinking text from the incident (361 chars), split at the real
// wire offsets: chunks of 1, 162, 120, and 78 chars.
const THINK_TEXT =
  "I'm setting up a post-task review with six parallel read-only agents, each " +
  'equipped with the task description and full change set. ' +
  "I'll pull the file modifications using git diff to get the change summary, " +
  "then spawn all six agents at once using Opus since that's the preferred " +
  'model for reviews. I need to make sure each agent knows not to spawn any ' +
  'subagents.';
const THINK_SPLITS = [1, 163, 283];
const PROSE_TEXT = 'Running post-task review before wrapping up.';

function thinkChunks(): string[] {
  const cuts = [0, ...THINK_SPLITS, THINK_TEXT.length];
  const chunks: string[] = [];
  for (let i = 1; i < cuts.length; i++) chunks.push(THINK_TEXT.slice(cuts[i - 1], cuts[i]));
  return chunks;
}

// The control variant: the SAME text at normal generation pace (~80cps,
// word-sized deltas — slower than the 160cps base reveal, so the ticker
// tracks the wire with no backlog). If the slide theory holds, the same
// instant one-line re-packs occur here too, just seconds apart instead of
// clustered at the end of a ceiling-rate drain.
function pacedThinkChunks(): string[] {
  const words = THINK_TEXT.split(/(?<= )/);
  const chunks: string[] = [];
  let cur = '';
  for (const w of words) {
    cur += w;
    if (cur.length >= 8) {
      chunks.push(cur);
      cur = '';
    }
  }
  if (cur.length > 0) chunks.push(cur);
  return chunks;
}

type ProbeMode = 'incident' | 'paced';

function buildScenario(mode: ProbeMode): Record<string, unknown> {
  const T = '${TURN}';
  const runOutput = Array.from({ length: 4 }, (_, i) => `line ${i} of mock output padding padding`).join('\n');
  const thinkDelta = (c: string) =>
    streamEvent('content_block_delta', { index: 0, delta: { type: 'thinking_delta', thinking: c } });

  // ONE chunk array per mode. The open steps stream everything but the
  // last chunk and the tail step below streams exactly that last one —
  // both derived from this array, so they cannot silently disagree about
  // where the split is.
  const chunks = mode === 'incident' ? thinkChunks() : pacedThinkChunks();
  const openDeltas = chunks.slice(0, -1).map(thinkDelta);
  const lastChunk = chunks.at(-1)!;
  const blockOpen = [
    streamEvent('message_start', { message: { id: `msg-${T}`, role: 'assistant' } }),
    streamEvent('content_block_start', { index: 0, content_block: { type: 'thinking', thinking: '' } }),
  ];

  const openSteps: unknown[] =
    mode === 'incident'
      ? [
          // Chunks 1+2 land 2ms apart (the field shape), chunk 3 after the
          // field's ~430ms gap; chunk 4 is `lastChunk`, in the tail step.
          { emit: { delayBetweenMs: 2, lines: [...blockOpen, ...openDeltas.slice(0, 2)] } },
          { delayMs: 430 },
          { emit: { lines: openDeltas.slice(2) } },
          { delayMs: 210 },
        ]
      : [
          { emit: { lines: blockOpen } },
          // Word-sized deltas at ~80cps — the ticker never falls behind.
          { emit: { delayBetweenMs: 110, lines: openDeltas } },
        ];

  return {
    version: 1,
    name: `reveal-slide-probe-${mode}`,
    description:
      mode === 'incident'
        ? 'incident-exact burst think (1/162/120/78 chars) + prose + successor tool'
        : 'same think text at ~80cps in word-sized deltas + prose + successor tool',
    provider: 'claude',
    turns: [
      {
        label: mode,
        steps: [
          // Prior activity so the think lands inside a live run clip, as in
          // the field capture.
          { emit: { delayBetweenMs: 60, lines: [1, 2].flatMap((n) => toolPair(T, n, runOutput)) } },
          { delayMs: 400 },
          ...openSteps,
          // The last chunk, settle, prose, and the message tail land within ~100ms,
          // as they did on the field wire. The per-block assistant records
          // mirror the CLI's per-block snapshots (the field wire emitted
          // assistant[thinking] right at content_block_stop). The successor
          // tool call is emitted as a record pair below rather than
          // streamed input_json_deltas — the mock's supported shape; the
          // reveal gate sees the same thing either way (a queued
          // non-smoothed successor row).
          {
            emit: {
              delayBetweenMs: 8,
              lines: [
                thinkDelta(lastChunk),
                streamEvent('content_block_stop', { index: 0 }),
                line({
                  type: 'assistant',
                  message: {
                    id: `msg-${T}`,
                    role: 'assistant',
                    content: [{ type: 'thinking', thinking: THINK_TEXT }],
                  },
                }),
                streamEvent('content_block_start', { index: 1, content_block: { type: 'text', text: '' } }),
                streamEvent('content_block_delta', { index: 1, delta: { type: 'text_delta', text: PROSE_TEXT } }),
                streamEvent('content_block_stop', { index: 1 }),
                line({
                  type: 'assistant',
                  message: {
                    id: `msg-${T}`,
                    role: 'assistant',
                    content: [
                      { type: 'thinking', thinking: THINK_TEXT },
                      { type: 'text', text: PROSE_TEXT },
                    ],
                  },
                }),
                streamEvent('message_stop', {}),
              ],
            },
          },
          { emit: { delayBetweenMs: 30, lines: toolPair(T, 3, runOutput) } },
          // The field turn stayed live through the drain (next message ~2s
          // later); keep the result envelope past the expected catch-up.
          { delayMs: 2500 },
          { emit: { lines: [line({ type: 'result', subtype: 'success', is_error: false })] } },
        ],
      },
    ],
    afterTurns: 'repeatLast',
  };
}

interface FrameSample extends ProbeFrame {
  /** Clamp box height (clientHeight). */
  bh: number;
  /** Clamp box width — the wrap width every line boundary depends on. */
  cw: number;
  /** Pixels of text clipped above the clamp box, from text-node rects
   *  (chromium's scrollHeight does not report content overflowing a
   *  flex-end box's TOP edge). RENDERED truth: the rects carry the inner
   *  wrapper's in-flight FLIP transform, so during a slide this lags the
   *  layout by `ty`. -1 when the body has no rects yet. */
  clip: number;
  /** The inner wrapper's current translateY, i.e. how much of the last
   *  re-pack the FLIP is still holding back. 0 on an un-animated build. */
  ty: number;
  /** LAYOUT truth: `clip + ty`, the clip with the transform removed, so a
   *  re-pack shows as a single-frame step regardless of the animation.
   *  -1 alongside `clip`. */
  clipL: number;
}

async function runProbe(
  browserType: BrowserType,
  engine: string,
  mode: ProbeMode,
  harness: ProbeHarness,
): Promise<{ records: TraceRecord[]; samples: FrameSample[] }> {
  await harness.rpc('HarnessSetScenario', { scenario: buildScenario(mode) });
  const threadId = await seedProbeThread(harness, {
    project: `reveal-slide-${engine}-${mode}`,
    thread: `Reveal slide ${engine} ${mode}`,
  });

  const browser = await browserType.launch();
  try {
    // Sized so the think body wraps like the field pane did: ~80 chars per
    // line, 361 chars ≈ 4.5 rendered lines through the 3-line clamp. (At
    // 800px the body column came out 264px wide — ~9 lines — which slides
    // twice as often as the incident did.)
    const page = await browser.newPage({ viewport: { width: 1100, height: 1100 } });
    await openProbeThread(page, harness.url, `Reveal slide ${engine} ${mode}`);

    // Per-frame sampler on the live think body. The rect and clientHeight
    // reads force layout, but the page is animating every frame anyway and
    // this is an opt-in instrument.
    await page.evaluate(() => {
      const probe = { samples: [] as unknown[], stop: false };
      (window as unknown as { __revealProbe: typeof probe }).__revealProbe = probe;
      // translateY of a COMPUTED transform, which is always serialized as
      // matrix()/matrix3d() (mirrors tailSlide.ts's transformTranslateY;
      // in-page code cannot import from the frontend source).
      const translateY = (transform: string): number => {
        if (!transform || !transform.startsWith('matrix')) return 0;
        const open = transform.indexOf('(');
        const close = transform.indexOf(')');
        if (open < 0 || close < 0) return 0;
        const parts = transform.slice(open + 1, close).split(',');
        const ty = Number.parseFloat(parts[transform.startsWith('matrix3d') ? 13 : 5] ?? '0');
        return Number.isFinite(ty) ? ty : 0;
      };
      let prev = performance.now();
      const loop = (t: number) => {
        if (probe.stop) return;
        const bodies = document.querySelectorAll('[data-testid="thinking-body"]');
        const body = bodies.length > 0 ? (bodies[bodies.length - 1] as HTMLElement) : null;
        // The FLIP target: the clamp box's only child, whose transform is
        // what the rect measurement below is seeing mid-slide.
        const inner = body?.firstElementChild;
        const ty = inner ? translateY(getComputedStyle(inner).transform) : 0;
        // How much text is clipped above the clamp box. scrollHeight is
        // useless for this in chromium (a flex-end box's top overflow is
        // not reported), so measure the first rendered line's rect against
        // the box's rect. Before the first paint there are no rects at
        // all — that is UNKNOWN (-1), not a zero clip, or the first frame
        // reads as a 0 → N slide.
        let clip = -1;
        let clipL = -1;
        if (body) {
          const range = document.createRange();
          range.selectNodeContents(body);
          const rects = range.getClientRects();
          if (rects.length > 0) {
            const rendered = body.getBoundingClientRect().top - rects[0].top;
            clip = Math.max(0, Math.round(rendered));
            clipL = Math.max(0, Math.round(rendered + ty));
          }
        }
        probe.samples.push({
          t: Math.round(t * 10) / 10,
          gap: Math.round((t - prev) * 10) / 10,
          tick: body?.textContent?.length ?? -1,
          rows: document.querySelectorAll('[data-row-index]').length,
          bh: body ? body.clientHeight : -1,
          cw: body ? body.clientWidth : -1,
          clip,
          ty: Math.round(ty * 10) / 10,
          clipL,
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
    await harness.rpc('SendMessage', threadId, 'run the reveal slide probe', null);
    await harness.waitForEvent('provider:turn_completed');
    // The prose releases only once the think frontier catches up (~1.6s
    // after the burst); the result envelope lands after that.
    await expect(page.getByText('Running post-task review')).toBeVisible({ timeout: 30_000 });
    // Let the prose drain and the release glide settle before collecting.
    await page.waitForTimeout(2500);

    return await collectRevealProbe<FrameSample>(page);
  } finally {
    await browser.close();
  }
}

function summarize(records: TraceRecord[], samples: FrameSample[]): Record<string, unknown> {
  // Reveal rate over 200ms windows + the largest single-frame char advance
  // (validates word-by-word: no frame may dump a multi-line run of text).
  const rates: { t: number; cps: number }[] = [];
  let windowStart = 0;
  let windowStartSeen = false;
  let windowGrowth = 0;
  let maxFrameAdvance = 0;
  let prevTick = -1;
  let firstTickAt = -1;
  let lastTickGrowthAt = -1;
  for (const s of samples) {
    if (s.tick >= 0 && prevTick >= 0 && s.tick > prevTick) {
      windowGrowth += s.tick - prevTick;
      maxFrameAdvance = Math.max(maxFrameAdvance, s.tick - prevTick);
      lastTickGrowthAt = s.t;
    }
    if (!windowStartSeen && s.tick >= 0) {
      windowStart = s.t;
      windowStartSeen = true;
      firstTickAt = s.t;
    }
    if (windowStartSeen && s.t - windowStart >= 200) {
      const cps = Math.round((windowGrowth * 1000) / (s.t - windowStart));
      if (cps > 0) rates.push({ t: Math.round(windowStart), cps });
      windowStart = s.t;
      windowGrowth = 0;
    }
    if (s.tick >= 0) prevTick = s.tick;
  }

  // Three regimes, one per column of the sample.
  //   growth  — the clamp box itself getting taller (pre-clamp; the
  //             enclosing scroll spring glides this).
  //   repacks — `clipL`, layout truth: the flex-end anchor packing the
  //             content one line up. Always a single-frame step.
  //   slides  — `clip`, rendered truth: the same advance as the FLIP
  //             actually paints it. On the animated build each repack is
  //             followed by a ~140ms ramp of these; on a pre-fix build a
  //             repack and its slide are the SAME frame and `ty` is 0.
  const growth: { t: number; from: number; to: number; gap: number }[] = [];
  const repacks: { t: number; dPx: number; charsSameFrame: number; gap: number }[] = [];
  const slides: { t: number; dPx: number; ty: number; charsSameFrame: number; gap: number }[] = [];
  for (let i = 1; i < samples.length; i++) {
    const a = samples[i - 1];
    const b = samples[i];
    if (a.bh < 0 || b.bh < 0) continue;
    const charsSameFrame = b.tick >= 0 && a.tick >= 0 ? b.tick - a.tick : -1;
    if (b.bh > a.bh) growth.push({ t: Math.round(b.t), from: a.bh, to: b.bh, gap: b.gap });
    if (a.clipL >= 0 && b.clipL > a.clipL) {
      repacks.push({ t: Math.round(b.t), dPx: b.clipL - a.clipL, charsSameFrame, gap: b.gap });
    }
    if (a.clip >= 0 && b.clip > a.clip) {
      slides.push({ t: Math.round(b.t), dPx: b.clip - a.clip, ty: b.ty, charsSameFrame, gap: b.gap });
    }
  }

  const gaps = summarizeGaps(samples);
  const finalTick = samples.reduce((m, s) => Math.max(m, s.tick), -1);
  const bodyWidth = samples.reduce((m, s) => Math.max(m, s.cw), -1);

  return {
    sampleCount: samples.length,
    thinkChars: finalTick,
    bodyWidthPx: bodyWidth,
    tickerRates: rates,
    maxFrameAdvanceChars: maxFrameAdvance,
    drainMs: firstTickAt >= 0 && lastTickGrowthAt >= 0 ? Math.round(lastTickGrowthAt - firstTickAt) : -1,
    growthEvents: growth,
    repackEvents: repacks,
    slideEvents: slides,
    releases: summarizeReleases(samples),
    rafGapP99: gaps.p99,
    rafGapMax: gaps.max,
    ...summarizeWriteTrace(records),
  };
}

test.describe('reveal slide probe', () => {
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
    for (const mode of ['incident', 'paced'] as const) {
      test(`replays the ${mode} wire shape under ${engine} and dumps clamp geometry`, async ({
        harness,
      }) => {
        const { records, samples } = await runProbe(browserType, engine, mode, harness);
        const summary = summarize(records, samples);
        await mkdir(OUT_DIR, { recursive: true });
        await writeFile(
          path.join(OUT_DIR, `reveal-slide-${engine}-${mode}.json`),
          JSON.stringify({ summary, samples, records }, null, 1),
        );
        console.log(`[reveal-slide:${engine}:${mode}]`, JSON.stringify(summary));
        expect(samples.length).toBeGreaterThan(0);
        expect(records.length).toBeGreaterThan(0);
      });
    }
  }
});
