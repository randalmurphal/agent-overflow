// Real sidebar menu/checkbox behavior and frontend-local reload persistence.
// Multi-host ownership and merged-repository filtering are covered by the
// rendered component test; this reaches Popover positioning and browser focus.
import { test, expect, type SeedResult } from './fixtures.js';

for (const compact of [false, true]) {
  test.describe(compact ? 'phone viewport' : 'desktop viewport', () => {
    test.use({ viewport: compact ? { width: 390, height: 844 } : { width: 1280, height: 900 }, hasTouch: compact });
    test('device filtering persists without removing projects or thread groups', async ({ harness, page }, testInfo) => {
      await harness.rpc('SetDeviceName', 'Sidebar test computer');
      const seed = await harness.rpc<SeedResult>('HarnessSeed', {
        projects: [{ name: 'Device filter project', repo: {}, threads: [
          { title: 'Kept conversation', turns: [{ userText: 'hello', items: [{ kind: 'assistant_text', summary: 'done' }] }] },
        ] }],
      });
      const { projectId, threadIds } = seed.projects[0];
      const group = await harness.rpc<{ id: string }>('CreateThreadGroup', projectId, 'Device filter group');
      await harness.rpc('SetThreadGroup', threadIds, group.id);
      await harness.open(page);
      await expect(page.getByTestId('thread-group-row')).toBeVisible();
      const trigger = page.getByRole('button', { name: 'Filter projects by device' });
      await trigger.click();
      const checkbox = page.getByRole('menuitemcheckbox', { name: 'Sidebar test computer' });
      await expect(checkbox).toBeChecked();
      await checkbox.click();
      await expect(checkbox).not.toBeChecked();
      await page.keyboard.press('Escape');
      await expect(page.getByTestId('thread-group-row')).toHaveCount(0);
      await expect(page.getByText('No projects on the selected devices.')).toBeVisible();
      await page.reload();
      await expect(page.getByText('No projects on the selected devices.')).toBeVisible();
      await trigger.click();
      await expect(checkbox).not.toBeChecked();
      await testInfo.attach('device-filter', { body: await page.screenshot(), contentType: 'image/png' });
      await page.getByRole('menuitem', { name: 'All devices' }).click();
      await page.keyboard.press('Escape');
      await expect(page.getByTestId('thread-group-row')).toBeVisible();
      await expect(page.getByTestId('thread-row').filter({ hasText: 'Kept conversation' })).toBeVisible();
      const rows = await harness.rpc<Array<{ id: string; groupId?: string }>>('HarnessListThreadRows');
      expect(rows.find((row) => row.id === threadIds[0])?.groupId).toBe(group.id);
    });
  });
}
