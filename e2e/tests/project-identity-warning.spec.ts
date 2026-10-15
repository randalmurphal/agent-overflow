// The sidebar's project identity warning appears only when verifying a
// GitHub or GitLab checkout failed. A git repository with no origin, or an
// origin on any other host, is just a folder to the app: no identity and no
// warning. A GitHub origin the forge verifies shows nothing; one the forge
// does not have shows the warning.
import { randomBytes } from 'node:crypto';

import { expect, test, type SeedResult } from './fixtures.js';
import { seedForge } from './forge-helpers.js';
import { harnessGit } from './worktree-removal-helpers.js';

test('only a failed GitHub/GitLab verification warns on the project row', async ({ harness, page }) => {
  const suffix = randomBytes(4).toString('hex');
  const names = { local: `local-${suffix}`, other: `bitbucket-${suffix}`, verified: `verified-${suffix}`, missing: `missing-${suffix}` };
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: Object.values(names).map((name) => ({
      name,
      repo: { commits: [{ message: 'init', files: { 'README.md': '# seed\n' } }] },
      threads: [{ title: `${name} thread`, turns: [{ userText: 'Hi.', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
    })),
  });
  const [, other, verified, missing] = seed.projects;
  await seedForge(harness, [{ forge: 'github', project: `acme/${names.verified}` }]);
  harnessGit(harness, other.path, 'remote', 'add', 'origin', 'https://bitbucket.org/acme/repo.git');
  harnessGit(harness, verified.path, 'remote', 'add', 'origin', `https://github.com/acme/${names.verified}.git`);
  harnessGit(harness, missing.path, 'remote', 'add', 'origin', `https://github.com/acme/${names.missing}.git`);
  for (const project of seed.projects) await harness.rpc('RefreshProjectIdentity', project.projectId);

  await harness.open(page);
  const warning = (name: string) => page.getByTestId('project-item')
    .filter({ has: page.getByTestId('project-item-label').getByText(name, { exact: true }) })
    .getByTestId('project-item-identity-error');
  await expect(warning(names.missing)).toBeVisible();
  await expect(page.getByTestId('project-item-identity-error')).toHaveCount(1);
  for (const name of [names.local, names.other, names.verified]) await expect(warning(name)).toHaveCount(0);
});
