// Two independently booted computers, real pairing/proxy/RPC and production
// frontend. Profiles and network identities outlive HarnessReset, so this
// flow owns both backends and all of its browser contexts.
import { type Page, type WebSocketRoute } from '@playwright/test';
import { expect, test } from './base-test.js';
import { harnessTiming, launchHarness, type HarnessApp } from '../src/harness.js';
import { connectedAfterConfirm, headlessPairing } from './headless-pairing-helpers.js';
import { startTogether } from './launch-helpers.js';
import { reloadAtRoot } from './compact-history-helpers.js';
import { instrument, socketAnswers } from './offhost-helpers.js';
import { expectProjectsMenuSays, watchProjectsQuiet } from './sidebar-quiet-helpers.js';
import { awaitTurnCompleted, setScenario, threadToolsScenario } from './thread-tools-helpers.js';
import { randomUUID } from 'node:crypto';
import { unusedPort } from '../src/ports.js';
import { doneResult, humanGateWorkflow, seedWorkflow, setClaudeScenario, startWorkflow, waitForWorkflowState, type WorkflowDetail } from './workflows-helpers.js';

// How long the page waits before it calls a computer unreachable
// (transportStatus UNREACHABLE_SETTLE_MS, half a minute in the product).
const SETTLE_MS = 5_000;

interface Seed { projects: Array<{ projectId: string; path: string; threadIds: string[] }> }
interface ProjectRow { project: { id: string; name: string; path: string } }

// Closes Settings and waits out its exit fade. Until the overlay is gone its
// Close button still reads visible, and a click on it then never lands.
async function closeSettings(page: Page): Promise<void> {
  await page.getByRole('button', { name: 'Close Settings', exact: true }).click();
  await expect(page.getByTestId('settings-overlay')).toHaveCount(0);
}

