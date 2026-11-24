// A thread_status wait over threads on two computers when one of them is
// down, both of them real harness backends with their own stores and mock
// providers. The wait answers for the thread that resolved, reads the one
// the down computer may hold as unresolved with the reason on its line, and
// names that computer under errors; the timeline row shows all three, the
// unresolved reference as text rather than a control that opens it.
// Specs: docs/specs/agent-thread-tools.md (`thread_status`).
import { test, expect } from './base-test.js';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { headlessPairing } from './headless-pairing-helpers.js';
import { expandRow } from './ao-tool-rows-helpers.js';
import { aoRow, openThread } from './ao-tool-rows-more-helpers.js';
import { awaitToolAnswer, setScenario, threadToolsScenario } from './thread-tools-helpers.js';

test.describe.configure({ mode: 'serial' });

let home: HarnessApp;
// Cleared once the test takes it down, so afterAll does not close it twice.
let bench: HarnessApp | undefined;

test.beforeAll(async () => {
  test.setTimeout(120_000);
  home = await launchHarness();
  bench = await launchHarness();
  const pairing = await headlessPairing(bench);
  try {
    const attachment = await home.rpc<{ id: string; verificationNumber: string }>('AddBackend', pairing.invite.url);
    await pairing.confirm(attachment.verificationNumber);
    await home.rpc('RenameBackend', attachment.id, 'Bench computer');
  } finally {
    pairing.close();
  }
});

test.afterAll(async () => {
  await home?.close();
  await bench?.close();
});

interface SeededProject {
  projectId: string;
  path: string;
  threadIds: string[];
}

async function seed(app: HarnessApp, name: string, titles: string[]): Promise<SeededProject> {
  const result = await app.rpc<{ projects: SeededProject[] }>('HarnessSeed', {
    projects: [{ name, repo: {}, threads: titles.map((title) => ({ title, provider: 'claude' })) }],
  });
  return result.projects[0];
}

interface StatusAnswer {
  threads: Array<{ thread_id: string; state: string; error?: string; error_code?: string }>;
  errors?: Array<{ computer: string; error: string }>;
  note?: string;
}

test('a wait reads a thread a down computer may hold as unresolved beside the thread that resolved', async ({ page }) => {
  test.setTimeout(180_000);
  const local = await seed(home, 'status-unanswered-home', ['Unanswered watch caller', 'Watched here']);
  const there = await seed(bench!, 'status-unanswered-bench', ['Watched on the bench']);
  const [caller, watchedHere] = local.threadIds;
  const watchedThere = there.threadIds[0];
  await setScenario(home, local.path, threadToolsScenario({
    name: 'status-unanswered',
    provider: 'claude',
    turns: [{
      steps: [{ call: { tool: 'thread_status', args: { thread_ids: [watchedHere, watchedThere], wait_seconds: 5 }, timeoutMs: 90_000 } }],
      text: 'Read what answered.',
    }],
  }));
  const down = bench!;
  bench = undefined;
  await down.close();

  await home.startSession(caller);
  await home.send(caller, 'wait on both threads');
  const answer = await awaitToolAnswer<StatusAnswer>(home, { tool: 'thread_status', cwd: local.path, timeoutMs: 120_000 });
  expect(answer.isError, answer.text).toBe(false);
  const [here, unresolved] = answer.value!.threads;
  expect(here.thread_id).toBe(watchedHere);
  expect(here.state).not.toBe('unresolved');
  expect(here.error).toBeUndefined();
  expect(unresolved.thread_id).toBe(watchedThere);
  expect(unresolved.state).toBe('unresolved');
  expect(unresolved.error_code).toBe('thread_resolution_incomplete');
  expect(unresolved.error).toContain('Bench computer did not answer');
  expect(answer.value!.errors?.map((failure) => failure.computer)).toEqual(['Bench computer']);

  await openThread(home, page, 'Unanswered watch caller');
  const row = aoRow(page, 'thread_status').first();
  await expandRow(row);
  const lines = row.getByTestId('tool-call-card-ao-line');
  await expect(lines).toHaveCount(2);
  await expect(lines.nth(0)).toContainText('Watched here');
  await expect(lines.nth(0)).not.toContainText('did not answer');
  await expect(lines.nth(1)).toContainText('unresolved');
  await expect(lines.nth(1)).toContainText('Bench computer did not answer');
  // The reference matched no thread, so the line is text, never a control
  // that would try to open it.
  await expect(lines.nth(1).getByRole('button')).toHaveCount(0);
  await expect(lines.nth(1).getByTestId('tool-call-card-ao-line-link')).toHaveCount(0);
  await expect(lines.nth(1).getByTestId('tool-call-card-ao-line-title')).toBeVisible();
  await expect(lines.nth(0).getByTestId('tool-call-card-ao-line-link')).toHaveCount(1);
  await expect(row.locator('[data-testid="tool-call-card-ao-result-fact"][data-fact="Bench computer"]')).toBeVisible();
  await row.screenshot({ path: test.info().outputPath('thread-status-unresolved.png') });
});
