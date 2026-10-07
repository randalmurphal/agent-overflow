// The same repository checked out at different paths on two paired computers
// is one sidebar entry. Covers a project the remote computer creates through
// session import, and the machine picker's folder flow, which refuses a
// checkout of another repository and adopts a checkout of the same one.
// The home checkouts have no origin and the remote clones do, so the entries
// merge on the root commit. Two harness backends, real pairing and the
// production frontend; git runs with each harness home.
import { expect, test } from '@playwright/test';
import { mkdir, writeFile } from 'node:fs/promises';
import * as path from 'node:path';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { headlessPairing } from './headless-pairing-helpers.js';
import { startTogether } from './launch-helpers.js';
import { harnessGit } from './worktree-removal-helpers.js';

interface Seed { projects: Array<{ projectId: string; path: string }> }
interface ProjectRow { project: { id: string; name: string; path: string; remoteURL?: string; rootCommit?: string } }
interface ImportScan { rows: Array<{ id: string; sessionId: string }> }
interface ImportProgress { importId: string; done?: boolean; error?: string; status?: string }

const IMPORT_SESSION = 'f1f1f1f1-6666-4666-8666-f1f1f1f1f1f1';
const IMPORT_PROMPT = 'Tidy the import checkout';

/** Clones home's repository into the remote's data root under another path. */
function cloneOnto(remote: HarnessApp, source: string, name: string, origin: string): string {
  const target = path.join(remote.bootstrap.dataRoot, 'workspaces', 'checkouts', name);
  harnessGit(remote, remote.bootstrap.dataRoot, 'clone', '--quiet', source, target);
  harnessGit(remote, target, 'remote', 'set-url', 'origin', origin);
  return target;
}

async function writeClaudeSession(remote: HarnessApp, workspace: string): Promise<void> {
  const home = remote.bootstrap.homeDir;
  if (!home) throw new Error('remote harness carries no homeDir');
  const dir = path.join(home, '.claude', 'projects', '-identity-merge');
  await mkdir(dir, { recursive: true });
  const at = (offset: number) => new Date(1_700_000_000_000 + offset).toISOString();
  const rows = [
    { type: 'user', uuid: 'im-u1', parentUuid: null, isSidechain: false, timestamp: at(0), cwd: workspace, gitBranch: 'main', message: { role: 'user', content: IMPORT_PROMPT } },
    { type: 'assistant', uuid: 'im-a1', parentUuid: 'im-u1', isSidechain: false, timestamp: at(1_000), cwd: workspace, message: { role: 'assistant', id: 'msg-im-1', model: 'claude-sonnet-4-5', content: [{ type: 'text', text: 'Done.' }], usage: { input_tokens: 10, output_tokens: 2 } } },
    { type: 'last-prompt', leafUuid: 'im-a1', lastPrompt: IMPORT_PROMPT },
  ];
  await writeFile(path.join(dir, `${IMPORT_SESSION}.jsonl`), rows.map((row) => JSON.stringify(row)).join('\n') + '\n');
}

