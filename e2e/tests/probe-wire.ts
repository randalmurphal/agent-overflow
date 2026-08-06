// Shared instrument plumbing for the probe specs (boundary-probe,
// reveal-drain-probe, reveal-slide-probe).
//
// Two layers, both deliberately scenario-agnostic:
//   - wire shapes — the mock provider's accepted Claude NDJSON forms, in
//     one place so the probes cannot drift apart on what a "tool pair" or
//     a stream event looks like on the wire;
//   - driver + folds — the parts every probe runs identically: the seeded
//     scrolling history, opening it past the warm gate, the in-page
//     collect, and the release / scroll-trace / rAF-gap summaries.
//
// What stays in each spec: its scenario shape, its per-frame sampler, and
// the summary fields only it reports.
import * as path from 'node:path';
import type { Page } from '@playwright/test';
import { expect, type SeedResult } from './fixtures.js';

export const OUT_DIR =
  process.env.BOUNDARY_PROBE_OUT ?? path.resolve(import.meta.dirname, '..', 'test-results');

// ── Wire shapes ────────────────────────────────────────────────────────

export function line(obj: unknown): string {
  return JSON.stringify(obj);
}

export function streamEvent(event: string, data: Record<string, unknown>): string {
  return line({ type: 'stream_event', event, data: { type: event, ...data } });
}

export function toolPair(turnVar: string, n: number, output: string): string[] {
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

export interface TraceRecord {
  seq: number;
  at: number;
  label: string;
  data: Record<string, unknown> | null;
}

// ── Driver ─────────────────────────────────────────────────────────────

/** Structural view of the harness fixture the probes drive. */
export interface ProbeHarness {
  rpc<T = unknown>(method: string, ...args: unknown[]): Promise<T>;
  waitForEvent<T = unknown>(channel: string, match?: (data: T) => boolean): Promise<T>;
  url: string;
}

/** The per-frame fields every probe's rAF sampler records. */
export interface ProbeFrame {
  t: number;
  /** Gap since the previous sampled frame, ms. */
  gap: number;
  /** Rendered text length of the live think body; -1 when absent. */
  tick: number;
  /** Mounted timeline row count. */
  rows: number;
}

const PROBE_HISTORY_TURNS = 8;

/**
 * Seeds one project holding one thread with enough history that the pane
 * genuinely scrolls, and returns its thread id. Only the names differ per
 * probe, so several seeded probes coexist in one harness reset.
 */
export async function seedProbeThread(
  harness: ProbeHarness,
  names: { project: string; thread: string },
): Promise<string> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: names.project,
        repo: {},
        threads: [
          {
            title: names.thread,
            turns: Array.from({ length: PROBE_HISTORY_TURNS }, (_, i) => ({
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
  return seed.projects[0].threadIds[0];
}

/**
 * Opens a thread seeded by `seedProbeThread` and lets the thread-switch
 * restore settle (warm gate) before the caller starts streaming.
 */
export async function openProbeThread(page: Page, url: string, thread: string): Promise<void> {
  await page.goto(url);
  await page.getByText(thread).click();
  await expect(page.getByText(`history question ${PROBE_HISTORY_TURNS - 1}`)).toBeVisible();
  await page.waitForTimeout(1500);
}

/**
 * Stops the in-page sampler and pulls both dumps out together. The frame
 * samples are whatever shape the spec's own sampler pushed.
 */
export async function collectRevealProbe<S extends ProbeFrame>(
  page: Page,
): Promise<{ records: TraceRecord[]; samples: S[] }> {
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
    samples: collected.samples as S[],
  };
}

// ── Folds ──────────────────────────────────────────────────────────────

export interface ReleaseEvent {
  t: number;
  from: number;
  to: number;
  rafGapMs: number;
}

/** Row-count releases: how many rows mounted between adjacent frames. */
export function summarizeReleases(samples: readonly ProbeFrame[]): ReleaseEvent[] {
  const releases: ReleaseEvent[] = [];
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
  return releases;
}

export interface GapSummary {
  p99: number;
  max: number;
  over25: number;
  over50: number;
}

/**
 * rAF gap stats over the streaming portion — from the first frame that
 * saw a think body onward. No such frame means nothing streamed, and gap
 * stats over the idle prelude would be noise, so that reports zeros.
 */
export function summarizeGaps(samples: readonly ProbeFrame[]): GapSummary {
  const firstTickAt = samples.find((s) => s.tick >= 0)?.t;
  if (firstTickAt === undefined) return { p99: 0, max: 0, over25: 0, over50: 0 };
  const gaps = samples.filter((s) => s.t >= firstTickAt).map((s) => s.gap);
  gaps.sort((a, b) => a - b);
  return {
    p99: gaps[Math.floor(gaps.length * 0.99)] ?? 0,
    max: gaps[gaps.length - 1] ?? 0,
    over25: gaps.filter((g) => g > 25).length,
    over50: gaps.filter((g) => g > 50).length,
  };
}

export interface WriteTraceSummary {
  writesByCaller: Record<string, { count: number; maxJump: number }>;
  chases: unknown[];
}

/**
 * Scroll-write trace fold: per-caller write count + largest single write,
 * plus the spring's chase records. (boundary-probe keeps its own richer
 * fold — it additionally tracks the content-layer lease.)
 */
export function summarizeWriteTrace(records: readonly TraceRecord[]): WriteTraceSummary {
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
  return { writesByCaller: Object.fromEntries(writesByCaller), chases };
}
