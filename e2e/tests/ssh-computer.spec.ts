// Real setup RPCs, console process and remote pairing, with only the SSH
// transport replaced. AO_E2E_SSH_CONFIG opts into an isolated real sshd fixture
// defining gpu-test, its identity file and pinned known_hosts. Never use a real
// user's config; the default gate needs no daemon or SSH credentials.
import { test, expect } from '@playwright/test';
import { mkdtemp, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, delimiter, isAbsolute } from 'node:path';
import { fileURLToPath } from 'node:url';
import { launchHarness, type HarnessApp } from '../src/harness.js';

test('connects a headless computer through the desktop SSH setup dialog', async ({ page }) => {
  test.setTimeout(90_000);
  test.skip(process.platform === 'win32', 'The native Windows launcher runs its SSH client in WSL.');
  const fixture = await mkdtemp(join(tmpdir(), 'ao-ssh-console-'));
  let remote: HarnessApp | undefined;
  let home: HarnessApp | undefined;
  try {
    remote = await launchHarness();
    await remote.rpc('HarnessSeed', { projects: [{ name: 'GPU project', repo: { commits: [{ files: { 'README.md': 'GPU project' } }] } }] });
    const binary = process.env.AO_HARNESS_BIN ?? fileURLToPath(new URL('../../bin/agent-overflow', import.meta.url));
    const sshConfig = process.env.AO_E2E_SSH_CONFIG;
    if (sshConfig && !isAbsolute(sshConfig)) throw new Error('The isolated SSH fixture needs an absolute config path');
    const script = `#!${process.execPath}
const { spawn } = require('node:child_process');
const args = process.argv.slice(2);
if (!args.includes('StrictHostKeyChecking=yes') || !args.includes('BatchMode=yes') || args.at(-2) !== 'gpu-test') process.exit(9);
if (!args.at(-1).includes(' pair --json --class desktop --wait 30s')) process.exit(10);
const child = ${sshConfig
  ? `spawn('/usr/bin/ssh', ['-F', ${JSON.stringify(sshConfig)}, ...args], { stdio: 'inherit' })`
  : `spawn(${JSON.stringify(binary)}, ['pair', '--json', '--class', 'desktop', '--config-root', ${JSON.stringify(remote.bootstrap.dataDir)}], { stdio: 'inherit' })`};
child.on('error', () => process.exit(11));
child.on('exit', (code) => process.exit(code ?? 1));
`;
    await writeFile(join(fixture, 'ssh'), script, { mode: 0o700 });
    // Real SSH exercises the closed command set and shell quoting too. The
    // remote executable has spaces and still pins the CLI to the owned host.
    const remoteExecutable = join(fixture, 'remote agent overflow');
    if (sshConfig) await writeFile(remoteExecutable, `#!${process.execPath}
const { spawn } = require('node:child_process');
const child = spawn(${JSON.stringify(binary)}, [...process.argv.slice(2), '--config-root', ${JSON.stringify(remote.bootstrap.dataDir)}], { stdio: 'inherit' });
child.on('error', () => process.exit(11));
child.on('exit', (code) => process.exit(code ?? 1));
`, { mode: 0o700 });
    home = await launchHarness({ env: { PATH: fixture + delimiter + process.env.PATH } });
    await home.open(page);
    await page.getByRole('button', { name: 'Settings', exact: true }).click();
    await page.getByRole('tab', { name: 'Connections', exact: true }).click();
    await page.getByRole('button', { name: 'Connect over SSH…', exact: true }).click();
    const dialog = page.getByRole('dialog', { name: 'Connect over SSH', exact: true });
    await dialog.getByLabel('SSH host', { exact: true }).fill('gpu-test');
    if (sshConfig) {
      await dialog.getByText('Startup & network', { exact: true }).click();
      await dialog.getByLabel('Remote executable', { exact: true }).fill(remoteExecutable);
    }
    await dialog.getByRole('button', { name: 'Continue', exact: true }).click();
    await expect(dialog.getByLabel('SSH verification number')).toHaveText(/\d{6}/);
    await dialog.getByRole('button', { name: 'Connect this computer', exact: true }).click();
    await expect(dialog).toContainText('Connected to gpu-test');
    await dialog.getByRole('button', { name: 'Done', exact: true }).click();
    await expect(page.getByTestId('attached-system')).toContainText('Connected');
    await page.getByRole('button', { name: 'Close Settings', exact: true }).click();
    await expect(page.getByText('GPU project', { exact: true })).toBeVisible();
    expect(await remote.rpc('ListProjects')).toBeTruthy();
  } finally {
    await home?.close();
    await remote?.close();
    await rm(fixture, { recursive: true, force: true });
  }
});
