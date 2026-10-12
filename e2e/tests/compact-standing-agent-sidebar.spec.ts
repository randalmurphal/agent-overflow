// The standing agent's sidebar row under the compact layout
// (docs/specs/standing-agents.md, "Sidebar"): the row, its state line and
// its children fit a phone width; the fold survives thread-screen
// navigation and reload; a collapsed agent keeps the focused child beneath
// it on the way back from the thread screen; and the done footer opens to
// its archived children.
import type { Locator } from '@playwright/test';

import { test, expect, type SeedResult } from './fixtures.js';
import { awaitToolAnswer, awaitTurnCompleted, setScenario, threadToolsScenario } from './thread-tools-helpers.js';
import { capture } from './wake-rows-helpers.js';

async function expectFits(target: Locator, viewportWidth: number): Promise<void> {
  const box = (await target.boundingBox())!;
  expect(box.x).toBeGreaterThanOrEqual(0);
  expect(box.x + box.width).toBeLessThanOrEqual(viewportWidth);
  expect(await target.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
}

test('an agent row folds, previews and lists its done children on a phone', async ({ harness, page }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'compact-agent-row', repo: {},
      threads: [
        { title: 'Release agent with a long name that truncates', turns: [{ userText: 'hi', items: [{ kind: 'assistant_text', summary: 'ready' }] }] },
        { title: 'Worker A', spawnedBy: 'Release agent with a long name that truncates', turns: [{ userText: 'a', items: [{ kind: 'assistant_text', summary: 'done' }] }] },
        { title: 'Worker B', spawnedBy: 'Release agent with a long name that truncates', turns: [{ userText: 'b', items: [{ kind: 'assistant_text', summary: 'done' }] }] },
        { title: 'Finished worker', spawnedBy: 'Release agent with a long name that truncates', turns: [{ userText: 'f', items: [{ kind: 'assistant_text', summary: 'done' }] }] },
        { title: 'Outside', turns: [{ userText: 'o', items: [{ kind: 'assistant_text', summary: 'done' }] }] },
      ],
    }],
  });
  const { path: cwd, threadIds } = seed.projects[0];
  const [agent, workerA, workerB, finished] = threadIds;
  await harness.rpc('PromoteStandingAgent', agent);
  await harness.rpc('ArchiveThread', finished);
  // The agent registers a loop so the row carries a state line.
  await setScenario(
    harness,
    cwd,
    threadToolsScenario({
      name: 'compact-agent-row',
      provider: 'claude',
      turns: [{ steps: [{ call: { tool: 'thread_loop', args: { label: 'board check', note: 'look at the board', every_seconds: 3600 } } }], text: 'Scheduled.' }],
      afterTurns: 'silent',
    }),
  );
  await harness.rpc('StartSession', agent);
  await harness.rpc('SendMessage', agent, 'set up the loop', null);
  const loop = await awaitToolAnswer(harness, { tool: 'thread_loop', cwd });
  expect(loop.isError, loop.text).toBe(false);
  await awaitTurnCompleted(harness, agent);

  await harness.open(page);
  const width = page.viewportSize()!.width;
  const agentRow = page.locator(`[data-sidebar-thread-id="${agent}"]`);
  const members = page.locator('[data-agent-member] [data-sidebar-thread-id]');
  const chevron = agentRow.getByTestId('thread-row-agent-expand');
  const footer = page.getByTestId('thread-row-agent-done');
  await expect(agentRow).toHaveAttribute('data-sidebar-agent', '');
  await expect(members).toHaveCount(2);
  await expect(footer.getByTestId('thread-row-agent-done-label')).toHaveText('1 done');
  // The state line is the row's sibling inside its shell.
  const state = page.locator(`[data-testid="thread-row-shell"]:has([data-sidebar-thread-id="${agent}"]) [data-testid="thread-row-agent-state"]`);
  await expect(state).toHaveText(/^loop board check · next (59m|1h)$/);
  for (const target of [agentRow, state, page.locator(`[data-sidebar-thread-id="${workerA}"]`), footer]) await expectFits(target, width);
  // The chevron is a touch target of its own.
  expect((await chevron.boundingBox())!.width).toBeGreaterThanOrEqual(16);
  await capture(page, 'compact-agent-expanded.png', agentRow, footer);

  // The fold holds through the thread screen and a reload.
  await chevron.click();
  await expect(agentRow).toHaveAttribute('data-expanded', 'false');
  await expect(agentRow.getByTestId('thread-row-agent-count')).toHaveText('2');
  await expect(members).toHaveCount(0);
  await expect(footer).toHaveCount(0);
  await page.locator(`[data-sidebar-thread-id="${threadIds[4]}"]`).click();
  await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'thread');
  await page.getByTestId('compact-back').click();
  await expect(agentRow).toHaveAttribute('data-expanded', 'false');
  await capture(page, 'compact-agent-folded.png', agentRow);

  // A collapsed agent previews the focused child on the way back.
  await chevron.click();
  await page.locator(`[data-sidebar-thread-id="${workerB}"]`).click();
  await expect(page.getByTestId('chat-header-title')).toHaveText('Worker B');
  await page.getByTestId('compact-back').click();
  await chevron.click();
  await expect(members).toHaveCount(1);
  await expect(page.locator(`[data-sidebar-thread-id="${workerB}"]`)).toBeVisible();
  await capture(page, 'compact-agent-collapsed-preview.png', agentRow, page.locator(`[data-sidebar-thread-id="${workerB}"]`));
  await page.reload();
  if (await page.getByTestId('compact-back').isVisible()) await page.getByTestId('compact-back').click();
  await expect(agentRow).toHaveAttribute('data-expanded', 'false');
  await expect(members).toHaveCount(1);

  // The done footer opens to the archived child, dimmed, behind the rail.
  await chevron.click();
  await expect(members).toHaveCount(2);
  await footer.getByTestId('thread-row-agent-done-toggle').click();
  const done = page.locator(`[data-sidebar-thread-id="${finished}"]`);
  await expect(done).toHaveAttribute('data-archived-child', 'true');
  await expectFits(done, width);
  await capture(page, 'compact-agent-done-footer.png', agentRow, footer, done);
  await done.click();
  await expect(page.getByTestId('chat-header-title')).toHaveText('Finished worker');
});
