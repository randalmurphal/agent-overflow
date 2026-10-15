// A Claude login past its refresh-token deadline whose access token is still
// live, through the shipped Settings page and the real backend. The CLI's last
// renewal before the deadline issued an eight-hour access token, so the login
// keeps working until the CLI would have to renew that token. The account is
// adopted from the native credential like any working login, and its card says
// when it really stops ("Login expires in 3h 5xm"), not "Login expired".
//
// Owns its backend: it adopts a provider account, which is process-wide state
// no other spec should inherit.

import { mkdir, writeFile } from 'node:fs/promises';
import * as path from 'node:path';
import { expect, test } from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';
import { claudeScenario, claudeUsageResult, emit, seedAgentThread, startMock, textLines } from './agent-visibility-helpers.js';

interface ManagedProviderAccount {
  id: string;
  provider: string;
  needsLogin: boolean;
  refreshTokenExpiresAt?: number;
}

let harness: HarnessApp;

test.beforeAll(async () => {
  harness = await launchHarness();
});

test.afterAll(async () => {
  await harness?.close();
});

test('a login past its refresh deadline with a live access token reads as working', async ({ browser }) => {
  const home = harness.bootstrap.homeDir;
  expect(home, 'the harness must isolate a provider home').toBeTruthy();
  const now = Date.now();
  const accessExpiresAt = now + 4 * 3600_000;
  await mkdir(path.join(home!, '.claude'), { recursive: true });
  await writeFile(
    path.join(home!, '.claude', '.credentials.json'),
    JSON.stringify({ claudeAiOauth: {
      accessToken: 'live-access-token',
      refreshToken: 'refresh-token',
      expiresAt: accessExpiresAt,
      refreshTokenExpiresAt: now - 2 * 3600_000,
      scopes: ['user:inference', 'user:profile'],
      subscriptionType: 'max',
    } }),
    { mode: 0o600 },
  );

  // A send reconciles the native login first, which is what adopts it.
  const threadId = await seedAgentThread(harness, 'login-past-deadline', 'Login past deadline');
  await harness.rpc('HarnessSetScenario', { scenario: claudeScenario('login-past-deadline', [emit([
    ...textLines('msg-login', 'Still working'),
    claudeUsageResult(10, 5),
  ])]) });
  await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'go', null);
  await harness.waitForEvent('provider:turn_completed');

  const accounts = await harness.rpc<ManagedProviderAccount[]>('ListProviderAccounts');
  const claude = accounts.filter((account) => account.provider === 'claude');
  expect(claude, 'the working login must be adopted as an account').toHaveLength(1);
  expect(claude[0].needsLogin).toBe(false);
  expect(claude[0].refreshTokenExpiresAt).toBe(accessExpiresAt - 5 * 60_000);

  const page = await browser.newPage();
  try {
    await harness.open(page);
    await page.getByTestId('sidebar-settings-button').click();
    await page.getByRole('tab', { name: 'Claude Code', exact: true }).click();
    const section = page.getByTestId('provider-accounts-claude');
    await expect(section).toBeVisible();
    await expect(section.getByTestId(`provider-account-login-expiry-${claude[0].id}`))
      .toHaveText(/^Login expires in 3h 5\dm$/);
    await expect(section).not.toContainText('Sign in again');
    await expect(section).not.toContainText('Login expired');
  } finally {
    await page.close();
  }
});
