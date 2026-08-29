// Smoke suite for the agent test harness: proves the pieces an agent
// chains during a debugging session — boot, seed, live mock turns,
// frame-accurate stepping — against the real backend and real SPA.
//
// Sidebar visibility note: App.ListThreads deliberately hides "draft"
// threads (created but no items yet). A seeded thread without turns
// only appears in the sidebar after its first message lands, so live
// tests send first, then open the thread in the UI.
import { test, expect, type HarnessMockEvent, type SeedResult } from './fixtures.js';

test('boots headless, serves the real SPA, answers harness RPCs', async ({ harness, page }) => {
  const info = await harness.rpc<{ version: string; dbPath: string; mockProvider: string }>(
    'HarnessInfo',
  );
  expect(info.dbPath).toContain(harness.bootstrap.dataDir);
  expect(info.mockProvider).toBe(harness.bootstrap.mockProvider);

  await page.goto(harness.url);
  await expect(page).toHaveTitle('Agent Overflow');
  await expect(page.getByText('No projects yet')).toBeVisible();
});

test('seeded projects and history render in the UI', async ({ harness, page }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'seeded-app',
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [
          {
            title: 'Sorting question',
            turns: [
              {
                userText: 'How do I sort an array in JS?',
                items: [
                  { kind: 'assistant_text', summary: 'Use Array.prototype.sort with a comparator.' },
                ],
              },
            ],
          },
        ],
      },
    ],
  });
  expect(seed.projects[0].threadIds).toHaveLength(1);

  await page.goto(harness.url);
  await page.getByText('Sorting question').click();
  await expect(page.getByText('How do I sort an array in JS?')).toBeVisible();
  await expect(page.getByText('Use Array.prototype.sort with a comparator.')).toBeVisible();
});

test('a live mock turn runs the full pipeline and renders', async ({ harness, page }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'live-app', repo: {}, threads: [{ title: 'Live turn' }] }],
  });
  const threadId = seed.projects[0].threadIds[0];

  await harness.rpc('StartSession', threadId);
  await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'registered',
  );
  await harness.rpc('SendMessage', threadId, 'Say hello', null);

  // Deterministic wait on the wire, then semantic assertions in the DOM.
  await harness.waitForEvent('provider:turn_completed');
  await page.goto(harness.url);
  await page.getByText('Live turn').click();
  await expect(page.getByText('Say hello')).toBeVisible();
  await expect(
    page.getByText('Hello! This reply is streaming from the mock provider (turn 1).'),
  ).toBeVisible();

  // Second turn: waitForEvent consumes matches, so this wait observes
  // the SECOND turn_completed — not the first one replayed from the
  // event log.
  await harness.rpc('SendMessage', threadId, 'Say hello again', null);
  await harness.waitForEvent('provider:turn_completed');
  await expect(
    page.getByText('Hello! This reply is streaming from the mock provider (turn 2).'),
  ).toBeVisible();
});

test('step-gated scenario advances frame by frame via HarnessMockCommand', async ({
  harness,
  page,
}) => {
  await harness.rpc('HarnessSetScenario', { name: 'step-gated' });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'gated-app', repo: {}, threads: [{ title: 'Gated turn' }] }],
  });
  const threadId = seed.projects[0].threadIds[0];

  await harness.rpc('StartSession', threadId);
  const registered = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'registered',
  );
  expect(registered.scenario).toBe('step-gated');
  await harness.rpc('SendMessage', threadId, 'stream carefully', null);

  // The scenario parks at its first gate; the user message has landed,
  // so the thread is sidebar-visible while zero frames have streamed.
  await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'waiting_signal' && ev.report.detail === 'block-start',
  );
  await page.goto(harness.url);
  await page.getByText('Gated turn').click();
  await expect(page.getByText('stream carefully')).toBeVisible();
  await expect(page.getByText('First chunk.')).not.toBeVisible();

  const advance = (name: string) =>
    harness.rpc('HarnessMockCommand', registered.mockId, { type: 'advance', name });

  await advance('block-start');
  await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'waiting_signal' && ev.report.detail === 'first-delta',
  );
  await advance('first-delta');
  await expect(page.getByText('First chunk.')).toBeVisible();
  await expect(page.getByText('Second chunk.')).not.toBeVisible();

  await advance('second-delta');
  await advance('finish');
  await harness.waitForEvent('provider:turn_completed');
  await expect(page.getByText('First chunk. Second chunk.')).toBeVisible();
});

test('reset returns the harness to a blank slate', async ({ harness, page }) => {
  const seedThrowaway = () =>
    harness.rpc<SeedResult>('HarnessSeed', {
      projects: [
        {
          name: 'throwaway',
          repo: {},
          threads: [
            {
              title: 'Doomed thread',
              turns: [{ userText: 'remember me', items: [] }],
            },
          ],
        },
      ],
    });
  await seedThrowaway();
  await harness.rpc('HarnessSetScenario', { name: 'step-gated' });
  await page.goto(harness.url);
  await expect(page.getByText('Doomed thread')).toBeVisible();

  await harness.reset();
  await page.reload();
  await expect(page.getByText('No projects yet')).toBeVisible();
  await expect(page.getByText('Doomed thread')).not.toBeVisible();

  // Reset covers harness-owned state too: scenario rules are gone, and
  // the generated workspace was removed so the same project name seeds
  // cleanly again.
  const scenarios = await harness.rpc<{ rules: unknown[] }>('HarnessListScenarios');
  expect(scenarios.rules).toHaveLength(0);
  await seedThrowaway();
  await page.reload();
  await expect(page.getByText('Doomed thread')).toBeVisible();
});

test('a session-scoped scenario rule binds only after the session resumes', async ({
  harness,
}) => {
  // sessionRef is matched against the mock's registration ResumeRef, which
  // is argv-derived: it is EMPTY on a session's first spawn, because there
  // is nothing to resume yet. Proving that boundary needs a real restart —
  // no backend unit test can produce it — so this test does the whole
  // sequence: first spawn, read the provider session id off the thread,
  // install the session-scoped rule, restart, and watch the second spawn
  // pick it up.
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'session-scoped', repo: {}, threads: [{ title: 'Scoped thread' }] }],
  });
  const threadId = seed.projects[0].threadIds[0];

  // A catch-all rule, so the first spawn has something specific to match
  // and "the session rule won" cannot be confused with "the default ran".
  await harness.rpc('HarnessSetScenario', { name: 'thinking-then-text' });

  await harness.rpc('StartSession', threadId);
  const firstSpawn = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'registered',
  );
  expect(firstSpawn.scenario).toBe('thinking-then-text');

  // The session id only exists once a turn has run against the provider.
  await harness.rpc('SendMessage', threadId, 'first turn', null);
  await harness.waitForEvent('provider:turn_completed');
  const thread = await harness.rpc<{ sessionRef: string }>('GetThread', threadId);
  expect(thread.sessionRef).not.toBe('');

  // Same provider, same workspace, narrower selector: this must beat the
  // catch-all on the resumed spawn.
  await harness.rpc('HarnessSetScenario', {
    name: 'streaming-text',
    sessionRef: thread.sessionRef,
  });

  await harness.rpc('StopSession', threadId);
  await harness.rpc('StartSession', threadId);
  const resumedSpawn = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'registered',
  );
  expect(resumedSpawn.mockId).not.toBe(firstSpawn.mockId);
  expect(resumedSpawn.scenario).toBe('streaming-text');
});
