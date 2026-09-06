// Two independently booted computers, real pairing/proxy/RPC and production
// frontend. Profiles and network identities outlive HarnessReset, so this
// flow owns both backends and all of its browser contexts.
import { expect, test, type Page } from '@playwright/test';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { headlessPairing } from './headless-pairing-helpers.js';
import { instrument } from './offhost-helpers.js';
import { runSessionCLI } from './session-cli-helpers.js';
import { randomUUID } from 'node:crypto';
import { unusedPort } from '../src/ports.js';
import { doneResult, humanGateWorkflow, seedWorkflow, setClaudeScenario, startWorkflow, waitForWorkflowState, type WorkflowDetail } from './workflows-helpers.js';

interface Seed { projects: Array<{ projectId: string; path: string; threadIds: string[] }> }
interface ProjectRow { project: { id: string; name: string; path: string } }

async function settingsPage(page: Page, name: string): Promise<void> {
  const close = page.getByRole('button', { name: 'Close Settings', exact: true });
  if (await close.isVisible()) await close.click();
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await page.getByRole('tab', { name, exact: true }).click();
}

export function connectedComputersFlow(): void {
  test('pairs computers, scopes configuration, registers a remote checkout, and retains offline catalogs', async ({ page, browser }) => {
    test.setTimeout(120_000);
    let home: HarnessApp | undefined;
    let remote: HarnessApp | undefined;
    let pairing: Awaited<ReturnType<typeof headlessPairing>> | undefined;
    const other = await browser.newContext();
    const errors: string[] = [];
    const surfaced = await instrument(page);
    page.on('pageerror', (error) => errors.push(error.message));
    try {
      home = await launchHarness();
      remote = await launchHarness();
      await home.rpc('UpdateSettings', { keepAwakeScreen: true });
      await remote.rpc('UpdateSettings', { keepAwakeScreen: true });
      const homeSeed = await home.rpc<Seed>('HarnessSeed', { projects: [{ name: 'Mac checkout', repo: { commits: [{ files: { 'README.md': 'Mac project' } }] }, threads: [{ title: 'Mac conversation', turns: [{ userText: 'Hello', items: [{ kind: 'assistant_text', summary: 'Done' }] }] }] }] });
      const remoteSeed = await remote.rpc<Seed>('HarnessSeed', { projects: [{ name: 'GPU checkout', repo: { commits: [{ files: { 'README.md': 'GPU project' } }] }, threads: [{ title: 'GPU conversation', turns: [{ userText: 'Hello', items: [{ kind: 'assistant_text', summary: 'Done' }] }] }] }] });
      const existing = await remote.rpc<Seed>('HarnessSeed', { projects: [{ name: 'already-cloned', repo: { commits: [{ files: { 'README.md': 'Unregistered project' } }] } }] });
      const checkout = existing.projects[0].path;
      // Deleting registration preserves the project checkout; simulate a
      // repo cloned outside AO, then add it through the remote picker.
      await remote.rpc('DeleteProject', existing.projects[0].projectId);
      await setClaudeScenario(remote, 'connected-workflow', [{ steps: [{ emit: { lines: [doneResult({ complete: true })] } }] }]);
      const workflowProject = await seedWorkflow(remote, 'Remote workflow', 'connected-flow', humanGateWorkflow('connected-flow'));
      const run = await startWorkflow(remote, workflowProject.projectId, 'connected-flow', 'Work on the GPU computer');
      await waitForWorkflowState(remote, run.id, 'needs-human', 'gate');
      const detail = await remote.rpc<WorkflowDetail>('WorkflowGetItem', run.id);
      const workflowThread = detail.phases.find((phase) => phase.phaseId === 'plan')?.threadId;
      expect(workflowThread).toBeTruthy();
      await home.open(page);
      const second = await other.newPage();
      await home.open(second);
      await expect(second.getByRole('button', { name: 'Settings', exact: true })).toBeVisible();
      const initialFont = await second.evaluate(() => getComputedStyle(document.documentElement).fontSize);

      await settingsPage(page, 'Connections');
      pairing = await headlessPairing(remote);
      const invite = pairing.invite;
      await page.getByRole('textbox', { name: 'Pairing link' }).fill(invite.url);
      await page.getByRole('button', { name: 'Connect', exact: true }).click();
      const verification = page.getByLabel('Verification number');
      await expect(verification).toBeVisible();
      await pairing.confirm((await verification.textContent())!.trim());
      const computerRow = page.getByTestId('attached-system');
      await expect(computerRow).toContainText('Connected');
      const systems = await home.rpc<Array<{ id: string }>>('ListBackends');
      expect(systems).toHaveLength(1);
      const remoteId = systems[0].id;
      await home.rpc('RenameBackend', remoteId, 'GPU workstation');
      await expect(computerRow).toContainText('GPU workstation');

      // Backend pairing alone does not allow model commands. Toggle the real
      // originating host's opt-in, then exercise the CLI with the credential
      // injected into its mocked provider session (never the page's token).
      await settingsPage(page, 'Agent access');
      const agentAccess = page.locator('section').filter({ has: page.getByRole('heading', { name: 'Agent access to other computers', exact: true }) });
      await expect(agentAccess.getByRole('button', { name: 'Enable', exact: true })).toBeVisible();
      await agentAccess.getByRole('button', { name: 'Enable', exact: true }).click();
      await expect(agentAccess.getByRole('button', { name: 'Enabled', exact: true })).toHaveAttribute('aria-pressed', 'true');
      const sourceThread = homeSeed.projects[0].threadIds[0];
      await home.rpc('StartSession', sourceThread);
      const listed = await runSessionCLI(home, sourceThread, ['remote', 'list']);
      expect(listed.code, listed.stderr).toBe(0);
      expect(JSON.parse(listed.stdout)).toEqual(expect.arrayContaining([expect.objectContaining({ id: remoteId })]));
      const commandID = randomUUID();
      const command = ['remote', 'run', '--computer', remoteId, '--project', remoteSeed.projects[0].projectId, '--id', commandID, '--', 'sh', '-c', 'printf peer-command-result'];
      const started = await runSessionCLI(home, sourceThread, command);
      expect(started.code, started.stderr).toBe(0);
      await expect.poll(async () => {
        const result = await runSessionCLI(home!, sourceThread, ['remote', 'status', '--computer', remoteId, commandID]);
        expect(result.code, result.stderr).toBe(0);
        return JSON.parse(result.stdout);
      }).toMatchObject({ state: 'succeeded', output: 'peer-command-result', sourceThreadId: sourceThread });
      // Opt-out hides discovery and blocks new work while preserving receipts.
      await agentAccess.getByRole('button', { name: 'Enabled', exact: true }).click();
      await expect(agentAccess.getByRole('button', { name: 'Enable', exact: true })).toHaveAttribute('aria-pressed', 'false');
      const disabled = await runSessionCLI(home, sourceThread, ['remote', 'list']);
      expect(disabled.code, disabled.stderr).toBe(0);
      expect(JSON.parse(disabled.stdout)).toEqual([]);
      const receipt = await runSessionCLI(home, sourceThread, ['remote', 'status', '--computer', remoteId, commandID]);
      expect(receipt.code, receipt.stderr).toBe(0);
      expect(JSON.parse(receipt.stdout)).toMatchObject({ state: 'succeeded', output: 'peer-command-result' });


      await settingsPage(page, 'Performance');
      await page.getByRole('combobox', { name: 'Computer', exact: true }).selectOption(remoteId);
      const screen = page.getByRole('switch', { name: 'Toggle Keep-Awake Screen' });
      await expect(screen).toBeEnabled();
      await expect(screen).toHaveAttribute('aria-checked', 'true');
      await screen.click();
      await expect.poll(async () => (await remote!.rpc<{ keepAwakeScreen: boolean }>('GetSettings')).keepAwakeScreen).toBe(false);
      expect((await home.rpc<{ keepAwakeScreen: boolean }>('GetSettings')).keepAwakeScreen).toBe(true);

      await settingsPage(page, 'Typography');
      const size = page.getByTestId('settings-font-size');
      await size.fill('17');
      await size.press('Tab');
      await expect.poll(() => page.evaluate(() => getComputedStyle(document.documentElement).fontSize)).not.toBe(initialFont);
      const changedFont = await page.evaluate(() => getComputedStyle(document.documentElement).fontSize);
      expect(await second.evaluate(() => getComputedStyle(document.documentElement).fontSize)).toBe(initialFont);
      await page.getByRole('button', { name: 'Close Settings', exact: true }).click();

      await page.getByTestId('sidebar-workflows-button').click();
      await page.getByTestId('workflows-pause-all').click();
      await expect.poll(async () => (await home!.rpc<{ paused: boolean }>('WorkflowGetEngineState')).paused).toBe(true);
      await expect.poll(async () => (await remote!.rpc<{ paused: boolean }>('WorkflowGetEngineState')).paused).toBe(true);
      await page.getByTestId('workflow-run-row').filter({ hasText: 'Work on the GPU computer' }).click();
      await page.locator('[data-testid="workflow-map-node"][data-phase-id="plan"]').getByTestId('workflow-map-node-label').click();
      await expect(page.locator(`[data-ui-surface="chat"][data-thread-id="${workflowThread}"]`)).toBeVisible();
      await expect(page.getByTestId('workflows-overlay')).toBeHidden();
      const back = page.getByTestId('compact-back');
      if (await back.isVisible()) await back.click();

      await page.getByRole('button', { name: 'Add Project', exact: true }).click();
      const modal = page.getByRole('dialog', { name: 'Add Project', exact: true });
      await modal.getByRole('combobox', { name: 'Computer', exact: true }).selectOption(remoteId);
      await page.getByTestId('directory-browser-path').fill(checkout);
      const add = page.getByTestId('add-project-submit');
      await expect(add).toBeEnabled();
      await add.click();
      await expect(modal).not.toBeVisible();
      const remoteProjects = await remote.rpc<ProjectRow[]>('ListProjects');
      expect(remoteProjects.some((row) => row.project.path === checkout)).toBe(true);
      expect((await home.rpc<ProjectRow[]>('ListProjects')).some((row) => row.project.path === checkout)).toBe(false);
      await expect(page.getByText('already-cloned', { exact: true })).toBeVisible();

      // Both saved addresses can become obsolete after a port/DHCP change.
      // Repair must use the existing computer identity and certificate pin;
      // a new pairing ceremony would hide a lost-session bug here.
      const port = await unusedPort();
      const network = await remote.rpc<Record<string, unknown>>('GetNetworkSettings');
      await remote.rpc('SetNetworkSettings', { ...network, listenPort: port });
      await page.reload();
      await settingsPage(page, 'Connections');
      await expect(computerRow).toContainText('Unreachable');
      await computerRow.getByRole('button', { name: 'Change address' }).click();
      await computerRow.getByLabel('New computer address').fill(`127.0.0.1:${port}`);
      await computerRow.getByRole('button', { name: 'Verify & reconnect' }).click();
      await expect(computerRow).toContainText('Connected');
      expect(await home.rpc<Array<{ id: string }>>('ListBackends')).toEqual(expect.arrayContaining([expect.objectContaining({ id: remoteId })]));
      await page.getByRole('button', { name: 'Close Settings', exact: true }).click();

      // Stop one independently; a remembered catalog must not disappear on
      // reload and the other computer must remain usable.
      await remote.stop();
      await page.reload();
      await expect(page.getByText('already-cloned', { exact: true })).toBeVisible();
      await expect(page.getByText('Mac checkout', { exact: true })).toBeVisible();
      await expect.poll(() => page.evaluate(() => getComputedStyle(document.documentElement).fontSize)).toBe(changedFont);
      expect(errors).toEqual([]);
      expect(surfaced.errorToasts).toEqual([]);
    } finally {
      pairing?.close();
      await other.close();
      await page.close();
      await remote?.close();
      await home?.close();
    }
  });
}
