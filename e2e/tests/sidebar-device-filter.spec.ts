// Real sidebar menu/checkbox behavior and frontend-local reload persistence.
// Covers paired-computer name collisions, archived names, Popover focus,
// and restoring a unique visible label after hiding a computer and reloading.
import { test, expect, type SeedResult } from './fixtures.js';
import { launchHarness } from '../src/harness.js';
import { startTogether } from './launch-helpers.js';
import { headlessPairing } from './headless-pairing-helpers.js';

for (const compact of [false, true]) {
  test.describe(compact ? 'phone viewport' : 'desktop viewport', () => {
    test.use({ viewport: compact ? { width: 390, height: 844 } : { width: 1280, height: 900 }, hasTouch: compact });
    test('a hidden computer cannot add a path to a unique visible project after reload', async ({ page }) => {
      const [harness, remote] = await startTogether(launchHarness(), launchHarness());
      let pairing: Awaited<ReturnType<typeof headlessPairing>> | undefined;
      try {
        await harness.rpc('SetDeviceName', 'Label home');
        await remote.rpc('SetDeviceName', 'Label remote');
        for (const computer of [harness, remote]) await computer.rpc('HarnessSeed', { projects: [{ name: 'app', repo: {} }] });
        await harness.open(page);
        await page.getByRole('button', { name: 'Settings', exact: true }).click();
        await page.getByRole('tab', { name: 'Connect to a computer', exact: true }).click();
        pairing = await headlessPairing(remote);
        await page.getByRole('textbox', { name: /^(Computer address or pairing link|Pairing link)$/ }).fill(pairing.invite.url);
        await page.getByRole('button', { name: 'Connect', exact: true }).click();
        const verification = page.getByLabel('Verification number');
        await expect(verification).toBeVisible();
        await pairing.confirm((await verification.textContent())!.trim());
        await expect(page.getByTestId('attached-system')).toContainText('Connected');
        await page.getByRole('button', { name: 'Close Settings', exact: true }).click();
        const labels = page.getByTestId('project-item-label');
        await expect(labels).toHaveCount(2);
        await expect(labels.filter({ hasText: /^app$/ })).toHaveCount(0);
        await page.getByRole('button', { name: 'Filter projects by computer' }).click();
        await page.getByRole('menuitemcheckbox', { name: 'Label remote' }).click();
        await page.keyboard.press('Escape');
        await expect(labels).toHaveText(['app']);
        await page.reload();
        await expect(labels).toHaveText(['app']);
        if (compact) await page.getByTestId('project-item-menu').click();
        else await labels.click({ button: 'right' });
        await page.getByRole('menuitem', { name: 'Archive Project', exact: true }).click();
        await expect(page.getByRole('dialog')).toContainText('Hide "app" from the sidebar.');
        await page.getByRole('button', { name: 'Cancel', exact: true }).click();
        await page.getByRole('button', { name: 'Filter projects by computer' }).click();
        await page.getByRole('menuitem', { name: 'All computers' }).click();
        await page.keyboard.press('Escape');
        await expect(labels).toHaveCount(2);
        await expect(labels.filter({ hasText: /^app$/ })).toHaveCount(0);
        await page.getByRole('button', { name: 'Settings', exact: true }).click();
        await page.getByRole('tab', { name: 'Projects', exact: true }).click();
        const projectOptions = page.getByTestId('settings-projects-select').locator('option');
        for (const computer of ['Label home', 'Label remote']) {
          await page.getByRole('combobox', { name: 'Computer', exact: true }).selectOption({ label: computer });
          await expect(projectOptions).toHaveText(['app']);
        }
      } finally {
        try { await page.goto('about:blank'); } finally {
          try { pairing?.close(); } finally {
            try { await remote.close(); } finally { await harness.close(); }
          }
        }
      }
    });

    test('hidden archived projects do not expand a visible name into a path after reload', async ({ harness, page }) => {
      const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [
        { name: 'first-checkout', repo: {} }, { name: 'second-checkout', repo: {} },
      ] });
      for (const project of seed.projects) await harness.rpc('RenameProject', project.projectId, 'app');
      await harness.open(page);
      const labels = page.getByTestId('project-item-label');
      await expect(labels).toHaveCount(2);
      await expect(labels.filter({ hasText: /^app$/ })).toHaveCount(0);
      await harness.rpc('ArchiveProject', seed.projects[1].projectId);
      await expect(labels).toHaveText(['app']);
      await page.reload();
      await expect(labels).toHaveText(['app']);
      await harness.rpc('UnarchiveProject', seed.projects[1].projectId);
      await expect(labels).toHaveCount(2);
      await expect(labels.filter({ hasText: /^app$/ })).toHaveCount(0);
    });

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
      const trigger = page.getByRole('button', { name: 'Filter projects by computer' });
      await trigger.click();
      const checkbox = page.getByRole('menuitemcheckbox', { name: 'Sidebar test computer' });
      await expect(checkbox).toBeChecked();
      await checkbox.click();
      await expect(checkbox).not.toBeChecked();
      await page.keyboard.press('Escape');
      await expect(page.getByTestId('thread-group-row')).toHaveCount(0);
      await expect(page.getByText('No projects on the selected computers.')).toBeVisible();
      await page.reload();
      await expect(page.getByText('No projects on the selected computers.')).toBeVisible();
      await trigger.click();
      await expect(checkbox).not.toBeChecked();
      await testInfo.attach('device-filter', { body: await page.screenshot(), contentType: 'image/png' });
      await page.getByRole('menuitem', { name: 'All computers' }).click();
      await page.keyboard.press('Escape');
      await expect(page.getByTestId('thread-group-row')).toBeVisible();
      await expect(page.getByTestId('thread-row').filter({ hasText: 'Kept conversation' })).toBeVisible();
      const rows = await harness.rpc<Array<{ id: string; groupId?: string }>>('HarnessListThreadRows');
      expect(rows.find((row) => row.id === threadIds[0])?.groupId).toBe(group.id);
    });
  });
}
