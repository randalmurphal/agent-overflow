// `ao-harness bench` end to end, driven the way a human drives it: the
// real CLI binary as a subprocess, pointed at this worker's harness, with
// a Playwright page attached so the frontend bridge can answer.
//
// The arithmetic already has unit coverage (cmd/ao-harness/bench_report_test.go)
// and the bridge has its own spec. What only this level can prove is the
// join: that a CLI outside the app can reset an instance, reload the page
// it is holding, seed a fixture, arm the meters, drive a scenario to turn
// completion, and land a report with numbers a real browser produced.
import { execFile } from 'node:child_process';
import { readFile, readdir, mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import * as path from 'node:path';
import { promisify } from 'node:util';
import type { HarnessApp } from '../src/harness.js';
import { test, expect } from './fixtures.js';

const run = promisify(execFile);

// The bench binary is bin/ao-harness, built beside bin/agent-overflow by
// `make harness-build` (which `make e2e` depends on). AO_HARNESS_BIN names
// the backend, so the CLI is resolved as its sibling for the same reason
// the CLI itself does that: a fresh checkout needs no configuration.
function benchBinary(): string {
  const repoRoot = path.resolve(import.meta.dirname, '..', '..');
  const backend = process.env.AO_HARNESS_BIN ?? path.join(repoRoot, 'bin', 'agent-overflow');
  return path.join(path.dirname(backend), 'ao-harness');
}

interface BenchAggregate {
  runs: number;
  p50: number;
  p95: number;
  min: number;
  max: number;
  unit: string;
  lowerIsBetter: boolean;
}

interface BenchDocument {
  status: 'running' | 'succeeded' | 'failed';
  workload: string;
  scenario?: string;
  repeat: number;
  startedAt: string;
  instance: string;
  runs: Array<{
    run: number;
    durationMs: number;
    perf: {
      runId: string;
      samples: number;
      durationMs: number;
      frontend?: { frames: { frames: number; fps: number; p95Ms: number } } | null;
      frontendError?: string;
      backend: { heapBytes: { count: number; max: number } };
    };
  }>;
  aggregate: Record<string, BenchAggregate>;
}

/** Resolves once the in-page bridge answers, which is what bench needs. */
async function waitForBridge(harness: HarnessApp): Promise<void> {
  await expect
    .poll(
      async () => {
        try {
          await harness.rpc('HarnessUIQuery', { v: 1, kind: 'element', selector: 'body' });
          return true;
        } catch {
          return false;
        }
      },
      { timeout: 30_000 },
    )
    .toBe(true);
}

test('bench burst-stream drives a real turn and writes a report', async ({ harness, page }) => {
  // A bench resets, reloads, seeds, streams a whole turn and settles, all
  // through a real browser. Well clear of the default 30s.
  test.setTimeout(180_000);

  // The page must be attached BEFORE the CLI runs: perf lives in the
  // document, and bench refuses up front when nothing answers a ui-query.
  await page.goto(harness.url);
  await waitForBridge(harness);

  const outDir = await mkdtemp(path.join(tmpdir(), 'ao-bench-'));
  try {
    const { stdout } = await run(
      benchBinary(),
      ['--instance', harness.bootstrap.dataRoot, 'bench', 'burst-stream', '--out', outDir],
      { timeout: 150_000 },
    );
    // The terminal summary is the thing a human reads; a silent success
    // would be a regression on its own.
    expect(stdout).toContain('bench burst-stream');
    expect(stdout).toContain('frames.fps');

    const files = (await readdir(outDir)).filter((name) => name.endsWith('.json'));
    expect(files).toContain('burst-stream-checkpoint.json');
    const reports = files.filter((name) => name !== 'burst-stream-checkpoint.json');
    expect(reports, 'the bench must write exactly one final report').toHaveLength(1);
    expect(reports[0]!).toMatch(/^burst-stream-\d{8}-\d{6}\.json$/);

    const document = JSON.parse(
      await readFile(path.join(outDir, reports[0]!), 'utf8'),
    ) as BenchDocument;
    const checkpoint = JSON.parse(
      await readFile(path.join(outDir, 'burst-stream-checkpoint.json'), 'utf8'),
    ) as BenchDocument;
    expect(checkpoint.status).toBe('succeeded');
    expect(checkpoint.runs).toHaveLength(1);
    expect(document.workload).toBe('burst-stream');
    expect(document.scenario).toBe('bench-burst-stream');
    expect(document.repeat).toBe(1);
    expect(document.runs).toHaveLength(1);

    const [firstRun] = document.runs;
    // The scenario paces itself with real delays, so a run that finished
    // instantly means the turn was not actually streamed.
    expect(firstRun!.durationMs).toBeGreaterThan(1000);
    expect(firstRun!.perf.samples).toBeGreaterThan(0);
    expect(firstRun!.perf.frontendError ?? '').toBe('');
    expect(firstRun!.perf.frontend, 'the attached page must have answered').toBeTruthy();
    expect(firstRun!.perf.frontend!.frames.frames).toBeGreaterThan(0);
    expect(firstRun!.perf.backend.heapBytes.max).toBeGreaterThan(0);

    // The aggregate is what --baseline reads back, so it has to be there
    // and it has to carry its direction.
    const fps = document.aggregate['frames.fps'];
    expect(fps, 'frames.fps must be in the aggregate').toBeTruthy();
    expect(fps!.runs).toBe(1);
    expect(fps!.lowerIsBetter).toBe(false);
    expect(fps!.p50).toBeGreaterThan(0);
    const duration = document.aggregate['duration.ms'];
    expect(duration!.lowerIsBetter).toBe(true);
    expect(duration!.p50).toBeGreaterThan(0);
  } finally {
    await rm(outDir, { recursive: true, force: true });
  }
});

test('health rolls up the same instance and exits ok', async ({ harness, page }) => {
  test.setTimeout(60_000);
  await page.goto(harness.url);
  await waitForBridge(harness);

  const { stdout } = await run(
    benchBinary(),
    ['--instance', harness.bootstrap.dataRoot, 'health'],
    { timeout: 30_000 },
  );
  expect(stdout).toContain('health ');
  for (const section of ['process', 'frontend-errors', 'ui-oracles', 'database', 'mocks']) {
    expect(stdout).toContain(section);
  }
  // A healthy instance must not report a renderer fault; that section is
  // the one the whole rollup exists to surface.
  expect(stdout).not.toMatch(/red\s+frontend-errors/);
});
