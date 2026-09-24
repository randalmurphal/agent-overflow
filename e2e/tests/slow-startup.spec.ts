// A backend that is slow to start and a catalog that is slow to answer.
// Covers the readiness gate's starting report (internal/startupprogress)
// against a harness held before App.Start (AO_HARNESS_HOLD_STARTUP), the
// page's 'starting' transport state, the startup screen and the sidebar's
// catalog rows, which name the boot phase and never present an unloaded
// catalog as empty. The second case delays every ListThreads and
// ListProjects answer past the startup read deadline.
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { test, expect, type Page } from '@playwright/test';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import type { SeedResult } from './fixtures.js';

const LIST_THREADS = 1090132042;
const LIST_PROJECTS = 2721360259;

interface StartingReport {
  reason: string;
  phase: string;
  detail: string;
  startedAt: number;
  updatedAt: number;
  aliveAt: number;
}

async function readBootstrap(harness: HarnessApp): Promise<{ status: number; headers: Headers; body: string }> {
  const resp = await fetch(`http://127.0.0.1:${harness.bootstrap.port}/bootstrap.json`, {
    headers: { authorization: `Bearer ${harness.bootstrap.token}` },
  });
  return { status: resp.status, headers: resp.headers, body: await resp.text() };
}

function consoleProblems(page: Page): () => string[] {
  const problems: string[] = [];
  page.on('pageerror', (error) => problems.push(`pageerror: ${error.message}`));
  page.on('console', (msg) => {
    if (msg.text().includes('connection preparation failed')) problems.push(msg.text().slice(0, 200));
  });
  return () => problems;
}

test('a held boot shows its phase, never an empty sidebar, and loads once ready', async ({ page }) => {
  test.setTimeout(90_000);
  const dir = await mkdtemp(path.join(tmpdir(), 'ao-hold-startup-'));
  const release = path.join(dir, 'release');
  const harness = await launchHarness({ env: { AO_HARNESS_HOLD_STARTUP: release } });
  const problems = consoleProblems(page);
  try {
    // The wire: a readiness-gated bootstrap answers with the report.
    const gate = await readBootstrap(harness);
    expect(gate.status).toBe(503);
    expect(gate.headers.get('cache-control')).toContain('no-store');
    expect(gate.headers.get('retry-after')).toBe('1');
    const report = JSON.parse(gate.body) as StartingReport;
    expect(report).toMatchObject({ reason: 'starting', phase: 'harness.hold_startup', detail: 'Holding startup for a test' });

    await harness.open(page);
    await expect(page.getByTestId('startup-screen-phase')).toHaveText('Holding startup for a test');
    await expect(page.getByTestId('sidebar-catalog-loading')).toHaveAttribute('data-status', 'starting');
    await expect(page.getByTestId('sidebar-catalog-loading-label')).toHaveText('Holding startup for a test');
    // The page keeps reading the report: the elapsed line advances on the
    // heartbeat while the held phase makes no progress.
    const meta = page.getByTestId('startup-screen-meta');
    await expect(meta).toHaveText(/^\d+:\d\d elapsed$/, { timeout: 10_000 });
    const shown = await meta.textContent();
    await expect(meta).not.toHaveText(shown ?? '', { timeout: 10_000 });
    const later = JSON.parse((await readBootstrap(harness)).body) as StartingReport;
    expect(later.startedAt).toBe(report.startedAt);
    expect(later.aliveAt).toBeGreaterThan(report.aliveAt);
    expect(later.updatedAt).toBe(report.updatedAt);
    // Loading is not empty, and a first boot is not a connection problem.
    await expect(page.getByTestId('sidebar-projects-empty')).toHaveCount(0);
    await expect(page.getByTestId('pane-host-empty')).toHaveCount(0);
    await expect(page.getByTestId('transport-status-banner')).toHaveCount(0);

    await writeFile(release, '');
    await expect(page.getByTestId('startup-screen')).toHaveCount(0, { timeout: 30_000 });
    await expect(page.getByTestId('pane-host-empty')).toBeVisible();
    await expect(page.getByTestId('sidebar-catalog-status')).toHaveCount(0);
    await expect(page.getByTestId('sidebar-projects-empty')).toBeVisible();
    expect((await readBootstrap(harness)).status).toBe(200);
    expect(problems()).toEqual([]);
  } finally {
    await page.close();
    // A failed assertion must not leave the boot held: release it so the
    // harness shuts down through its ordinary path.
    await writeFile(release, '');
    await harness.close();
    await rm(dir, { recursive: true, force: true });
  }
});

