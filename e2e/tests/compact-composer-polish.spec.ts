// Phone gestures and geometry over the production SPA: a meter stays open,
// the header's facts line and the rail's tokens fit without horizontal
// overflow and open their pickers, a picked file lands in the draft, and an
// Bash row shows its description and expands to the full command and output.
import { test, expect } from './fixtures.js';
import {
  claudeScenario, claudeUsageResult, emit, seedAgentThread, startMock, toolResultLine, toolUseLine,
} from './agent-visibility-helpers.js';

test('phone meters, attachments, workspace and command details remain usable', async ({ harness, page }) => {
  const command = 'printf "%s\\n" "a deliberately long command argument that exceeds a phone header"\ngit status --short';
  const description = 'Print a long argument and check the working tree';
  const threadId = await seedAgentThread(harness, 'a-project-with-a-long-name-for-the-phone-footer', 'Phone polish');
  await harness.rpc('HarnessSetScenario', { scenario: claudeScenario('phone-polish', [emit([
    toolUseLine('msg-command', 'tool-command', 'Bash', { command, description }),
    toolResultLine('tool-command', 'the complete command output'),
    claudeUsageResult(123456, 45678),
  ])]) });
  await harness.open(page);
  await page.getByTestId('thread-row').click();
  await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'show command details', null);
  await harness.waitForEvent('provider:turn_completed');

  const ring = page.getByTestId('composer-rate-limit-7d').getByRole('button');
  await ring.tap();
  const tooltip = page.getByRole('tooltip');
  await expect(tooltip).toBeVisible();
  // The shared hover-close timer is 140ms. Observe beyond it so a synthetic
  // touch mouseleave cannot make a transient opening pass this assertion.
  await page.waitForTimeout(300);
  await expect(tooltip).toBeVisible();
  // Anchored at the ring, not a sheet at the bottom edge.
  const ringBox = (await ring.boundingBox())!;
  const tipBox = (await tooltip.boundingBox())!;
  expect(Math.abs(tipBox.y + tipBox.height - ringBox.y)).toBeLessThanOrEqual(16);
  // A tap anywhere else closes it; a tap on the ring again closes it too.
  await page.getByTestId('chat-header-title').tap();
  await expect(tooltip).toHaveCount(0);
  await ring.tap();
  await expect(tooltip).toBeVisible();
  await ring.tap();
  await expect(tooltip).toHaveCount(0);

  // No workspace strip on the phone: the token count rides the activity rail and
  // the workspace facts are the header's own line.
  await expect(page.getByTestId('composer-workspace-strip')).toHaveCount(0);
  const rail = page.getByTestId('activity-rail');
  await expect(rail.getByTestId('usage-chip-trigger')).toHaveText('45.7k');
  for (const width of [412, 360, 320]) {
    await page.setViewportSize({ width, height: 850 });
    const railRow = rail.locator('[data-activity-rail-row]');
    await expect.poll(() => railRow.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
    const facts = page.getByTestId('chat-header-facts');
    await expect.poll(() => facts.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
    for (const id of ['chat-header-project', 'chat-header-branch', 'chat-header-worktree', 'usage-chip-trigger', 'composer-attach']) {
      const box = await page.getByTestId(id).boundingBox();
      expect(box, id).not.toBeNull();
      expect(box!.x, id).toBeGreaterThanOrEqual(0);
      expect(box!.x + box!.width, id).toBeLessThanOrEqual(width);
    }
    // The worktree icon keeps its box at the end of the line at every width.
    const worktree = (await page.getByTestId('chat-header-worktree').boundingBox())!;
    const branch = (await page.getByTestId('chat-header-branch').boundingBox())!;
    expect(worktree.width).toBeGreaterThanOrEqual(20);
    expect(worktree.x).toBeGreaterThanOrEqual(branch.x + branch.width - 1);
  }
  await page.getByTestId('chat-header-branch').tap();
  await expect(page.getByRole('menu', { name: 'Branches', exact: true })).toBeVisible();
  await page.getByTestId('chat-header-title').tap();
  await expect(page.getByRole('menu', { name: 'Branches', exact: true })).toHaveCount(0);
  await page.getByTestId('chat-header-worktree').tap();
  await expect(page.getByRole('menu', { name: 'Workspace', exact: true })).toBeVisible();
  await expect(page.getByRole('menuitem', { name: /New Worktree/i })).toBeVisible();
  await page.getByTestId('chat-header-title').tap();
  await page.getByTestId('usage-chip-trigger').tap();
  await expect(page.getByTestId('usage-chip-popover')).toBeVisible();
  await expect(page.getByTestId('usage-chip-cost')).toContainText('$');
  await page.getByTestId('chat-header-title').tap();

  const chooser = page.waitForEvent('filechooser');
  await page.getByTestId('composer-attach').tap();
  await (await chooser).setFiles({ name: 'picked-note.txt', mimeType: 'text/plain', buffer: Buffer.from('picked on the phone') });
  await expect(page.getByLabel('Remove picked-note.txt')).toBeVisible();

  const toggle = page.getByTestId('command-output-toggle').first();
  await expect(page.getByTestId('command-output-command').first()).toHaveText(description);
  await expect(page.getByTestId('command-output-command').first()).toHaveAttribute('title', command);
  await toggle.tap();
  const full = page.getByTestId('command-output-full-command').first();
  await expect(full).toHaveText(command);
  await expect(page.getByText('the complete command output', { exact: true })).toBeVisible();
});
