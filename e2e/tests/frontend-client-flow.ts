import { test, expect } from '@playwright/test';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { headlessPairing } from './headless-pairing-helpers.js';
import { launchFrontendClient } from './frontend-client-helpers.js';

export function frontendClientFlow(): void {
  test('a new standalone frontend opens its local settings before any computer is paired', async ({ page }) => {
    test.setTimeout(45_000);
    page.setDefaultTimeout(10_000);
    const root = await mkdtemp(join(tmpdir(), 'ao-empty-frontend-'));
    let frontend: Awaited<ReturnType<typeof launchFrontendClient>> | undefined;
    const errors: string[] = [];
    page.on('pageerror', (error) => errors.push(error.message));
    try {
      frontend = await launchFrontendClient(join(root, 'profiles'), join(root, 'frontend'), '');
      await frontend.open(page);
      await page.getByRole('button', { name: 'Settings', exact: true }).click();
      await page.getByRole('tab', { name: 'Theme', exact: true }).click();
      await page.getByTestId('settings-theme-mode').selectOption('dark');
      await expect(page.getByTestId('settings-theme-mode')).toHaveValue('dark');
      const allSettings = page.getByRole('button', { name: 'All settings', exact: true });
      if (await allSettings.isVisible()) await allSettings.click();
      await page.getByRole('tab', { name: 'Computers', exact: true }).click();
      await expect(page.getByTestId('attached-system')).toHaveCount(0);
      await expect(page.getByRole('textbox', { name: 'Pairing link' })).toBeVisible();
      await expect(page.getByTestId('transport-status-banner')).toHaveCount(0);
      expect(errors).toEqual([]);
    } finally {
      try { await page.close(); await frontend?.close(); }
      finally { await rm(root, { recursive: true, force: true }); }
    }
  });
  for (const sharedRepo of [false, true]) {
    test(`the standalone frontend survives an offline launch computer and can forget it (${sharedRepo ? 'shared' : 'separate'} repository)`, async ({ page }, info) => {
      test.setTimeout(90_000);
      page.setDefaultTimeout(10_000);
      const root = await mkdtemp(join(tmpdir(), 'ao-frontend-client-'));
      let first: HarnessApp | undefined;
      let second: HarnessApp | undefined;
      let frontend: Awaited<ReturnType<typeof launchFrontendClient>> | undefined;
      const errors: string[] = [];
      page.on('pageerror', (error) => errors.push(error.message));
      try {
        first = await launchHarness({ dataDir: join(root, 'first') });
        second = await launchHarness();
        await first.rpc('HarnessSeed', { projects: [{ name: 'First computer project', repo: { commits: [{ files: { 'README.md': 'First project' } }] }, threads: [{ title: 'First computer conversation', turns: [{ userText: 'First host', items: [{ kind: 'assistant_text', summary: 'First host is ready.' }] }] }] }] });
        await second.rpc('HarnessSeed', { projects: [{ name: 'Second computer project', repo: { commits: [{ files: { 'README.md': sharedRepo ? 'First project' : 'Second project' } }] }, threads: [{ title: 'Second computer conversation', turns: [{ userText: 'Second host', items: [{ kind: 'assistant_text', summary: 'Second host is ready.' }] }] }] }] });
        let firstID = '';
        for (const [host, name] of [[first, 'First computer'], [second, 'Second computer']] as const) {
          const pairing = await headlessPairing(host);
          try {
            const result = await first.rpc<{ id: string; verificationNumber: string }>('AddBackend', pairing.invite.url);
            await pairing.confirm(result.verificationNumber);
            await first.rpc('RenameBackend', result.id, name);
            if (host === first) firstID = result.id;
          } finally { pairing.close(); }
        }
        const profiles = join(first.bootstrap.dataDir, 'device');
        const config = join(root, 'frontend');
        frontend = await launchFrontendClient(profiles, config, firstID);
        await frontend.open(page);
        // A representative can change while the same repository's two
        // memberships hydrate. Let the outgoing sidebar row finish its exit.
        await expect(page.getByTestId('thread-row-title').filter({ hasText: 'First computer conversation' })).toHaveCount(1);
        await expect(page.getByTestId('thread-row-title').filter({ hasText: 'First computer conversation' })).toBeVisible();
        await expect(page.getByTestId('thread-row-title').filter({ hasText: 'Second computer conversation' })).toHaveCount(1);
        await expect(page.getByTestId('thread-row-title').filter({ hasText: 'Second computer conversation' })).toBeVisible();
        await page.getByRole('button', { name: 'Settings', exact: true }).click();
        await page.getByRole('tab', { name: 'Theme', exact: true }).click();
        await page.getByTestId('settings-theme-mode').selectOption('dark');
        await expect(page.getByTestId('settings-theme-mode')).toHaveValue('dark');
        const hostTheme = await first.rpc<{ appearance: { mode: string } }>('GetThemeFiles');
        expect(hostTheme.appearance.mode).toBe('system');
        const port = Number(new URL(frontend.origin).port);
        await page.goto('about:blank');
        await frontend.close(); frontend = undefined;
        await first.close(); first = undefined;
        // Cold-start the frontend while the computer named by --connect is off.
        frontend = await launchFrontendClient(profiles, config, firstID, port);
        await frontend.open(page);
        await expect(page.getByTestId('thread-row-title').filter({ hasText: 'Second computer conversation' })).toBeVisible();
        await page.getByRole('button', { name: 'Settings', exact: true }).click();
        await page.getByRole('tab', { name: 'Theme', exact: true }).click();
        await expect(page.getByTestId('settings-theme-mode')).toHaveValue('dark');
        const allSettings = page.getByRole('button', { name: 'All settings', exact: true });
        if (await allSettings.isVisible()) await allSettings.click();
        await page.getByRole('tab', { name: 'Computers', exact: true }).click();
        const removed = page.getByTestId('attached-system').filter({ hasText: 'First computer' });
        await expect(removed).toBeVisible();
        await removed.getByRole('button', { name: 'Remove', exact: true }).click();
        await removed.getByRole('button', { name: 'Confirm remove', exact: true }).click();
        await expect(removed).toHaveCount(0);
        await expect(page.getByTestId('attached-system')).toHaveCount(1);
        await expect(page.getByTestId('attached-system')).toContainText('Second computer');
        await page.screenshot({ path: info.outputPath('computers.png') });
        await page.getByRole('button', { name: 'Close Settings', exact: true }).click();
        const remainingThread = page.getByTestId('thread-row-title').filter({ hasText: 'Second computer conversation' });
        await expect(remainingThread).toHaveCount(1);
        await remainingThread.click();
        await expect(page.getByText('Second host is ready.', { exact: true })).toBeVisible();
        await page.goto('about:blank');
        await frontend.close(); frontend = undefined;
        // An update reopens --frontend, with no invitation or named computer.
        // The original host has been removed; the remaining catalog still boots.
        frontend = await launchFrontendClient(profiles, config, '', port);
        await frontend.open(page);
        await expect(page.getByTestId('thread-row-title').filter({ hasText: 'Second computer conversation' })).toHaveCount(1);
        await page.getByTestId('thread-row-title').filter({ hasText: 'Second computer conversation' }).click();
        await expect(page.getByText('Second host is ready.', { exact: true })).toBeVisible();
        expect(errors).toEqual([]);
      } finally {
        try { await page.close(); await frontend?.close(); await second?.close(); await first?.close(); }
        finally { await rm(root, { recursive: true, force: true }); }
      }
    });
  }
}
