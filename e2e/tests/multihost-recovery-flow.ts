// An execution host restarts while a standalone frontend remains alive. The
// other host must still accept/render a turn, and the restarted host must
// recover its history and accept new work without reloading or re-pairing.
// Both processes are real isolated hosts; this does not stand in for OS- or
// release-specific Windows/Android validation.
import { expect, test, type Page } from '@playwright/test';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { isAbsolute, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { launchFrontendClient } from './frontend-client-helpers.js';
import { headlessPairing } from './headless-pairing-helpers.js';
import { RESULT_LINE, advance, claudeScenario, emit, listItems, seedAgentThread, startMock, textLines, waitForGate } from './agent-visibility-helpers.js';
import { confirmOnHost, instrument, type PairingInvite } from './offhost-helpers.js';

async function openThread(page: Page, title: string): Promise<void> {
  if ((await page.locator('html').getAttribute('data-compact-screen')) === 'thread') {
    await page.getByTestId('compact-back').click();
  }
  await page.getByTestId('thread-row').filter({ hasText: title }).click();
  await expect(page.getByTestId('chat-header-title')).toHaveText(title);
}

async function stageTurn(host: HarnessApp, thread: string, label: string): Promise<string> {
  await host.rpc('HarnessSetScenario', { scenario: claudeScenario(label, [
    emit(textLines(`${label}-start`, `${label} is running.`)),
    { waitSignal: { name: 'finish' } },
    emit([...textLines(`${label}-end`, `${label} is complete.`), RESULT_LINE]),
  ]) });
  return startMock(host, thread);
}

async function baselinePairing(host: HarnessApp) {
  // Older releases have the desktop app's pairing RPCs but no `pair` CLI.
  // Exercise those real owner actions without requiring a newer discovery
  // file or adding a compatibility branch to the production application.
  await host.rpc('SetNetworkSettings', { bindAll: true });
  const invite = await host.rpc<PairingInvite>('MintDevicePairing', 'desktop', 'full');
  return { invite, confirm: (number: string) => confirmOnHost(host, number), close() {} };
}

export function multihostRecoveryFlow(): void {
  // Optional actual release binary, not a rewritten hello/version field.
  // The current mock is explicit because a release artifact has no sibling
  // mock executable. All homes/state still belong to launchHarness.
  const baseline = process.env.AO_E2E_RECOVERY_BASELINE;
  if (baseline && !isAbsolute(baseline)) throw new Error('AO_E2E_RECOVERY_BASELINE must name an absolute saved-release binary path');
  const firstOptions = baseline ? { binary: baseline, mockProvider: fileURLToPath(new URL('../../bin/ao-mockprovider', import.meta.url)) } : {};
  test(`one host can restart during a turn without stranding either computer${baseline ? ' (saved release host; graceful restart)' : ''}`, async ({ page }) => {
    test.setTimeout(120_000);
    page.setDefaultTimeout(10_000);
    const root = await mkdtemp(join(tmpdir(), 'ao-multihost-recovery-'));
    const firstData = join(root, 'first');
    let first: HarnessApp | undefined;
    let second: HarnessApp | undefined;
    let frontend: Awaited<ReturnType<typeof launchFrontendClient>> | undefined;

    const surfaced = await instrument(page);
    const errors: string[] = [];
    page.on('pageerror', error => errors.push(error.message));
    try {
      first = await launchHarness({ ...firstOptions, dataDir: firstData });
      second = await launchHarness();
      if (baseline) {
        expect(first.bootstrap.version).toBeTruthy();
        expect(second.bootstrap.version).toBeTruthy();
        expect(first.bootstrap.version).not.toBe(second.bootstrap.version);
        test.info().annotations.push({ type: 'host versions', description: `${first.bootstrap.version} and ${second.bootstrap.version}` });
      }
      const firstThread = await seedAgentThread(first, 'first-project', 'First host conversation');
      const secondThread = await seedAgentThread(second, 'second-project', 'Second host conversation');
      // The frontend alone owns its pairings. Sharing a live host's profile
      // files would introduce two independent session-renewal owners, which
      // is not the standalone app's real setup flow.
      frontend = await launchFrontendClient(join(root, 'profiles'), join(root, 'frontend'), '');
      await frontend.open(page);
      await page.getByRole('button', { name: 'Settings', exact: true }).click();
      await page.getByRole('tab', { name: 'Connect to a computer', exact: true }).click();
      for (const [index, host] of [first, second].entries()) {
        const pairing = baseline && host === first ? await baselinePairing(host) : await headlessPairing(host);
        try {
          await page.getByRole('textbox', { name: /^(Computer address or pairing link|Pairing link)$/ }).fill(pairing.invite.url);
          await page.getByRole('button', { name: 'Connect', exact: true }).click();
          const verification = page.getByLabel('Verification number');
          await expect(verification).toBeVisible();
          await pairing.confirm((await verification.textContent())!.trim());
          await expect(page.getByTestId('attached-system')).toHaveCount(index + 1);
          await expect(page.getByTestId('attached-system')).toContainText(Array(index + 1).fill('Connected'));
        } finally { pairing.close(); }
      }
      await page.getByRole('button', { name: 'Close Settings', exact: true }).click();
      await expect(page.getByTestId('thread-row')).toHaveCount(2);
      await openThread(page, 'First host conversation');
      await stageTurn(first, firstThread, 'Interrupted host');
      const input = page.getByLabel('Message Input');
      const stop = page.getByRole('button', { name: 'Interrupt current turn', exact: true });
      await input.fill('Start on the first host.');
      await page.getByRole('button', { name: 'Send message', exact: true }).click();
      await waitForGate(first, 'finish');
      await expect(page.getByText('Interrupted host is running.', { exact: true })).toBeVisible();
      await expect(stop).toBeVisible();
      const originalOrigin = new URL(first.url).origin;
      await test.step(baseline ? 'stop the saved release host normally' : 'lose the active host and its provider process', async () => {
        // The saved 0.0.14 harness predates the current lifetime-lock fix
        // and cannot reopen after SIGKILL. Do not delete its lock to make
        // compatibility look green: current hosts exercise abrupt loss;
        // the optional old-host leg exercises a normal stop/restart.
        expect(await (baseline ? first!.stop() : first!.crash())).toBe(true);
      });
      first = undefined;

      // Changing the viewed host must release the failed host's turn/send
      // state. Catalog visibility alone would miss the former frozen UI.
      await test.step('switch away from the offline host', () => openThread(page, 'Second host conversation'));
      await expect(stop).toHaveCount(0);
      await expect(input).toBeEnabled();
      const secondMock = await stageTurn(second, secondThread, 'Surviving host');
      await input.fill('Work while the first host is offline.');
      await page.getByRole('button', { name: 'Send message', exact: true }).click();
      await waitForGate(second, 'finish');
      await expect(page.getByText('Surviving host is running.', { exact: true })).toBeVisible();
      await expect(stop).toBeVisible();

      // Recover A while B is still running. A fresh launch identity must not
      // clear B's active state or replace its visible timeline.
      first = await test.step('restart the same saved host', () => launchHarness({ ...firstOptions, dataDir: firstData }));
      expect(new URL(first.url).origin).toBe(originalOrigin);
      // Harness boot deliberately ignores persisted network settings. The
      // old desktop pairing link uses LAN, so restore that fixture listener
      // on each launch without changing the saved pairing or session.
      if (baseline) {
        await first.rpc('SetNetworkSettings', { bindAll: false });
        await first.rpc('SetNetworkSettings', { bindAll: true });
      }
      await advance(second, secondMock, 'finish');
      await second.waitForEvent('provider:turn_completed');
      await expect(page.getByText('Surviving host is complete.', { exact: true })).toBeVisible();
      await expect(stop).toHaveCount(0);
      expect((await listItems(second, secondThread)).filter(item => item.kind === 'user_text' && item.summary === 'Work while the first host is offline.')).toHaveLength(1);

      await openThread(page, 'First host conversation');
      await expect(input).toBeEnabled({ timeout: 30_000 });
      await expect(stop).toHaveCount(0);
      await expect(page.getByText('Interrupted host is running.', { exact: true })).toBeVisible();
      const resumedMock = await stageTurn(first, firstThread, 'Recovered host');
      await input.fill('Continue after restarting the host.');
      await page.getByRole('button', { name: 'Send message', exact: true }).click();
      await waitForGate(first, 'finish');
      await expect(page.getByText('Recovered host is running.', { exact: true })).toBeVisible();
      await expect(stop).toBeVisible();
      await advance(first, resumedMock, 'finish');
      await first.waitForEvent('provider:turn_completed');
      await expect(page.getByText('Recovered host is complete.', { exact: true })).toBeVisible();
      await expect(stop).toHaveCount(0);
      const firstItems = await listItems(first, firstThread);
      for (const message of ['Start on the first host.', 'Continue after restarting the host.']) {
        expect(firstItems.filter(item => item.kind === 'user_text' && item.summary === message)).toHaveLength(1);
      }
      expect(errors).toEqual([]);
      expect(surfaced.errorToasts).toEqual([]);
    } finally {
      try { await page.close(); await frontend?.close(); await second?.close(); await first?.close(); }
      finally { await rm(root, { recursive: true, force: true }); }
    }
  });
}
