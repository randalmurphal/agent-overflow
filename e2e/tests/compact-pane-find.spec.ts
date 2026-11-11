// The pane find bar under the compact layout and the browser's Back (the
// ladder utils/stepBack.ts runs): with Settings opened over a thread whose
// find is open, Back closes Settings first and leaves the find open; the
// next Back closes the find, and the one after returns to the list. With
// find open in a thread and an agent pane opened from it on screen (focus
// stays on the thread), Back closes the agent pane and leaves the thread's
// find open; mod+f there opens the agent pane's own find, which Back closes
// first. Desktop coverage: pane-find.spec.ts.
import { test, expect, type SeedResult } from './fixtures.js';
import {
  RESULT_LINE, asyncAgentAckLine, backgroundTasksChangedLine, claudeScenario,
  emit, seedAgentThread, startMock, taskStartedLine, toolUseLine,
} from './agent-visibility-helpers.js';

test('Back closes the overlay above an open find before the find', async ({ harness, page }) => {
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'compact-find', repo: {}, threads: [
      { title: 'Phone find', provider: 'claude', turns: [{ userText: 'Hello', items: [{ kind: 'assistant_text', summary: 'phone-needle answer' }] }] },
    ] }],
  });
  await harness.open(page);
  const screen = page.locator('html');
  await expect(screen).toHaveAttribute('data-compact-screen', 'list');
  await page.getByText('Phone find', { exact: true }).click();
  await expect(screen).toHaveAttribute('data-compact-screen', 'thread');

  await page.getByLabel('Message Input').click();
  await page.keyboard.press('ControlOrMeta+f');
  const bar = page.getByTestId('pane-find-bar');
  await expect(bar).toBeVisible();
  await page.keyboard.type('phone-needle');
  await expect(bar.getByTestId('pane-find-count')).toHaveText('1/1');

  // settings.open is not reachable from a text field.
  await page.getByTestId('chat-header-title').click();
  await page.keyboard.press('ControlOrMeta+,');
  const settings = page.getByTestId('settings-overlay');
  await expect(settings).toBeVisible();

  await page.goBack();
  await expect(settings).toHaveCount(0);
  await expect(bar).toBeVisible();
  await expect(screen).toHaveAttribute('data-compact-screen', 'thread');

  await page.goBack();
  await expect(bar).toHaveCount(0);
  await expect(screen).toHaveAttribute('data-compact-screen', 'thread');

  await page.goBack();
  await expect(screen).toHaveAttribute('data-compact-screen', 'list');
});

test('Back resolves the find on the pane on screen, not the thread focus stayed on', async ({ harness, page }) => {
  const description = 'Check the release notes against the tags';
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('compact-find-agent', [
      emit([
        toolUseLine('msg-notes', 'tu-notes', 'Agent', { description, subagent_type: 'general-purpose' }),
        taskStartedLine('task-notes', 'tu-notes', description),
        asyncAgentAckLine('tu-notes', 'task-notes', description),
        backgroundTasksChangedLine([{ task_id: 'task-notes', task_type: 'local_agent', description }]),
        RESULT_LINE,
      ]),
    ]),
  });
  const threadId = await seedAgentThread(harness, 'compact-find-agent-app', 'Find beside agent');
  await harness.open(page);
  const screen = page.locator('html');
  await page.getByTestId('thread-row').filter({ hasText: 'Find beside agent' }).click();
  await expect(screen).toHaveAttribute('data-compact-screen', 'thread');
  await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'check the notes', null);
  await harness.waitForEvent('provider:turn_completed');

  const thread = page.locator('section[data-pane-kind="thread"]');
  await page.getByLabel('Message Input').click();
  await page.keyboard.press('ControlOrMeta+f');
  const threadBar = thread.getByTestId('pane-find-bar');
  await expect(threadBar).toBeVisible();
  await page.keyboard.type('Ready');

  const agentBody = page.getByTestId('companion-pane-agent-body');
  const agent = page.locator('section[data-pane-kind="agent"]');
  // Back reads the pane under the strip's centre: wait out the glide.
  const openAgent = async () => {
    await thread.locator('[data-item-id="tu-notes"]').getByTestId('agent-row-open-pane').click();
    await expect(agent).toBeInViewport({ ratio: 1 });
  };
  await openAgent();

  // The agent pane is on screen with no find: Back closes it, not the thread's find.
  await page.goBack();
  await expect(agentBody).toHaveCount(0);
  await expect(threadBar).toBeVisible();
  await expect(screen).toHaveAttribute('data-compact-screen', 'thread');

  // mod+f on the agent pane opens its own find, and Back closes that first.
  await openAgent();
  await page.keyboard.press('ControlOrMeta+f');
  await expect(agent.getByTestId('pane-find-bar')).toBeVisible();
  await page.goBack();
  await expect(agent.getByTestId('pane-find-bar')).toHaveCount(0);
  await expect(agent).toBeInViewport({ ratio: 1 });
  await expect(threadBar).toBeAttached();
  await page.goBack();
  await expect(agentBody).toHaveCount(0);
  await expect(threadBar).toBeVisible();

  await page.goBack();
  await expect(threadBar).toHaveCount(0);
  await expect(screen).toHaveAttribute('data-compact-screen', 'thread');
});
