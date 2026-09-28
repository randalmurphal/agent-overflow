// The Create PR/MR dialog end to end, shared by the desktop (GitHub) and
// compact (GitLab) projects: the git actions menu opens a dialog with
// nothing prefilled whose submit stays disabled until a title is typed;
// submitting runs the forge CLI with the typed title, description and draft
// flag, shows the created URL in a success toast, and the forge's new PR/MR
// then turns the menu item into Open PR/MR. A forge refusal (an open PR/MR
// already exists for the branch) shows inline and leaves the dialog open
// with the typed title. The branch is published to a local origin that
// answers as the forge (`publishBranch`); nothing leaves loopback.
import { writeFile } from 'node:fs/promises';
import path from 'node:path';
import type { Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import { expectEveryForgeCallHandled, forgeInvocations, publishBranch, seedForge, type ForgeRepo } from './forge-helpers.js';
import { errorToasts, harnessGit } from './worktree-removal-helpers.js';

export interface CreatePRSurface {
  forge: 'github' | 'gitlab';
  /** Opens the git actions menu on the open thread. */
  openGitMenu(page: Page): Promise<void>;
  /** Opens the dialog some other way than the menu, or null for none. */
  openDialogElsewhere: ((page: Page) => Promise<void>) | null;
}

const BRANCH = 'feature-pr';

const labels = {
  github: { noun: 'PR', create: 'Create PR', open: 'Open PR', dialog: 'Create Pull Request' },
  gitlab: { noun: 'MR', create: 'Create MR', open: 'Open MR', dialog: 'Create Merge Request' },
} as const;

/** The argv the app hands the forge CLI for a create. */
function createArgv(forge: 'github' | 'gitlab', title: string, body: string, draft: boolean): string[] {
  if (forge === 'github') return ['pr', 'create', '--title', title, '--body', body, ...(draft ? ['--draft'] : [])];
  return ['mr', 'create', '--title', title, '--description', body, '--yes', '--no-editor', ...(draft ? ['--draft'] : [])];
}

function createdURL(repo: ForgeRepo, number: number): string {
  return repo.forge === 'github'
    ? `https://github.com/${repo.project}/pull/${number}`
    : `https://gitlab.com/${repo.project}/-/merge_requests/${number}`;
}

/** A thread on a project whose checked-out branch is published with no PR/MR. */
async function seedPublishedBranch(harness: HarnessApp, forge: 'github' | 'gitlab', name: string): Promise<ForgeRepo> {
  const title = `${name} thread`;
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name,
      repo: { commits: [{ message: 'init', files: { 'README.md': '# seed\n' } }] },
      threads: [{ title, provider: 'claude', turns: [{ userText: 'set the stage', items: [{ kind: 'assistant_text', summary: 'Ready.' }] }] }],
    }],
  });
  const root = seed.projects[0].path;
  harnessGit(harness, root, 'checkout', '--quiet', '-b', BRANCH);
  await writeFile(path.join(root, 'feature.md'), 'Feature work.\n');
  harnessGit(harness, root, 'add', 'feature.md');
  harnessGit(harness, root, 'commit', '--quiet', '-m', 'feature work');
  const repo: ForgeRepo = { forge, project: `acme/${name}` };
  await publishBranch(harness, root, repo, BRANCH);
  return repo;
}

async function openThread(page: Page, name: string): Promise<void> {
  await page.getByTestId('thread-row').filter({ hasText: `${name} thread` }).click();
  await expect(page.getByLabel('Message Input')).toBeVisible();
}

function dialog(page: Page, surface: CreatePRSurface) {
  return page.getByRole('dialog', { name: labels[surface.forge].dialog });
}

async function openFromMenu(page: Page, surface: CreatePRSurface): Promise<void> {
  await surface.openGitMenu(page);
  const item = page.getByRole('menu', { name: 'Git actions' }).getByRole('menuitem', { name: labels[surface.forge].create });
  // Enabled once git status has read the upstream and found no open PR/MR.
  await expect(item).toBeEnabled({ timeout: 30_000 });
  await item.click();
}

