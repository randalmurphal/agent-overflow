// The Commit dialog on the compact layout, against real git in the seeded
// repository: the chat header's thread actions menu reaches Commit through
// "Git actions…"; the file list sits above the preview and both fit the
// screen's width; a tapped row is previewed and a tapped checkbox selects
// it; the keyboard hints are not shown; Commit, reachable in the dialog's
// footer, commits the selected file only and leaves the dialog open on
// the other.
import { writeFile } from 'node:fs/promises';
import path from 'node:path';

import { expect, test, type SeedResult } from './fixtures.js';
import { errorToasts, harnessGit } from './worktree-removal-helpers.js';

test('the Commit dialog stacks the list over the preview and commits a tapped selection', async ({ harness, page }) => {
  const title = 'compact-commit thread';
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'compact-commit',
      repo: { commits: [{ message: 'init', files: { 'notes.md': 'base\n' } }] },
      threads: [{ title, provider: 'claude', turns: [{ userText: 'set the stage', items: [{ kind: 'assistant_text', summary: 'Ready.' }] }] }],
    }],
  });
  const root = seed.projects[0].path;
  await writeFile(path.join(root, 'notes.md'), 'base\nedited in notes\n');
  await writeFile(path.join(root, 'second.md'), 'a second file\n');
  const startHead = harnessGit(harness, root, 'rev-parse', 'HEAD');

  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: title }).click();
  await expect(page.getByLabel('Message Input')).toBeVisible();

  // Commit is enabled once git status reports the changes.
  await expect(async () => {
    await page.getByTestId('chat-header-more').click();
    await page.getByRole('menuitem', { name: 'Git actions…' }).click();
    const commit = page.getByRole('menu', { name: 'Git actions' }).getByRole('menuitem', { name: 'Commit', exact: true });
    try {
      await expect(commit).toBeEnabled({ timeout: 1_000 });
    } catch (err) {
      await page.keyboard.press('Escape');
      throw err;
    }
    await commit.click();
  }).toPass({ timeout: 30_000 });

  const dialog = page.getByRole('dialog', { name: 'Commit Changes' });
  const list = dialog.getByRole('listbox', { name: 'Files to commit' });
  const preview = dialog.getByTestId('commit-file-preview');
  const row = (name: string) => dialog.getByRole('option', { name, exact: true });
  await expect(dialog.getByRole('option')).toHaveCount(2);
  await expect(dialog.getByTestId('commit-dialog-selection')).toHaveText('0 of 2 files selected');
  await expect(dialog.getByText('ctrl enter')).toBeHidden();

  // One column: the preview starts below the list, and neither is wider
  // than the screen.
  const viewport = page.viewportSize()!;
  const listBox = (await list.boundingBox())!;
  const previewBox = (await preview.boundingBox())!;
  expect(previewBox.y).toBeGreaterThanOrEqual(listBox.y + listBox.height);
  for (const box of [listBox, previewBox]) {
    expect(box.x).toBeGreaterThanOrEqual(0);
    expect(box.x + box.width).toBeLessThanOrEqual(viewport.width);
    expect(box.height).toBeGreaterThan(100);
  }

  // A tap on a row previews it without selecting it; a tap on its
  // checkbox selects it.
  await row('second.md').tap();
  await expect(preview).toContainText('+a second file');
  await expect(row('second.md')).toHaveAttribute('aria-selected', 'false');
  await row('second.md').getByTestId('commit-file-check').tap();
  await expect(row('second.md')).toHaveAttribute('aria-selected', 'true');
  await expect(dialog.getByTestId('commit-dialog-selection')).toHaveText('1 of 2 files selected');

  await dialog.getByLabel('Subject').fill('Commit the second file');
  await dialog.getByRole('button', { name: 'Commit', exact: true }).tap();

  await expect(dialog.getByRole('option')).toHaveCount(1);
  await expect(row('notes.md')).toHaveAttribute('aria-selected', 'false');
  expect(harnessGit(harness, root, 'rev-parse', 'HEAD~1')).toBe(startHead);
  expect(harnessGit(harness, root, 'show', '--format=', '--name-status', 'HEAD')).toBe('A\tsecond.md');
  expect(harnessGit(harness, root, 'status', '--porcelain')).toBe('M notes.md');
  await expect(errorToasts(page)).toHaveCount(0);
});
