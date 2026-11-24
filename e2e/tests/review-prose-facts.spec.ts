// What a PR/MR body names that only the forge or the repository can answer,
// through the shipped UI, the real backend and the fake forge. Covered: an
// issue, a merge request and a mention the forge knows link with their
// titles, and ones it does not know are text; GitLab labels are chips in
// their colors, a milestone links, and a label that does not exist is text;
// an image that names a file of the PR's repository is read from the local
// git objects at the PR head (a Git LFS pointer says so and links to the
// forge, a path the head lacks is the unavailable chip); a GitHub permalink
// to lines of the repository at the head commit shows those lines, numbered,
// between the sentence's two halves. The pane asks the forge once for a
// PR's references, GitLab's labels by one read of the project's label list
// beside them and none by title, and a later poll that changes nothing
// asks nothing. The worker backend polls the PR every second
// (pr-update=1s) instead of every 45 s, which changes when a poll runs and
// not what it does.
// Unit and component coverage: forgeFacts.svelte.test.ts,
// forgeFactReferences.test.ts, forgeRepositoryFiles.test.ts,
// ChatMarkdown.forgeFacts.browser.test.ts,
// ChatMarkdown.repositoryFiles.browser.test.ts (geometry),
// internal/git/forge_facts_test.go, internal/git/local_facts_test.go.
import { randomBytes } from 'node:crypto';

import { HARNESS_WORKER_TIMEOUT_MS, expect, test as base } from './fixtures.js';
import { harnessTiming, launchHarness, type HarnessApp } from '../src/harness.js';
import {
  expandReviewSection,
  expectEveryForgeCallHandled,
  forgeInvocations,
  openPullRequestReview,
  publishPullRequest,
  seedForge,
  seedThreadProject,
  solidPNG,
  type ForgeAccount,
  type ForgeHTTPInvocation,
  type ForgeRepo,
  type PullHistory,
} from './forge-helpers.js';

const test = base.extend<object, { harnessWorker: HarnessApp }>({
  harnessWorker: [
    async ({}, use) => {
      const app = await launchHarness({ env: harnessTiming({ 'pr-update': '1s' }) });
      await use(app);
      await app.close();
    },
    { scope: 'worker', timeout: HARNESS_WORKER_TIMEOUT_MS },
  ],
});

const TITLE = 'Prose facts';
const LFS_POINTER = 'version https://git-lfs.github.com/spec/v1\noid sha256:4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393\nsize 12345\n';
const APP_TS = Array.from({ length: 8 }, (_, i) => `export const line${i + 1} = ${i + 1};`).join('\n') + '\n';

function history(): PullHistory {
  return {
    commits: [{
      message: 'add docs',
      files: { 'docs/shot.png': solidPNG(320, 180, [40, 160, 200]), 'docs/lfs.png': LFS_POINTER, 'src/app.ts': APP_TS },
    }],
  };
}

/** The fake's answered facts requests, oldest first. */
async function factsCalls(harness: Parameters<typeof forgeInvocations>[0]): Promise<ForgeHTTPInvocation[]> {
  return (await forgeInvocations(harness))
    .filter((call): call is ForgeHTTPInvocation => call.via === 'http' && /graphql ForgeFacts(\d+|Labels)$/.test(String(call.route)) && call.status < 300);
}

/** Wait for the pane's next read of the PR after `route`'s current count. */
async function nextPoll(harness: Parameters<typeof forgeInvocations>[0], route: string): Promise<void> {
  const count = async () => (await forgeInvocations(harness)).filter((call) => call.via === 'http' && call.route === route).length;
  const from = await count();
  await expect.poll(count, { timeout: 90_000, intervals: [1_000] }).toBeGreaterThan(from);
}