test('one repository at different paths on two computers is one project', async ({ page }) => {
  test.setTimeout(120_000);
  let home: HarnessApp | undefined;
  let remote: HarnessApp | undefined;
  let pairing: Awaited<ReturnType<typeof headlessPairing>> | undefined;
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  try {
    [home, remote] = await startTogether(launchHarness(), launchHarness());
    const seeded = await home.rpc<Seed>('HarnessSeed', {
      projects: ['picker-app', 'import-app', 'other-app'].map((name) => ({
        name, repo: { commits: [{ message: `init ${name}`, files: { 'README.md': `${name}\n` } }] },
      })),
    });
    const [picker, imported, other] = seeded.projects;
    const pickerClone = cloneOnto(remote, picker.path, 'picker-app-checkout', 'https://github.com/me/picker-app');
    const importClone = cloneOnto(remote, imported.path, 'import-app-checkout', 'https://github.com/me/import-app');
    const otherClone = cloneOnto(remote, other.path, 'other-app-checkout', 'git@github.com:me/other-app.git');
    const draft = await home.rpc<{ id: string }>('CreateThread', { projectId: picker.projectId, title: 'Picker draft', provider: 'claude' });
    await home.rpc('SaveDraft', draft.id, 'Not sent yet', [], [], null);

    await home.open(page);
    await page.getByRole('button', { name: 'Settings', exact: true }).click();
    await page.getByRole('tab', { name: 'Connect to a computer', exact: true }).click();
    pairing = await headlessPairing(remote);
    await page.getByRole('textbox', { name: /^(Computer address or pairing link|Pairing link)$/ }).fill(pairing.invite.url);
    await page.getByRole('button', { name: 'Connect', exact: true }).click();
    const verification = page.getByLabel('Verification number');
    await expect(verification).toBeVisible();
    await pairing.confirm((await verification.textContent())!.trim());
    const computerRow = page.getByTestId('attached-system');
    await expect(computerRow).toContainText('Connected');
    const [{ id: remoteId }] = await home.rpc<Array<{ id: string }>>('ListBackends');
    await home.rpc('RenameBackend', remoteId, 'Desktop');
    await expect(computerRow).toContainText('Desktop');
    await page.getByRole('button', { name: 'Close Settings', exact: true }).click();
    const labels = page.getByTestId('project-item-label');

    // Session import on the remote creates the project for its checkout,
    // with identity, so it joins home's entry instead of adding one.
    await writeClaudeSession(remote, importClone);
    const scan = await remote.rpc<ImportScan>('ListImportableSessions', { forceRefresh: true });
    const row = scan.rows.find((candidate) => candidate.sessionId === IMPORT_SESSION);
    expect(row).toBeDefined();
    const run = await remote.rpc<{ importId: string }>('ImportSessions', { ids: [row!.id] });
    const done = await remote.waitForEvent<ImportProgress>('session-import:progress', (frame) => frame.importId === run.importId && frame.done === true);
    expect(done.error ?? '').toBe('');
    const remoteImported = (await remote.rpc<ProjectRow[]>('ListProjects')).find((entry) => entry.project.name === 'import-app-checkout');
    expect(remoteImported?.project).toMatchObject({ remoteURL: 'https://github.com/me/import-app', rootCommit: expect.stringMatching(/^[0-9a-f]{40}$/) });
    await expect(page.getByTestId('thread-row').filter({ hasText: IMPORT_PROMPT })).toBeVisible();
    await expect(labels.filter({ hasText: /^import-app$/ })).toHaveCount(1);
    await expect(labels.filter({ hasText: 'import-app-checkout' })).toHaveCount(0);

    // The machine picker asks for the folder on a computer with no checkout,
    // refuses another repository, and adopts the same one.
    await page.getByTestId('thread-row-title').filter({ hasText: /^Picker draft$/ }).click();
    await page.getByTestId('machine-picker-trigger').click();
    await page.getByRole('menuitem', { name: /^Desktop/ }).click();
    const modal = page.getByRole('dialog', { name: 'Choose picker-app on Desktop', exact: true });
    await expect(modal).toBeVisible();
    await expect(modal.getByRole('combobox', { name: 'Computer', exact: true })).toBeDisabled();
    const pathInput = page.getByTestId('directory-browser-path');
    const add = page.getByTestId('add-project-submit');

    await pathInput.fill(otherClone);
    await expect(add).toBeEnabled();
    await add.click();
    await expect(page.getByTestId('add-project-error')).toHaveText('That folder is a checkout of github.com/me/other-app, not picker-app.');
    await expect(modal).toBeVisible();
    expect((await remote.rpc<ProjectRow[]>('ListProjects')).some((entry) => entry.project.path === otherClone)).toBe(false);

    await pathInput.fill(pickerClone);
    await expect(add).toBeEnabled();
    await add.click();
    await expect(modal).not.toBeVisible();
    await expect(page.getByTestId('machine-picker-trigger')).toContainText('Desktop');
    const remoteProjects = await remote.rpc<ProjectRow[]>('ListProjects');
    expect(remoteProjects.find((entry) => entry.project.name === 'picker-app-checkout')?.project.remoteURL).toBe('https://github.com/me/picker-app');
    await expect(labels.filter({ hasText: /^picker-app$/ })).toHaveCount(1);
    await expect(labels.filter({ hasText: 'picker-app-checkout' })).toHaveCount(0);
    await expect(labels.filter({ hasText: 'other-app' })).toHaveCount(1);
    expect(errors).toEqual([]);
  } finally {
    try {
      if (!page.isClosed()) await page.goto('about:blank');
    } finally {
      try { pairing?.close(); } finally {
        try { await remote?.stop(); } finally { await home?.stop(); }
      }
    }
  }
});
