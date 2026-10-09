// Error reports on a paired off-host browser: a failed Push answers the
// device with the method's own error text and wrap chain (no origin
// redaction), and the device reads the backend log lines behind the
// failure from the backend that answered, under its threads:operate grant.
//
// The peer is a real second browser context on a non-loopback address,
// paired through the shipped pairing screen (`offhost-helpers.ts`). It owns
// its backend because the LAN bind persists to the settings file, which
// `harness.reset()` does not undo.
import { expect, test, type BrowserContext, type Page } from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';
import { type SeedResult } from './fixtures.js';
import {
  PAIRED_APP_MOUNT_MS,
  confirmOnHost,
  instrument,
  mintInvite,
  nonLoopbackIPv4,
  redeemOnScreen,
  type Surfaced,
} from './offhost-helpers.js';
import { harnessGit } from './worktree-removal-helpers.js';

const lanIP = nonLoopbackIPv4();
const TITLE = 'Push from the couch';
const NO_REMOTE = 'cannot push because no git remote is configured';

test.describe.serial('off-host error reports', () => {
  test.skip(
    lanIP === null,
    'outside the test network namespace, so no off-host peer can be produced without LAN traffic',
  );

  let harness: HarnessApp;
  let phoneContext: BrowserContext;
  let phone: Page;
  let surfaced: Surfaced;

  test.beforeAll(async ({ browser }) => {
    harness = await launchHarness();
    const network = await harness.rpc<{ bindAll: boolean }>('SetNetworkSettings', { bindAll: true });
    expect(network.bindAll).toBe(true);

    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [{
        name: 'offhost-error-report',
        repo: { commits: [{ message: 'init', files: { 'notes.md': 'base\n' } }] },
        threads: [{ title: TITLE, provider: 'claude', turns: [{ userText: 'first', items: [{ kind: 'assistant_text', summary: 'Ready.' }] }] }],
      }],
    });
    const root = seed.projects[0].path;
    harnessGit(harness, root, 'checkout', '--quiet', '-b', 'topic');
    harnessGit(harness, root, 'commit', '--quiet', '--allow-empty', '-m', 'topic work');

    phoneContext = await browser.newContext();
    phone = await phoneContext.newPage();
    surfaced = await instrument(phone);
    const invite = await mintInvite(harness, 'full');
    const shown = await redeemOnScreen(phone, invite, 'Couch laptop');
    await confirmOnHost(harness, shown);
    await expect(phone.getByTestId('thread-row')).toHaveCount(1, { timeout: PAIRED_APP_MOUNT_MS });
  });

  test.afterAll(async () => {
    await phoneContext?.close();
    await harness?.rpc('SetNetworkSettings', { bindAll: false }).catch(() => undefined);
    await harness?.close();
  });

  test('a failed Push shows the paired device its error chain and reads the backend log', async () => {
    await phone.getByTestId('thread-row').filter({ hasText: TITLE }).click();
    await expect(phone.getByLabel('Message Input')).toBeEnabled();

    const push = phone.getByRole('menu', { name: 'Git actions' }).getByRole('menuitem', { name: 'Push', exact: true });
    await expect(async () => {
      await phone.getByRole('button', { name: 'More git actions' }).click();
      try {
        await expect(push).toBeEnabled({ timeout: 1_000 });
      } catch (err) {
        await phone.keyboard.press('Escape');
        throw err;
      }
    }).toPass({ timeout: 30_000 });
    await push.click();

    const row = phone.getByTestId('pane-error-banner').filter({ hasText: `Push failed: ${NO_REMOTE}` });
    await expect(row).toBeVisible();
    await row.getByTestId('error-details-toggle').click();
    const details = row.getByTestId('error-details');
    await expect(details).toContainText('GitPush');
    await expect(details.locator('li')).toContainText([NO_REMOTE]);

    // The log read went to the backend that answered and was admitted.
    await expect.poll(() => surfaced.rpcReplies.includes('GetErrorLogLines')).toBe(true);
    expect(surfaced.refusals.filter((refusal) => refusal.startsWith('GetErrorLogLines'))).toEqual([]);
  });
});