test('GitHub: references resolve, repository images and permalink lines come from local git', async ({ harness, page }) => {
  test.setTimeout(180_000);
  const workspace = await seedThreadProject(harness, TITLE, 'prose-facts');
  const project = `ao-e2e/prose-facts-${randomBytes(4).toString('hex')}`;
  const repo: ForgeRepo = {
    forge: 'github',
    project,
    issues: [{ number: 12, title: 'Crash on save', state: 'closed' }],
    pulls: [{ number: 41, title: 'Docs', body: '' }],
  };
  const accounts: ForgeAccount[] = [{ forge: 'github', login: 'octo', name: 'Octo Cat' }];
  const published = await publishPullRequest(harness, workspace, repo, history());
  const head = published.pulls![0]!.headSha!;
  const body = [
    'Fixes #12 and #404, thanks @octo and @ghost-nobody.',
    '',
    '![shot](docs/shot.png)',
    '',
    '![lfs](docs/lfs.png)',
    '',
    '![gone](docs/missing.png)',
    '',
    `See https://github.com/${project}/blob/${head}/src/app.ts#L2-L4 for the lines.`,
    '',
    'After the snippet.',
  ].join('\n');
  await seedForge(harness, [{ ...published, pulls: [{ ...published.pulls![0]!, body }] }], accounts);

  await harness.open(page);
  await openPullRequestReview(page, TITLE);
  const description = await expandReviewSection(page, 'review-pr-description');
  const prose = description.locator('.markdown-body');

  // The forge's answers: a found reference links with its title, one the
  // forge does not have is text.
  const issue = prose.getByRole('link', { name: '#12', exact: true });
  await expect(issue).toHaveAttribute('title', /Crash on save/, { timeout: 30_000 });
  await expect(issue).toHaveAttribute('href', `https://github.com/${project}/issues/12`);
  await expect(prose.getByRole('link', { name: '#404' })).toHaveCount(0);
  await expect(prose).toContainText('#404');
  await expect(prose.getByRole('link', { name: '@octo', exact: true })).toHaveAttribute('title', /Octo Cat/);
  await expect(prose.getByRole('link', { name: '@ghost-nobody' })).toHaveCount(0);

  // The repository image, from the local objects at the head: its pixels,
  // in a box of its own size.
  const shot = prose.locator('img[alt="shot"]');
  await expect(shot).toBeVisible({ timeout: 30_000 });
  await expect.poll(() => shot.evaluate((img: HTMLImageElement) => img.naturalWidth)).toBe(320);
  const box = (await shot.boundingBox())!;
  expect(Math.round(box.width)).toBe(320);
  expect(Math.round(box.height)).toBe(180);

  // An LFS pointer says so and links to the file on the forge; a path the
  // head lacks is the unavailable chip.
  const lfs = prose.locator('[data-repository-image="lfs"]');
  await expect(lfs).toContainText('stored in Git LFS');
  await expect(prose.getByRole('link', { name: 'open on GitHub' })).toHaveAttribute('href', `https://github.com/${project}/blob/${head}/docs/lfs.png`);
  await expect(prose.locator('[data-repository-image="absent"]')).toContainText('gone');

  // The permalink's lines, numbered from the first, between the sentence's halves.
  const snippet = prose.locator('[data-streamdown-code-snippet]');
  await expect(snippet).toHaveCount(1);
  await expect(snippet).toContainText('src/app.ts');
  await expect(snippet).toContainText('Lines 2 to 4 in');
  await expect(snippet).toContainText(head.slice(0, 7));
  await expect(snippet).toContainText('export const line2 = 2;');
  await expect(snippet).toContainText('export const line4 = 4;');
  await expect(snippet).not.toContainText('export const line1 = 1;');
  await expect(snippet).not.toContainText('export const line5 = 5;');
  await expect(snippet.locator('.md-code-line-numbers')).toHaveText(/^\s*2\s*3\s*4\s*$/);
  const order = await prose.evaluate((root) => {
    const text = root.textContent ?? '';
    return [text.indexOf('See'), text.indexOf('Lines 2 to 4'), text.indexOf('for the lines.'), text.indexOf('After the snippet.')];
  });
  expect(order.every((at) => at >= 0)).toBe(true);
  expect([...order].sort((a, b) => a - b)).toEqual(order);

  // One request for the PR's references; a poll that changes nothing asks none.
  expect((await factsCalls(harness)).length).toBe(1);
  await nextPoll(harness, 'gh graphql PRTick');
  expect((await factsCalls(harness)).length).toBe(1);
  await expectEveryForgeCallHandled(harness);
});

