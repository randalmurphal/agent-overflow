// Phone gestures and geometry over the production SPA: a meter stays open,
// workspace/cost fit without horizontal overflow, a picked file lands in the draft, and an
// expanded Bash row exposes the full command before its output.
import { test, expect } from './fixtures.js';
import {
  claudeScenario, claudeUsageResult, emit, seedAgentThread, startMock, toolResultLine, toolUseLine,
} from './agent-visibility-helpers.js';

test('phone meters, attachments, workspace and command details remain usable', async ({ harness, page }) => {
  const command = 'printf "%s\\n" "a deliberately long command argument that exceeds a phone header"\ngit status --short';
  const threadId = await seedAgentThread(harness, 'a-project-with-a-long-name-for-the-phone-footer', 'Phone polish');
  await harness.rpc('HarnessSetScenario', { scenario: claudeScenario('phone-polish', [emit([
    toolUseLine('msg-command', 'tool-command', 'Bash', { command }),
    toolResultLine('tool-command', 'the complete command output'),
    claudeUsageResult(123456, 45678),
  ])]) });
  await harness.open(page);
  await page.getByTestId('thread-row').click();
  await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'show command details', null);
  await harness.waitForEvent('provider:turn_completed');

  await page.getByTestId('composer-rate-limit-7d').getByRole('button').tap();
  await expect(page.getByRole('tooltip')).toBeVisible();
  // The shared hover-close timer is 140ms. Observe beyond it so a synthetic
  // touch mouseleave cannot make a transient opening pass this assertion.
  await page.waitForTimeout(300);
  await expect(page.getByRole('tooltip')).toBeVisible();
  await page.getByTestId('chat-header-title').tap();
  await expect(page.getByRole('tooltip')).toHaveCount(0);

  await expect(page.getByTestId('usage-chip-trigger')).toBeVisible();
  for (const width of [412, 360, 320]) {
    await page.setViewportSize({ width, height: 850 });
    const strip = page.getByTestId('composer-workspace-strip');
    await expect.poll(() => strip.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
    for (const id of ['workspace-picker-trigger', 'usage-chip-trigger', 'composer-attach']) {
      const box = await page.getByTestId(id).boundingBox();
      expect(box).not.toBeNull();
      expect(box!.x).toBeGreaterThanOrEqual(0);
      expect(box!.x + box!.width).toBeLessThanOrEqual(width);
    }
  }
  await page.getByTestId('workspace-picker-trigger').tap();
  await expect(page.getByRole('menu', { name: 'Workspace options' })).toBeVisible();
  await page.getByRole('menuitem', { name: /Branch/ }).tap();
  await expect(page.getByRole('menu', { name: 'Branches', exact: true })).toBeVisible();
  await page.getByTestId('chat-header-title').tap();

  const chooser = page.waitForEvent('filechooser');
  await page.getByTestId('composer-attach').tap();
  await (await chooser).setFiles({ name: 'picked-note.txt', mimeType: 'text/plain', buffer: Buffer.from('picked on the phone') });
  await expect(page.getByLabel('Remove picked-note.txt')).toBeVisible();

  const toggle = page.getByTestId('command-output-toggle').first();
  await toggle.tap();
  const full = page.getByTestId('command-output-full-command').first();
  await expect(full).toHaveText(command);
  await expect(page.getByText('the complete command output', { exact: true })).toBeVisible();
});