async function createCalls(harness: HarnessApp) {
  return (await forgeInvocations(harness)).filter((call) => call.args[1] === 'create');
}

export function createPRFlow(surface: CreatePRSurface): void {
  const { forge } = surface;
  const label = labels[forge];

  test(`${label.create} from the git actions menu creates a draft ${label.noun} with the typed title and description`, async ({
    harness,
    page,
  }) => {
    const name = `create-${forge}-ok`;
    const repo = await seedPublishedBranch(harness, forge, name);
    await harness.open(page);
    await openThread(page, name);

    await openFromMenu(page, surface);
    const form = dialog(page, surface);
    await expect(form).toBeVisible();
    const titleInput = form.getByLabel('Title');
    const bodyInput = form.getByLabel('Description (optional)');
    const draft = form.getByLabel('Open as draft');
    const submit = form.getByTestId('create-pr-submit');
    // Nothing is prefilled, and there is nothing to submit without a title.
    await expect(titleInput).toHaveValue('');
    await expect(bodyInput).toHaveValue('');
    await expect(draft).not.toBeChecked();
    await expect(submit).toBeDisabled();
    await titleInput.fill('   ');
    await expect(submit).toBeDisabled();

    await titleInput.fill('Add the feature');
    await expect(submit).toBeEnabled();
    await bodyInput.fill('Adds feature.md.\n\nReviewed locally.');
    await draft.check();
    await submit.click();

    const url = createdURL(repo, 1);
    await expect(page.getByRole('alert').filter({ hasText: `${label.noun} created: ${url}` })).toBeVisible();
    await expect(form).toHaveCount(0);

    const [call, ...more] = await createCalls(harness);
    expect(more, 'the dialog created more than once').toEqual([]);
    expect(call.args).toEqual(createArgv(forge, 'Add the feature', 'Adds feature.md.\n\nReviewed locally.', true));
    expect(call.exitCode).toBe(0);

    // The forge now has the PR/MR, and the menu offers to open it.
    await surface.openGitMenu(page);
    await expect(page.getByRole('menu', { name: 'Git actions' }).getByRole('menuitem', { name: label.open })).toBeVisible({ timeout: 30_000 });
    await page.keyboard.press('Escape');
    await expectEveryForgeCallHandled(harness);
    await expect(errorToasts(page)).toHaveCount(0);
  });

  test(`a ${label.noun} the forge refuses shows its error in the dialog, which stays open`, async ({ harness, page }) => {
    const name = `create-${forge}-refused`;
    const repo = await seedPublishedBranch(harness, forge, name);
    await harness.open(page);
    await openThread(page, name);

    if (surface.openDialogElsewhere) await surface.openDialogElsewhere(page);
    else await openFromMenu(page, surface);
    const form = dialog(page, surface);
    await expect(form).toBeVisible();

    // Someone opens a PR/MR for the branch while the dialog is up.
    await seedForge(harness, [{ ...repo, pulls: [{ number: 41, title: 'Opened elsewhere', headRef: BRANCH }] }]);

    await form.getByLabel('Title').fill('Second attempt');
    await form.getByTestId('create-pr-submit').click();
    await expect(form.getByRole('alert')).toContainText(forge === 'github' ? 'already exists' : 'Another open merge request already exists');
    await expect(form).toBeVisible();
    await expect(form.getByLabel('Title')).toHaveValue('Second attempt');

    const [call] = await createCalls(harness);
    expect(call.args).toEqual(createArgv(forge, 'Second attempt', '', false));
    expect(call.exitCode).toBe(1);
    await expect(page.getByRole('alert').filter({ hasText: `${label.noun} created` })).toHaveCount(0);
    await expectEveryForgeCallHandled(harness);
  });
}
