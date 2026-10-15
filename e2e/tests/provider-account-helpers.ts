// Saved provider accounts through the shipped Settings page: a native Claude
// login to adopt, a sign-in finished through the mock's code paste, and the
// card order as the page draws it.

import { mkdir, writeFile } from 'node:fs/promises';
import * as path from 'node:path';
import { expect, type Locator, type Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';

export interface ManagedProviderAccount {
  id: string;
  provider: string;
  email?: string;
  active: boolean;
  refreshTokenExpiresAt?: number;
}

// The code the mock's approval page shows; `:<email>` picks the account the
// sign-in lands on (cmd/ao-mockprovider/login.go).
const MOCK_CLAUDE_CODE = 'mock-auth-code';
// A sign-in spawns the mock's login process and probes the home it wrote.
export const SIGNIN_STEP_MS = 20_000;

/**
 * A native Claude login already on the machine, as Claude Code leaves one:
 * the credential, and the account record the CLI reports its identity from.
 */
export async function seedNativeClaudeLogin(
  harness: HarnessApp,
  email: string,
  sessionEndsAt: number,
): Promise<void> {
  const home = harness.bootstrap.homeDir;
  expect(home, 'the harness must isolate a provider home').toBeTruthy();
  await mkdir(path.join(home!, '.claude'), { recursive: true });
  await writeFile(
    path.join(home!, '.claude', '.credentials.json'),
    JSON.stringify({ claudeAiOauth: {
      accessToken: 'native-access-token',
      refreshToken: 'native-refresh-token',
      expiresAt: Math.min(sessionEndsAt, Date.now() + 8 * 3600_000),
      refreshTokenExpiresAt: sessionEndsAt,
      scopes: ['user:inference', 'user:profile'],
      subscriptionType: 'max',
    } }),
    { mode: 0o600 },
  );
  await writeFile(
    path.join(home!, '.claude.json'),
    JSON.stringify({ oauthAccount: {
      emailAddress: email,
      organizationUuid: 'mock-org-uuid',
      organizationName: 'Mock Org',
    } }),
    { mode: 0o600 },
  );
}

export async function claudeAccounts(harness: HarnessApp): Promise<ManagedProviderAccount[]> {
  const accounts = await harness.rpc<ManagedProviderAccount[]>('ListProviderAccounts');
  return accounts.filter((account) => account.provider === 'claude');
}

export async function claudeAccountId(harness: HarnessApp, email: string): Promise<string> {
  const account = (await claudeAccounts(harness)).find((candidate) => candidate.email === email);
  expect(account, `${email} must be saved`).toBeTruthy();
  return account!.id;
}

export function claudeAccountsSection(page: Page): Locator {
  return page.getByTestId('provider-accounts-claude');
}

export async function openClaudeAccounts(page: Page): Promise<void> {
  await page.getByTestId('sidebar-settings-button').click();
  await page.getByRole('tab', { name: 'Claude Code', exact: true }).click();
  await expect(claudeAccountsSection(page)).toBeVisible();
}

/** The account ids in the order the Settings cards are drawn. */
export async function cardOrder(page: Page): Promise<string[]> {
  return claudeAccountsSection(page)
    .locator('[data-account-card]')
    .evaluateAll((cards) => cards.map((card) => (card as HTMLElement).dataset.accountCard ?? ''));
}

/** Finishes the open Claude sign-in flow as the account `email`. */
export async function completeClaudeSignIn(page: Page, email: string): Promise<void> {
  const flow = page.getByTestId('provider-login-flow-claude');
  await expect(flow).toHaveAttribute('data-phase', 'awaiting_code', { timeout: SIGNIN_STEP_MS });
  // An isolated boot has no browser to open, so even a page on the backend's
  // own machine gets the link to open itself and the code box.
  await expect(flow).toHaveAttribute('data-method', 'remote');
  const url = (await page.getByTestId('provider-login-url').inputValue()).trim();
  const state = new URL(url).searchParams.get('state');
  expect(state, `the sign-in link must carry its exchange state: ${url}`).toBeTruthy();
  await page.getByTestId('provider-login-code-input').fill(`${MOCK_CLAUDE_CODE}:${email}#${state}`);
  await page.getByRole('button', { name: 'Submit code' }).click();
  await expect(flow).toHaveCount(0, { timeout: SIGNIN_STEP_MS });
}
