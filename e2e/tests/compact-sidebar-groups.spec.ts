// Compact group collapse keeps the focused thread beneath the closed group
// across thread-screen navigation, project collapse, and reload.
import { test, expect, type SeedResult } from './fixtures.js';

test('a collapsed group retains its focused member through compact navigation', async ({ harness, page }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'compact-groups', repo: {},
      threads: ['Alpha', 'Beta', 'Outside'].map(title => ({
        title, turns: [{ userText: title, items: [{ kind: 'assistant_text', summary: 'done' }] }],
      })),
    }],
  });
  const { projectId, threadIds } = seed.projects[0];
  const group = await harness.rpc<{ id: string }>('CreateThreadGroup', projectId, 'Compact work');
  await harness.rpc('SetThreadGroup', threadIds.slice(0, 2), group.id);
  await harness.open(page);
  const groupRow = page.getByTestId('thread-group-row');
  const members = page.locator('[data-group-member] [data-sidebar-thread-id]');
  const alpha = page.locator(`[data-sidebar-thread-id="${threadIds[0]}"]`);
  const beta = page.locator(`[data-sidebar-thread-id="${threadIds[1]}"]`);

  await alpha.click();
  await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'thread');
  await page.getByTestId('compact-back').click();
  await groupRow.getByTestId('thread-group-row-expand').click();
  await expect(groupRow).toHaveAttribute('data-expanded', 'false');
  await expect(groupRow.getByTestId('thread-group-row-count')).toHaveText('2');
  await expect(members).toHaveCount(1);
  await expect(alpha).toBeVisible();
  await expect(beta).toHaveCount(0);

  // Collapsing the project and reopening it remounts the group list.
  await page.getByTestId('project-item-chevron').click();
  await expect(groupRow).toHaveCount(0);
  await expect(alpha).toBeVisible();
  await page.getByTestId('project-item-chevron').click();
  await expect(groupRow).toHaveAttribute('data-expanded', 'false');
  await expect(alpha).toBeVisible();
  await expect(members).toHaveCount(1);

  await alpha.click();
  await expect(page.getByTestId('chat-header-title')).toHaveText('Alpha');
  await page.getByTestId('compact-back').click();
  await expect(groupRow).toHaveAttribute('data-expanded', 'false');

  await page.locator(`[data-sidebar-thread-id="${threadIds[2]}"]`).click();
  await page.getByTestId('compact-back').click();
  await expect(members).toHaveCount(0);
  await expect(groupRow).toHaveAttribute('data-expanded', 'false');

  await groupRow.getByTestId('thread-group-row-expand').click();
  await beta.click();
  await page.getByTestId('compact-back').click();
  await groupRow.getByTestId('thread-group-row-expand').click();
  await expect(members).toHaveCount(1);
  await expect(beta).toBeVisible();
  await page.reload();
  // Restore may reveal the focused thread screen; either way, the stored
  // collapse choice must survive returning to the list.
  await expect(page.getByTestId('chat-header-title')).toHaveText('Beta');
  if (await page.getByTestId('compact-back').isVisible()) {
    await page.getByTestId('compact-back').click();
  }
  await expect(groupRow).toHaveAttribute('data-expanded', 'false');
  await expect(members).toHaveCount(1);
  await expect(beta).toBeVisible();
});
