// The Codex usage resets flow both layouts run: the harness seeds earned
// resets for the mock Codex account (HarnessSetCodexResetCredits), a usage
// refresh reads them through the real account probe onto the card's
// "Usage resets (N)" button, and Use in its dropdown spends one behind a
// confirm through the real consume. A spend whose answer is lost stays
// unconfirmed on the card until Finish sends the same key again, which
// spends nothing more. The account switcher shows the same button on the
// account's row, and its last reset is spent there from the keyboard.

import { expect, type Locator, type Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { completeCodexSignIn } from './provider-account-helpers.js';

const ACCOUNT_ID = 'mock-workspace';
const DAY_S = 24 * 3600;

interface FakeAccount {
  credits: { id: string; status: string; grantedAt: number; expiresAt?: number }[];
  failNextConsume?: boolean;
  redeemed?: Record<string, string>;
  consumes?: number;
  usedPercent?: number;
}

export interface ResetsLayout {
  /** Checks the open dropdown's geometry against its trigger, on both screens. */
  checkDropdown?: (trigger: Locator, list: Locator) => Promise<void>;
}

function codexCard(page: Page) {
  return page.getByTestId('provider-accounts-codex').locator('[data-account-card]');
}

async function refreshUsage(page: Page): Promise<void> {
  const refresh = codexCard(page).getByRole('button', { name: /^Refresh usage for / });
  await expect(refresh).toBeEnabled();
  await refresh.click();
  await expect(refresh).toBeEnabled();
}

// The reset confirm; the Settings view around it is a dialog too.
function confirmDialog(page: Page) {
  return page.getByRole('dialog').filter({ hasText: 'Use one Full reset on' });
}

function toast(page: Page, text: string) {
  return page.getByTestId('toast-message').filter({ hasText: text });
}

export async function runCodexUsageResets(harness: HarnessApp, page: Page, layout: ResetsLayout = {}): Promise<void> {
  const fake = () => harness.rpc<FakeAccount>('HarnessCodexResetCredits', ACCOUNT_ID);
  await harness.open(page);
  await page.getByTestId('sidebar-settings-button').click();
  await page.getByRole('tablist', { name: 'Settings Sections' }).getByRole('tab', { name: 'Codex', exact: true }).click();
  await completeCodexSignIn(harness, page);
  await expect(codexCard(page)).toHaveCount(1);
  const triggerName = /^provider-account-resets-(?!list|box)/;
  const trigger = codexCard(page).getByTestId(triggerName);
  const list = page.getByTestId(/^provider-account-resets-list-/);
  await expect(trigger).toHaveCount(0);

  const now = Math.floor(Date.now() / 1000);
  await harness.rpc('HarnessSetCodexResetCredits', {
    accountId: ACCOUNT_ID,
    account: {
      credits: [
        { id: 'RateLimitResetCredit_late', status: 'available', grantedAt: now, expiresAt: now + 20 * DAY_S },
        { id: 'RateLimitResetCredit_soon', status: 'available', grantedAt: now, expiresAt: now + 10 * DAY_S },
        { id: 'RateLimitResetCredit_last', status: 'available', grantedAt: now, expiresAt: now + 30 * DAY_S },
      ],
      usedPercent: 100,
    },
  });
  await refreshUsage(page);
  await expect(trigger).toHaveText('Usage resets (3)');

  const open = async () => {
    await trigger.click();
    await expect(list).toHaveCount(1);
    await layout.checkDropdown?.(trigger, list);
  };
  const use = async () => {
    await open();
    await expect(list).toContainText(`${await usable()} left`);
    await list.getByRole('button', { name: /^Use a Full reset on / }).click();
    await expect(list).toHaveCount(0);
    const dialog = confirmDialog(page);
    await expect(dialog).toHaveCount(1);
    await expect(dialog).toContainText('Refills your current Codex usage limits.');
    await dialog.getByRole('button', { name: 'Use reset' }).click();
  };
  const usable = async () => Number((await trigger.textContent())?.match(/\((\d+)\)/)?.[1]);

  // Escape closes the dropdown, and Cancel in the confirm spends nothing.
  await open();
  await page.keyboard.press('Escape');
  await expect(list).toHaveCount(0);
  await open();
  await list.getByRole('button', { name: /^Use a Full reset on / }).click();
  await confirmDialog(page).getByRole('button', { name: 'Cancel' }).click();
  await expect(confirmDialog(page)).toHaveCount(0);
  expect((await fake()).consumes ?? 0).toBe(0);

  await use();
  await expect(toast(page, 'Reset used. Your Codex usage limits are refilled.')).toHaveCount(1);
  await expect(trigger).toHaveText('Usage resets (2)');
  const afterFirst = await fake();
  expect(afterFirst.consumes).toBe(1);
  expect(Object.values(afterFirst.redeemed ?? {})).toEqual(['RateLimitResetCredit_soon']);

  // The next answer is lost after the credit is spent. The card then shows
  // the reset as unconfirmed, offers Finish and no Use, and Finish sends the
  // same key, so the account loses one reset, not two.
  await harness.rpc('HarnessSetCodexResetCredits', { accountId: ACCOUNT_ID, account: { ...afterFirst, failNextConsume: true } });
  await use();
  await expect(toast(page, 'Codex did not confirm the reset. Press Finish on the account to complete it')).toHaveCount(1);
  await expect(trigger).toHaveAttribute('data-unsettled', 'true');
  await open();
  await expect(list.getByTestId('provider-account-reset-unsettled')).toContainText('Full reset not confirmed');
  await expect(list.getByRole('button', { name: /^Use a Full reset on / })).toHaveCount(0);
  await list.getByRole('button', { name: /^Finish the Full reset on / }).click();
  await expect(toast(page, 'This reset was already used')).toHaveCount(1);
  await expect(trigger).not.toHaveAttribute('data-unsettled', 'true');
  await expect(trigger).toHaveText('Usage resets (1)');
  const afterFinish = await fake();
  expect(afterFinish.consumes).toBe(3);
  expect(Object.keys(afterFinish.redeemed ?? {})).toHaveLength(2);
  expect(afterFinish.credits.filter((credit) => credit.status === 'available').map((credit) => credit.id))
    .toEqual(['RateLimitResetCredit_last']);

  // The switcher's row has the same button. Enter opens its menu on the one
  // Use line and Enter there opens the confirm; spending the last reset
  // takes the button away.
  await page.getByRole('button', { name: 'Close Settings' }).click();
  await page.keyboard.press('ControlOrMeta+Shift+K');
  const palette = page.getByTestId('command-palette-input');
  await palette.fill('Switch Account');
  await page.getByRole('option', { name: /Provider: Switch Account/ }).click();
  // The row of the one saved Codex account.
  const row = page.getByTestId('account-switcher-group-codex').getByTestId(/^account-switcher-row-/);
  const rowTrigger = row.getByTestId(triggerName);
  await expect(rowTrigger).toHaveText('Usage resets (1)');
  // The switcher focuses its list as it opens; the button is focused after.
  await expect(page.getByTestId('account-switcher')).toBeFocused();
  await rowTrigger.focus();
  await expect(rowTrigger).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(list).toHaveCount(1);
  await layout.checkDropdown?.(rowTrigger, list);
  await expect(list.getByRole('menuitem').first()).toBeFocused();
  await page.keyboard.press('Enter');
  const dialog = confirmDialog(page);
  await expect(dialog).toHaveCount(1);
  await dialog.getByRole('button', { name: 'Use reset' }).click();
  await expect(toast(page, 'Reset used. Your Codex usage limits are refilled.')).toHaveCount(2);
  await expect(rowTrigger).toHaveCount(0);
  await expect(row.getByTestId(/^provider-account-resets-box-/)).toHaveCount(1);
  const afterSwitcher = await fake();
  expect(afterSwitcher.consumes).toBe(4);
  expect(Object.values(afterSwitcher.redeemed ?? {})).toHaveLength(3);
}