test('a catalog answering after the startup deadline shows loading, then its rows, never empty', async ({ page }) => {
  test.setTimeout(60_000);
  const delayMs = 3000;
  const harness = await launchHarness();
  const problems = consoleProblems(page);
  try {
    await harness.rpc<SeedResult>('HarnessSeed', { projects: [{
      name: 'slow-catalog', repo: {}, threads: [{ title: 'Slow conversation', provider: 'claude',
        turns: [{ userText: 'Hello', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
    }] });
    // Every catalog read takes delayMs to answer, as on a backend busy with
    // its post-boot work. The boot read and the hello's refresh both wait.
    let delayed = 0;
    const pending = new Set<string>();
    await page.routeWebSocket(/\/ws(?:\?|$)/, (socket) => {
      const server = socket.connectToServer();
      socket.onMessage((message) => {
        const frame = JSON.parse(String(message)) as { type?: string; id?: string; methodId?: number };
        if (frame.type === 'rpc' && frame.id && (frame.methodId === LIST_THREADS || frame.methodId === LIST_PROJECTS)) {
          delayed++;
          pending.add(frame.id);
        }
        server.send(message);
      });
      server.onMessage((message) => {
        const frame = JSON.parse(String(message)) as { type?: string; id?: string };
        if (frame.type === 'rpc' && frame.id && pending.delete(frame.id)) {
          setTimeout(() => socket.send(message), delayMs);
        } else socket.send(message);
      });
    });
    // Sample the sidebar from the page itself, every animation frame.
    await page.addInitScript(() => {
      const samples: Array<{ at: number; loading: boolean; empty: boolean; rows: number }> = [];
      (window as unknown as { __sidebarSamples: typeof samples }).__sidebarSamples = samples;
      const sample = () => {
        samples.push({
          at: performance.now(),
          loading: document.querySelector('[data-testid="sidebar-catalog-loading"]') !== null,
          empty: document.querySelector('[data-testid="sidebar-projects-empty"]') !== null,
          rows: document.querySelectorAll('[data-testid="project-item"]').length,
        });
        requestAnimationFrame(sample);
      };
      requestAnimationFrame(sample);
    });

    await harness.open(page);
    await expect(page.getByTestId('project-item')).toHaveCount(1, { timeout: 20_000 });
    const samples = await page.evaluate(() =>
      (window as unknown as { __sidebarSamples: Array<{ at: number; loading: boolean; empty: boolean; rows: number }> }).__sidebarSamples);
    expect(delayed).toBeGreaterThanOrEqual(2);
    expect(samples.filter((entry) => entry.empty)).toEqual([]);
    const firstRows = samples.find((entry) => entry.rows > 0);
    const firstLoading = samples.find((entry) => entry.loading);
    expect(firstLoading).toBeDefined();
    expect(firstRows).toBeDefined();
    // Loading covers every frame from its first until the rows arrive.
    const between = samples.filter((entry) => entry.at >= firstLoading!.at && entry.at < firstRows!.at);
    expect(between.every((entry) => entry.loading)).toBe(true);
    expect(firstRows!.at - firstLoading!.at).toBeGreaterThan(delayMs - 1000);
    test.info().annotations.push({
      type: 'measurement',
      description: `delay=${delayMs}ms loading-first=${Math.round(firstLoading!.at)}ms rows-first=${Math.round(firstRows!.at)}ms`
        + ` loading-span=${Math.round(firstRows!.at - firstLoading!.at)}ms frames=${samples.length} empty-frames=0`
        + ` catalog-reads=${delayed}`,
    });
    console.log(`[slow-startup] ${test.info().annotations.at(-1)!.description}`);
    expect(problems()).toEqual([]);
  } finally {
    await page.close();
    await harness.close();
  }
});
