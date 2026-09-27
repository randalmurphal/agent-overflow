// A terminal thread lives as long as its shells (docs/decisions.md,
// internal/app/app_terminal_threads.go). What must hold, on the desktop and
// compact projects alike:
//
//   last exit - the last shell exiting deletes the thread on its computer,
//               and every page showing it drops the row and closes the pane
//               with no reload, including a page that did not type the exit.
//   restart   - a restart ends the thread: the next load of the page shows
//               neither its row nor the pane its saved layout held, and no
//               shell is opened again. A page that stays open through a
//               restart is terminal-thread-lifetime.spec.ts's paired case.
//
// Each case runs its own backend with /bin/sh as the shell, so the prompt
// and `exit` do not depend on the developer's login shell.
import type { Page } from '@playwright/test';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';

export interface TerminalThreadRow { id: string; title: string; mode: string }

export const SHELL_ENV = { SHELL: '/bin/sh' };

export function terminalTabs(page: Page) {
  return page.locator('[data-testid^="terminal-tab-"]:not([data-testid^="terminal-tab-close-"])');
}

export async function openThread(page: Page, title: string): Promise<void> {
  if ((await page.locator('html').getAttribute('data-compact-screen')) === 'thread') {
    await page.getByTestId('compact-back').click();
  }
  await page.getByTestId('thread-row').filter({ hasText: title }).click();
}

/** The panes on screen that show the thread titled `title`. */
export function paneShowing(page: Page, title: string) {
  return page.locator('[data-pane-id]').filter({ hasText: title });
}

export function terminalInput(page: Page) {
  return page.getByRole('textbox', { name: 'Terminal input' });
}

/**
 * Types `exit` into the page's shell once its prompt shows. The terminal
 * draws with WebGL, so the prompt is read from the computer's replay.
 */
export async function exitShell(page: Page, host: HarnessApp, threadId: string): Promise<void> {
  await expect.poll(async () => {
    const [shell] = await host.rpc<Array<{ terminalID: string }>>('ListTerminals', threadId);
    if (!shell) return '';
    const replay = await host.rpc<{ data: string }>('GetTerminalReplay', shell.terminalID);
    return Buffer.from(replay.data, 'base64').toString();
  }).toContain('$');
  await terminalInput(page).focus();
  await page.keyboard.type('exit');
  await page.keyboard.press('Enter');
}

export async function seedProject(host: HarnessApp, name = 'terminal-lifetime'): Promise<string> {
  const seed = await host.rpc<SeedResult>('HarnessSeed', { projects: [{ name, repo: {} }] });
  return seed.projects[0].projectId;
}

export async function startTerminalThread(host: HarnessApp, title: string, projectId?: string): Promise<TerminalThreadRow> {
  return host.rpc<TerminalThreadRow>('StartTerminal', { projectId: projectId ?? await seedProject(host), title });
}

/** The pane layout this browser context saved, which a load restores. */
async function persistedPaneLayout(page: Page): Promise<string> {
  return await page.evaluate(() => {
    const bucket = JSON.parse(localStorage.getItem('agent-overflow:uistate:bucket') ?? '{}') as Record<string, string>;
    return bucket.paneLayout ?? '';
  });
}

export async function rowIds(host: HarnessApp): Promise<string[]> {
  return (await host.rpc<TerminalThreadRow[]>('HarnessListThreadRows')).map((row) => row.id);
}

export function terminalThreadLifetimeFlow(): void {
  test('the last shell exiting ends the terminal thread on every page', async ({ page }) => {
    test.setTimeout(90_000);
    let host: HarnessApp | undefined;
    try {
      host = await launchHarness({ env: SHELL_ENV });
      const thread = await startTerminalThread(host, 'Short-lived shell');
      await host.open(page);
      await openThread(page, thread.title);
      await expect(paneShowing(page, thread.title)).toHaveCount(1);
      await expect(terminalTabs(page)).toHaveCount(1);

      const other = await page.context().newPage();
      await host.open(other);
      await openThread(other, thread.title);
      await expect(terminalTabs(other)).toHaveCount(1);
      // The second page adopted the running shell rather than opening one.
      expect(await host.rpc<unknown[]>('ListTerminals', thread.id)).toHaveLength(1);

      await exitShell(page, host, thread.id);

      for (const shown of [page, other]) {
        await expect(paneShowing(shown, thread.title)).toHaveCount(0);
        await expect(shown.getByTestId('thread-row').filter({ hasText: thread.title })).toHaveCount(0);
        await expect(terminalTabs(shown)).toHaveCount(0);
        await expect(terminalInput(shown)).toHaveCount(0);
      }
      expect(await rowIds(host)).not.toContain(thread.id);
      await other.close();
    } finally {
      await host?.close();
    }
  });

  test('a restart ends the terminal thread and the next load shows none of it', async ({ page }) => {
    test.setTimeout(120_000);
    const root = await mkdtemp(join(tmpdir(), 'ao-terminal-restart-'));
    let host: HarnessApp | undefined;
    try {
      host = await launchHarness({ dataDir: root, env: SHELL_ENV });
      const origin = new URL(host.url).origin;
      const thread = await startTerminalThread(host, 'Shell across a restart');
      await host.open(page);
      await openThread(page, thread.title);
      await expect(paneShowing(page, thread.title)).toHaveCount(1);
      await expect(terminalTabs(page)).toHaveCount(1);
      // The saved layout holds the terminal pane, so the next load has a
      // pane to restore.
      await expect.poll(() => persistedPaneLayout(page)).toContain(thread.id);

      expect(await host.stop()).toBe(true);
      host = await launchHarness({ dataDir: root, env: SHELL_ENV });
      expect(new URL(host.url).origin).toBe(origin);
      expect(await rowIds(host)).not.toContain(thread.id);

      await host.open(page);
      await expect(page.getByTestId('project-item').filter({ hasText: 'terminal-lifetime' })).toBeVisible();
      await expect(page.getByTestId('thread-row').filter({ hasText: thread.title })).toHaveCount(0);
      await expect(paneShowing(page, thread.title)).toHaveCount(0);
      await expect(terminalTabs(page)).toHaveCount(0);
      await expect(terminalInput(page)).toHaveCount(0);
      expect(await host.rpc<unknown[]>('ListTerminals', thread.id)).toEqual([]);
    } finally {
      try {
        await host?.close();
      } finally {
        await rm(root, { recursive: true, force: true });
      }
    }
  });
}
