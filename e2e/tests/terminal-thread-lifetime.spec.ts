// Terminal lifetimes on the desktop project. Beside the shared flow
// (terminal-thread-lifetime-flow.ts):
//
//   drawer   - a chat thread's drawer terminal exiting removes its tab and
//              collapses the drawer. The thread stays, no shell reopens, and
//              the drawer opens a new one when asked.
//   computer - a terminal thread on a paired computer, shown by a standalone
//              frontend, ends with its last shell, and a crash of that
//              computer ends another one: the page closes the pane once the
//              computer is back.
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';
import { launchFrontendClient } from './frontend-client-helpers.js';
import { headlessPairing } from './headless-pairing-helpers.js';
import {
  SHELL_ENV,
  exitShell,
  openThread,
  paneShowing,
  rowIds,
  seedProject,
  startTerminalThread,
  terminalInput,
  terminalTabs,
  terminalThreadLifetimeFlow,
} from './terminal-thread-lifetime-flow.js';

terminalThreadLifetimeFlow();

test('a drawer terminal exiting collapses the drawer and keeps the chat thread', async ({ page }) => {
  test.setTimeout(90_000);
  let host: HarnessApp | undefined;
  try {
    host = await launchHarness({ env: SHELL_ENV });
    const seed = await host.rpc<SeedResult>('HarnessSeed', { projects: [{
      name: 'drawer-lifetime', repo: {}, threads: [{ title: 'Chat with a drawer', provider: 'claude',
        turns: [{ userText: 'hello', items: [{ kind: 'assistant_text', summary: 'hi' }] }] }],
    }] });
    const threadId = seed.projects[0].threadIds[0];
    await host.open(page);
    await openThread(page, 'Chat with a drawer');
    await page.getByTestId('terminal-toggle').click();
    await expect(terminalTabs(page)).toHaveCount(1);

    await exitShell(page, host, threadId);

    await expect(terminalTabs(page)).toHaveCount(0);
    await expect(page.getByTestId('terminal-open')).toHaveCount(0);
    await expect(terminalInput(page)).toHaveCount(0);
    await expect(page.getByTestId('thread-row').filter({ hasText: 'Chat with a drawer' })).toHaveCount(1);
    await expect(paneShowing(page, 'Chat with a drawer')).toHaveCount(1);
    expect(await rowIds(host)).toContain(threadId);
    expect(await host.rpc<unknown[]>('ListTerminals', threadId)).toEqual([]);

    await page.getByTestId('terminal-toggle').click();
    await expect(terminalTabs(page)).toHaveCount(1);
    expect(await host.rpc<unknown[]>('ListTerminals', threadId)).toHaveLength(1);
  } finally {
    await host?.close();
  }
});

test('a paired computer ends its terminal threads on a last exit and on a crash', async ({ page }) => {
  test.setTimeout(150_000);
  page.setDefaultTimeout(15_000);
  const root = await mkdtemp(join(tmpdir(), 'ao-terminal-computer-'));
  const hostData = join(root, 'host');
  let host: HarnessApp | undefined;
  let frontend: Awaited<ReturnType<typeof launchFrontendClient>> | undefined;
  try {
    host = await launchHarness({ dataDir: hostData, env: SHELL_ENV });
    const origin = new URL(host.url).origin;
    const projectId = await seedProject(host, 'computer-terminals');
    const exited = await startTerminalThread(host, 'Remote shell that exits', projectId);
    const crashed = await startTerminalThread(host, 'Remote shell through a crash', projectId);

    frontend = await launchFrontendClient(join(root, 'profiles'), join(root, 'frontend'), '');
    await frontend.open(page);
    await page.getByRole('button', { name: 'Settings', exact: true }).click();
    await page.getByRole('tab', { name: 'Connect to a computer', exact: true }).click();
    const pairing = await headlessPairing(host);
    try {
      await page.getByRole('textbox', { name: /^(Computer address or pairing link|Pairing link)$/ }).fill(pairing.invite.url);
      await page.getByRole('button', { name: 'Connect', exact: true }).click();
      const verification = page.getByLabel('Verification number');
      await expect(verification).toBeVisible();
      await pairing.confirm((await verification.textContent())!.trim());
      await expect(page.getByTestId('attached-system')).toContainText('Connected');
    } finally { pairing.close(); }
    await page.getByRole('button', { name: 'Close Settings', exact: true }).click();

    const row = (title: string) => page.getByTestId('thread-row').filter({ hasText: title });
    await openThread(page, exited.title);
    await expect(terminalTabs(page)).toHaveCount(1);
    await exitShell(page, host, exited.id);
    await expect(paneShowing(page, exited.title)).toHaveCount(0);
    await expect(row(exited.title)).toHaveCount(0);
    await expect(terminalTabs(page)).toHaveCount(0);
    expect(await rowIds(host)).not.toContain(exited.id);

    await openThread(page, crashed.title);
    await expect(paneShowing(page, crashed.title)).toHaveCount(1);
    await expect(terminalTabs(page)).toHaveCount(1);
    expect(await host.crash()).toBe(true);
    host = await launchHarness({ dataDir: hostData, env: SHELL_ENV });
    expect(new URL(host.url).origin).toBe(origin);
    expect(await rowIds(host)).not.toContain(crashed.id);
    await expect(paneShowing(page, crashed.title)).toHaveCount(0, { timeout: 30_000 });
    await expect(row(crashed.title)).toHaveCount(0);
    await expect(terminalTabs(page)).toHaveCount(0);
    await expect(terminalInput(page)).toHaveCount(0);
    expect(await host.rpc<unknown[]>('ListTerminals', crashed.id)).toEqual([]);
  } finally {
    try { await page.close(); await frontend?.close(); await host?.close(); }
    finally { await rm(root, { recursive: true, force: true }); }
  }
});