async function settingsPage(page: Page, name: string): Promise<void> {
  if (await page.getByTestId('settings-overlay').isVisible()) await closeSettings(page);
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
    const reciprocal = await browser.newContext();
    const errors: string[] = [];
    const surfaced = await instrument(page);
    let remoteOnline = true;
    let remoteSocket: { page: WebSocketRoute; server: WebSocketRoute } | undefined;
    const answers = socketAnswers();
    await page.routeWebSocket(/\/ws\/backend\//, (socket) => {
      if (!remoteOnline) { void socket.close({ code: 1012 }); return; }
      const server = answers.connect(socket);
      remoteSocket = { page: socket, server };
    });
    page.on('pageerror', (error) => errors.push(error.message));
    try {
      [home, remote] = await startTogether(launchHarness({ env: harnessTiming({ 'unreachable-settle': `${SETTLE_MS}ms` }) }), launchHarness());
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
      // The receiving computer's desktop is already open when another
      // computer enrolls. Reciprocal enrollment must publish its new profile
      // to that live page, not wait for another bootstrap/restart.
      const remotePage = await reciprocal.newPage();
      const remoteSurfaced = await instrument(remotePage);
      remotePage.on('pageerror', (error) => errors.push(error.message));
      await remote.open(remotePage);
      await expect(remotePage.getByTestId('thread-row').filter({ hasText: 'GPU conversation' })).toBeVisible();
      await expect(remotePage.getByTestId('thread-row').filter({ hasText: 'Mac conversation' })).toHaveCount(0);

      await settingsPage(page, 'Connect to a computer');
      pairing = await headlessPairing(remote);
      const invite = pairing.invite;
      await page.getByRole('textbox', { name: /^(Computer address or pairing link|Pairing link)$/ }).fill(invite.url);
      await page.getByRole('button', { name: 'Connect', exact: true }).click();
      const verification = page.getByLabel('Verification number');
      await expect(verification).toBeVisible();
      await pairing.confirm((await verification.textContent())!.trim());
      const computerRow = page.getByTestId('attached-system');
      await connectedAfterConfirm({ home, remote }, (wait) => expect(computerRow).toContainText('Connected', wait));
      const systems = await home.rpc<Array<{ id: string }>>('ListBackends');
      expect(systems).toHaveLength(1);
      const remoteId = systems[0].id;
      await home.rpc('RenameBackend', remoteId, 'GPU workstation');
      await expect(computerRow).toContainText('GPU workstation');
      await connectedAfterConfirm({ home, remote }, (wait) => expect(remotePage.getByTestId('thread-row').filter({ hasText: 'Mac conversation' })).toBeVisible(wait));
      const homeDraft = await home.rpc<{ id: string }>('CreateThread', {
        projectId: homeSeed.projects[0].projectId, title: 'Pushed after reciprocal pairing', provider: 'claude',
      });
      await home.rpc('SaveDraft', homeDraft.id, 'Not sent yet', [], [], null);
      await expect(remotePage.getByTestId('thread-row').filter({ hasText: 'Pushed after reciprocal pairing' })).toBeVisible();

      // The remote computer has never been opened in a pane. Its sidebar
      // must still receive new rows live, across replay, and after replay.
      // Keep home online throughout so a home restart cannot hide a missing
      // subscription or accidentally repair the remote catalog.
      await closeSettings(page);
      const createRemoteDraft = async (title: string) => {
        const thread = await remote!.rpc<{ id: string }>('CreateThread', {
          projectId: remoteSeed.projects[0].projectId, title, provider: 'codex',
        });
        await remote!.rpc('SaveDraft', thread.id, 'Not sent yet', [], [], null);
      };
      await createRemoteDraft('Created before outage');
      await expect(page.getByTestId('thread-row').filter({ hasText: 'Created before outage' })).toBeVisible();
      expect(remoteSocket).toBeDefined();
      remoteOnline = false;
      await remoteSocket!.page.close({ code: 1012, reason: 'attached computer outage' });
      await remoteSocket!.server.close();
      await createRemoteDraft('Created during outage');
      const answered = answers.count();
      remoteOnline = true;
      await answers.waitBeyond(answered);
      await expect(page.getByTestId('thread-row').filter({ hasText: 'Created during outage' })).toBeVisible();
      await createRemoteDraft('Created after outage');
      await expect(page.getByTestId('thread-row').filter({ hasText: 'Created after outage' })).toBeVisible();

      // Backend pairing alone does not allow model commands. Toggle the real
      // originating host's opt-in, then exercise ao-remote-tools MCP from a
      // mocked provider session (never the page's token).
      await settingsPage(page, 'Agent remote tools');
      const agentAccess = page.locator('section').filter({ has: page.getByRole('heading', { name: 'Agent remote tools', exact: true }) });
      await expect(agentAccess.getByRole('button', { name: 'Enable', exact: true })).toBeVisible();
      await agentAccess.getByRole('button', { name: 'Enable', exact: true }).click();
      await expect(agentAccess.getByRole('button', { name: 'Enabled', exact: true })).toHaveAttribute('aria-pressed', 'true');
      const sourceThread = homeSeed.projects[0].threadIds[0];
      const commandID = randomUUID();
      await setScenario(home, homeSeed.projects[0].path, threadToolsScenario({
        name: 'connected-remote-mcp',
        provider: 'claude',
        turns: [
          {
            steps: [
              { call: { server: 'ao-remote-tools', tool: 'remote_computers', args: {} } },
              {
                call: {
                  server: 'ao-remote-tools',
                  tool: 'remote_run',
                  args: {
                    computer_id: remoteId,
                    project_id: remoteSeed.projects[0].projectId,
                    request_id: commandID,
                    label: 'peer check',
                    argv: ['sh', '-c', 'printf peer-command-result'],
                    wait_seconds: 30,
                  },
                },
              },
            ],
            text: 'Remote command finished.',
          },
          {
            steps: [
              { call: { server: 'ao-remote-tools', tool: 'remote_computers', args: {} } },
              { call: { server: 'ao-remote-tools', tool: 'remote_status', args: { computer_id: remoteId, request_id: commandID } } },
            ],
            text: 'Checked after opt-out.',
          },
        ],
        afterTurns: 'silent',
      }));
      await home.startSession(sourceThread);
      const firstTurn = awaitTurnCompleted(home, sourceThread);
      const listedPromise = home.awaitMcpResult({ server: 'ao-remote-tools', tool: 'remote_computers', cwd: homeSeed.projects[0].path, timeoutMs: 30_000 });
      const startedPromise = home.awaitMcpResult({ server: 'ao-remote-tools', tool: 'remote_run', cwd: homeSeed.projects[0].path, timeoutMs: 60_000 });
      await home.send(sourceThread, 'run a remote command');
      const listed = await listedPromise;
      expect(listed.report.isError, listed.report.result).toBeFalsy();
      expect(JSON.parse(listed.report.result ?? '[]')).toEqual(expect.arrayContaining([expect.objectContaining({ id: remoteId })]));
      const started = await startedPromise;
      expect(started.report.isError, started.report.result).toBeFalsy();
      expect(JSON.parse(started.report.result ?? '{}')).toMatchObject({ state: 'succeeded', output: 'peer-command-result', sourceThreadId: sourceThread });
      await firstTurn;
      // Opt-out hides discovery and blocks new work while preserving receipts.
      await agentAccess.getByRole('button', { name: 'Enabled', exact: true }).click();
      await expect(agentAccess.getByRole('button', { name: 'Enable', exact: true })).toHaveAttribute('aria-pressed', 'false');
      const secondTurn = awaitTurnCompleted(home, sourceThread);
      const disabledPromise = home.awaitMcpResult({ server: 'ao-remote-tools', tool: 'remote_computers', cwd: homeSeed.projects[0].path, timeoutMs: 30_000 });
      const receiptPromise = home.awaitMcpResult({ server: 'ao-remote-tools', tool: 'remote_status', cwd: homeSeed.projects[0].path, timeoutMs: 30_000 });
      await home.send(sourceThread, 'check after opt-out');
      const disabled = await disabledPromise;
      expect(disabled.report.isError, disabled.report.result).toBeFalsy();
      expect(JSON.parse(disabled.report.result ?? 'null')).toEqual([]);
      const receipt = await receiptPromise;
      expect(receipt.report.isError, receipt.report.result).toBeFalsy();
      expect(JSON.parse(receipt.report.result ?? '{}')).toMatchObject({ state: 'succeeded', output: 'peer-command-result' });
      await secondTurn;


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
      await size.selectOption('17');
      await expect.poll(() => page.evaluate(() => getComputedStyle(document.documentElement).fontSize)).not.toBe(initialFont);
      const changedFont = await page.evaluate(() => getComputedStyle(document.documentElement).fontSize);
      expect(await second.evaluate(() => getComputedStyle(document.documentElement).fontSize)).toBe(initialFont);
      await closeSettings(page);

      await page.getByTestId('sidebar-tool-workflows').click();
      await page.getByTestId('workflows-pause-all').click();
      await expect.poll(async () => (await home!.rpc<{ paused: boolean }>('WorkflowGetEngineState')).paused).toBe(true);
      await expect.poll(async () => (await remote!.rpc<{ paused: boolean }>('WorkflowGetEngineState')).paused).toBe(true);
      await page.getByTestId('workflow-run-row').filter({ hasText: 'Work on the GPU computer' }).click();
      await page.locator('[data-testid="workflow-map-node"][data-phase-id="plan"]').getByTestId('workflow-map-node-label').click();
      await expect(page.locator(`[data-ui-surface="chat"][data-thread-id="${workflowThread}"]`)).toBeVisible();
      await expect(page.getByTestId('workflows-pane')).toBeHidden();
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
      await reloadAtRoot(page);
      await settingsPage(page, 'Connect to a computer');
      await expect(computerRow).toContainText('Offline');
      await computerRow.getByRole('button', { name: 'Change address' }).click();
      await computerRow.getByLabel('New computer address').fill(`127.0.0.1:${port}`);
      await computerRow.getByRole('button', { name: 'Verify & reconnect' }).click();
      await expect(computerRow).toContainText('Connected');
      expect(await home.rpc<Array<{ id: string }>>('ListBackends')).toEqual(expect.arrayContaining([expect.objectContaining({ id: remoteId })]));
      await closeSettings(page);

      // Stop one independently; a remembered catalog must not disappear on
      // reload and the other computer must remain usable.
      // The page clock runs as the real one does; the settle is run below
      // by moving it on.
      await page.clock.install();
      await remote.stop();
      await reloadAtRoot(page);
      await expect(page.getByText('already-cloned', { exact: true })).toBeVisible();
      await expect(page.getByText('Mac checkout', { exact: true })).toBeVisible();
      // Once its dial at the reload has failed, the page asks nothing of the
      // stopped computer until its own computer reports it back, and the one
      // timer left to move the Projects section is the computer's
      // unreachable settle. Through that settle the stopped computer adds no
      // status line and no animation to the section; the Projects menu says
      // it is offline.
      expect(await watchProjectsQuiet(page, () => page.clock.runFor(SETTLE_MS + 1_000)))
        .toMatchObject({ statusLines: [], animations: [] });
      await expectProjectsMenuSays(page, /Offline/);
      await expect.poll(() => page.evaluate(() => getComputedStyle(document.documentElement).fontSize)).toBe(changedFont);
      expect(errors).toEqual([]);
      expect(surfaced.errorToasts).toEqual([]);
      expect(remoteSurfaced.errorToasts).toEqual([]);
    } finally {
      pairing?.close();
      await reciprocal.close();
      await other.close();
      await page.close();
      await remote?.close();
      await home?.close();
    }
  });
}
