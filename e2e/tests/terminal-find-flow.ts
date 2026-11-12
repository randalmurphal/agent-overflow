// Shared steps of the thread terminal's find specs (terminal-find.spec.ts,
// compact-terminal-find.spec.ts): a chat thread with its terminal drawer
// open on /bin/sh, and a shell that has printed known lines. The terminal
// draws with WebGL, so what the shell printed is read from the computer's
// replay.
import type { Page } from '@playwright/test';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, type SeedResult } from './fixtures.js';
import { SHELL_ENV, openThread, terminalInput, terminalTabs } from './terminal-thread-lifetime-flow.js';

/** Printed by `printTangos`: three lines no command echo contains. */
export const QUERY = 'tango-';

export interface TerminalFindSetup {
  host: HarnessApp;
  threadId: string;
}

export async function replayOf(host: HarnessApp, threadId: string, index = 0): Promise<string> {
  const shells = await host.rpc<Array<{ terminalID: string }>>('ListTerminals', threadId);
  const shell = shells[index];
  if (!shell) return '';
  const replay = await host.rpc<{ data: string }>('GetTerminalReplay', shell.terminalID);
  return Buffer.from(replay.data, 'base64').toString();
}

/** The header's terminal toggle; under compact it is a row of the header menu. */
export async function toggleTerminal(page: Page): Promise<void> {
  if ((await page.locator('html').getAttribute('data-compact-screen')) === null) {
    await page.getByTestId('terminal-toggle').click();
    return;
  }
  await page.getByTestId('chat-header-more').click();
  await page.locator('[data-popover]').getByText('Terminal', { exact: true }).click();
}

export async function openDrawerThread(page: Page, title: string): Promise<TerminalFindSetup> {
  const host = await launchHarness({ env: SHELL_ENV });
  const seed = await host.rpc<SeedResult>('HarnessSeed', { projects: [{
    name: 'terminal-find', repo: {}, threads: [{ title, provider: 'claude',
      turns: [{ userText: 'hello', items: [{ kind: 'assistant_text', summary: 'hi' }] }] }],
  }] });
  const threadId = seed.projects[0].threadIds[0];
  await host.open(page);
  await openThread(page, title);
  await toggleTerminal(page);
  await expect(terminalTabs(page)).toHaveCount(1);
  await expect.poll(() => replayOf(host, threadId)).toContain('$');
  return { host, threadId };
}

/** Types a command at the focused shell's prompt and waits for `expectText`. */
export async function runInShell(page: Page, setup: TerminalFindSetup, command: string, expectText: string): Promise<void> {
  await terminalInput(page).focus();
  await page.keyboard.type(command);
  await page.keyboard.press('Enter');
  await expect.poll(() => replayOf(setup.host, setup.threadId)).toContain(expectText);
}

export async function printTangos(page: Page, setup: TerminalFindSetup): Promise<void> {
  await runInShell(page, setup, "printf '%s-%s\\n' tango x tango y tango z", 'tango-z');
}

export function drawer(page: Page) {
  return page.getByTestId('terminal-drawer');
}

export function decorations(page: Page) {
  return drawer(page).locator('.terminal-find-match');
}
