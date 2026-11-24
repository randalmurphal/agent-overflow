// The ao-harness CLI end to end, driven the way a human drives it: the
// real CLI binary as a subprocess, pointed at this worker's harness, with
// a Playwright page attached where the frontend bridge has to answer.
//
// The arithmetic already has unit coverage (cmd/ao-harness/bench_report_test.go)
// and the bridge has its own spec. What only this level can prove is the
// join: that a CLI outside the app can reset an instance, reload the page
// it is holding, seed a fixture, arm the meters, drive a scenario to turn
// completion, and land a report with numbers a real browser produced; and
// that `send --wait` names its message's turn from the completions the
// backend really publishes, and refuses a working thread.
import { readFile, readdir, mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import * as path from 'node:path';
import type { HarnessApp } from '../src/harness.js';
import { execProduct, productBinary } from '../src/product-process.js';
import { test, expect, type SeedResult } from './fixtures.js';
import { advanceGate, awaitGate, setScenario, threadToolsScenario } from './thread-tools-helpers.js';

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
  await harness.open(page);
  await waitForBridge(harness);

  const outDir = await mkdtemp(path.join(tmpdir(), 'ao-bench-'));
  try {
    const { stdout } = await execProduct(
      productBinary('ao-harness'),
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
  await harness.open(page);
  await waitForBridge(harness);

  // A red rollup exits non-zero with its sections on stdout, which
  // execFile's own message leaves out; the failure has to name the section.
  const { stdout } = await execProduct(
    productBinary('ao-harness'),
    ['--instance', harness.bootstrap.dataRoot, 'health'],
    { timeout: 30_000 },
  ).catch((err: Error & { stdout?: string }) => {
    throw new Error(`${err.message}\n${err.stdout ?? ''}`, { cause: err });
  });
  expect(stdout).toContain('health ');
  for (const section of ['process', 'frontend-errors', 'ui-oracles', 'database', 'mocks']) {
    expect(stdout).toContain(section);
  }
  // A healthy instance must not report a renderer fault; that section is
  // the one the whole rollup exists to surface.
  expect(stdout).not.toMatch(/red\s+frontend-errors/);
});

for (const provider of ['claude', 'codex'] as const) {
  test(`send --wait reports the turn its message is placed in and refuses a working thread (${provider})`, async ({ harness }) => {
    test.setTimeout(90_000);
    const gate = `cli-send-wait-held-${provider}`;
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [{ name: 'CLI send', repo: {}, threads: [{ title: 'CLI send thread', provider }] }],
    });
    const cwd = seed.projects[0].path;
    const threadId = seed.projects[0].threadIds[0];
    await setScenario(harness, cwd, threadToolsScenario({
      name: `cli-send-wait-${provider}`,
      provider,
      turns: [{ text: 'The first answer.' }, { steps: [{ gate }], text: 'The held answer.' }],
    }));
    const cli = (...args: string[]) =>
      execProduct(productBinary('ao-harness'), ['--instance', harness.bootstrap.dataRoot, '-o', 'json', ...args], { timeout: 60_000 });

    // At rest: the CLI exits 0 with the completion of its message's turn.
    const { stdout } = await cli('send', '--thread', threadId, '--wait', 'first message');
    expect((JSON.parse(stdout) as { completed: unknown }).completed).toMatchObject({ threadId, turnIndex: 0 });

    // Working: the backend refuses the send. Were it taken, it would queue
    // behind the held turn, and the turn it runs in could not be named.
    const parked = awaitGate(harness, gate, cwd);
    await harness.send(threadId, 'held message');
    const { mockId } = await parked;
    const refused = await cli('send', '--thread', threadId, '--wait', 'refused message').then(
      () => ({ code: 0, stderr: '' }),
      (error: { code?: number; stderr?: string }) => ({ code: error.code ?? -1, stderr: error.stderr ?? '' }),
    );
    expect(refused.code, 'send --wait to a working thread must fail').toBe(1);
    expect(refused.stderr).toContain('thread is working: a message sent now would queue behind that work');
    await advanceGate(harness, mockId, gate);
  });
}