test('GitLab: labels are chips in their colors; issues, merge requests, milestones and users resolve', async ({ harness, page }) => {
  test.setTimeout(180_000);
  const workspace = await seedThreadProject(harness, TITLE, 'prose-facts');
  const project = `ao-e2e/prose-facts-${randomBytes(4).toString('hex')}`;
  const body = [
    'Fixes #12 and #404, see !7 and !99, cc @octo.',
    '',
    'Labels ~bug ~"type::feature" ~nope for %17.0.',
    '',
    '![shot](docs/shot.png)',
    '',
    'After the image.',
  ].join('\n');
  const published = await publishPullRequest(harness, workspace, {
    forge: 'gitlab',
    project,
    issues: [{ number: 12, title: 'Crash on save' }],
    labels: [{ name: 'bug', color: '#d9534f' }, { name: 'type::feature', color: '#1f75cb' }],
    milestones: [{ title: '17.0' }],
    pulls: [{ number: 7, title: 'Docs MR', body }],
  }, history());
  await seedForge(harness, [published], [{ forge: 'gitlab', login: 'octo', name: 'Octo Cat' }]);

  await harness.open(page);
  await openPullRequestReview(page, TITLE);
  const description = await expandReviewSection(page, 'review-pr-description');
  const prose = description.locator('.markdown-body');

  const issue = prose.getByRole('link', { name: '#12', exact: true });
  await expect(issue).toHaveAttribute('title', /Crash on save/, { timeout: 30_000 });
  await expect(prose.getByRole('link', { name: '#404' })).toHaveCount(0);
  await expect(prose.getByRole('link', { name: '!7', exact: true })).toHaveAttribute('title', /Docs MR/);
  await expect(prose.getByRole('link', { name: '!99' })).toHaveCount(0);
  await expect(prose.getByRole('link', { name: '@octo', exact: true })).toHaveAttribute('title', /Octo Cat/);
  await expect(prose.getByRole('link', { name: '%17.0', exact: true })).toHaveAttribute('href', new RegExp(`/${project}/-/milestones/\\d+$`));

  // The labels: a chip on the label's color, a scoped one in two parts,
  // and a title no label has as the text it was written as.
  const chips = prose.locator('.md-label-chip');
  await expect(chips).toHaveCount(2);
  await expect(chips.nth(0)).toHaveText('bug');
  await expect.poll(() => chips.nth(0).locator('.md-label-chip-key').evaluate((el) => getComputedStyle(el).backgroundColor)).toBe('rgb(217, 83, 79)');
  await expect(chips.nth(1).locator('.md-label-chip-key')).toHaveText('type');
  await expect(chips.nth(1).locator('.md-label-chip-value')).toHaveText('feature');
  await expect.poll(() => chips.nth(1).locator('.md-label-chip-key').evaluate((el) => getComputedStyle(el).backgroundColor)).toBe('rgb(31, 117, 203)');
  await expect(prose).toContainText('~nope');

  const shot = prose.locator('img[alt="shot"]');
  await expect(shot).toBeVisible({ timeout: 30_000 });
  await expect.poll(() => shot.evaluate((img: HTMLImageElement) => img.naturalWidth)).toBe(320);

  // One read of the references and one of the label list, which decides
  // every label: no label is asked for by title.
  const calls = await factsCalls(harness);
  expect(calls.map((call) => call.route).sort()).toEqual(['glab graphql ForgeFacts16', 'glab graphql ForgeFactsLabels']);
  const slots = calls.find((call) => call.route === 'glab graphql ForgeFacts16')!;
  expect(Object.entries(slots.variables ?? {}).filter(([name, on]) => /^l\d+$/.test(name) && on === true)).toEqual([]);
  expect(calls.find((call) => call.route === 'glab graphql ForgeFactsLabels')!.variables).toEqual({ p: project, after: null });
  await nextPoll(harness, 'glab api merge request');
  expect((await factsCalls(harness)).length).toBe(2);

  // The user's reload asks the forge again, inside the time the computer
  // would answer from its cache: a label recolored since shows its color.
  await seedForge(harness, [{ ...published, labels: [{ name: 'bug', color: '#00aa55' }, { name: 'type::feature', color: '#1f75cb' }] }]);
  await page.getByTestId('review-reload').click();
  await expect.poll(() => chips.nth(0).locator('.md-label-chip-key').evaluate((el) => getComputedStyle(el).backgroundColor), { timeout: 30_000 }).toBe('rgb(0, 170, 85)');
  expect((await factsCalls(harness)).filter((call) => call.route === 'glab graphql ForgeFactsLabels').length).toBe(2);
  await expectEveryForgeCallHandled(harness);
});
