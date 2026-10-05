// Create PR dialog on the desktop against the fake `gh`: opened from the
// chat header's git actions menu, and from the command palette's
// `git.openPR` command ("Git: Open Pull/Merge Request"). Coverage is
// create-pr-flow.ts's header.
import type { Page } from '@playwright/test';
import { expect } from './fixtures.js';
import { createPRFlow } from './create-pr-flow.js';

async function openFromPalette(page: Page): Promise<void> {
  await page.keyboard.press('ControlOrMeta+Shift+K');
  const input = page.getByTestId('command-palette-input');
  await expect(input).toBeVisible();
  await input.fill('Git: Open Pull/Merge Request');
  await page.getByRole('option', { name: /Git: Open Pull\/Merge Request/ }).click();
}

createPRFlow({
  forge: 'github',
  openGitMenu: (page) => page.getByRole('button', { name: 'More git actions' }).click(),
  openDialogElsewhere: openFromPalette,
});
