// Error reports on a real backend failure, on the loopback desktop page:
// - A failed Push (no git remote) shows its pane banner row with a details
//   toggle and a copy action. Details name the failed method, its log
//   reference and the backend's error.
// - Copy puts a report on the clipboard with the method, the reference, the
//   error and the backend log lines ending at that reference's line.
import type { Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import { harnessGit, openThread } from './worktree-removal-helpers.js';

const NO_REMOTE = 'cannot push because no git remote is configured';

async function seedThread(harness: HarnessApp, name: string): Promise<{ root: string; title: string }> {
  const title = `${name} thread`;
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name,
      repo: { commits: [{ message: 'init', files: { 'notes.md': 'base\n' } }] },
      threads: [{ title, provider: 'claude', turns: [{ userText: 'set the stage', items: [{ kind: 'assistant_text', summary: 'Ready.' }] }] }],
    }],
  });
  return { root: seed.projects[0].path, title };
}

async function pushFromGitMenu(page: Page): Promise<void> {
  const push = page.getByRole('menu', { name: 'Git actions' }).getByRole('menuitem', { name: 'Push', exact: true });
  await expect(async () => {
    await page.getByRole('button', { name: 'More git actions' }).click();
    try {
      await expect(push).toBeEnabled({ timeout: 1_000 });
    } catch (err) {
      await page.keyboard.press('Escape');
      throw err;
    }
  }).toPass({ timeout: 30_000 });
  await push.click();
}

test('a failed Push explains itself and copies a report with the backend log', async ({ harness, page }) => {
  const { root, title } = await seedThread(harness, 'error-report-push');
  harnessGit(harness, root, 'checkout', '--quiet', '-b', 'topic');
  harnessGit(harness, root, 'commit', '--quiet', '--allow-empty', '-m', 'topic work');
  expect(harnessGit(harness, root, 'remote')).toBe('');

  await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], {
    origin: new URL(harness.url).origin,
  });
  await harness.open(page);
  await openThread(page, title);
  await pushFromGitMenu(page);

  const row = page.getByTestId('pane-error-banner').filter({ hasText: 'Push failed' });
  await expect(row).toBeVisible();
  await expect(row.getByTestId('error-details')).toHaveCount(0);

  await row.getByTestId('error-details-toggle').click();
  const details = row.getByTestId('error-details');
  await expect(details).toContainText('GitPush');
  await expect(details).toContainText(NO_REMOTE);
  const ref = (await details.textContent())?.match(/ref (\S+)/)?.[1];
  expect(ref, 'details name the log reference').toBeTruthy();

  // The log lines are read when the error is captured; a copy taken before
  // that read lands says so, so retry until the report carries them.
  await expect(async () => {
    await row.getByRole('button', { name: 'Copy error for an agent' }).click();
    const report = await page.evaluate(() => navigator.clipboard.readText());
    expect(report).toContain('Agent Overflow error: Push failed');
    expect(report).toContain('- method: GitPush');
    expect(report).toContain(`- ref: ${ref}`);
    expect(report).toContain(NO_REMOTE);
    const log = report.split('Backend log leading up to it:\n```\n')[1]?.split('\n```')[0]?.split('\n') ?? [];
    expect(log.at(-1)).toContain(`(id: ${ref})`);
    expect(log.at(-1)).toContain(NO_REMOTE);
  }).toPass({ timeout: 10_000 });

  await row.getByRole('button', { name: 'Dismiss Banner' }).click();
  await expect(row).toHaveCount(0);
});
