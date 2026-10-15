// Claude sign-ins and the saved-account order, through the shipped Settings
// page, the real backend and the mock provider's sign-in wire.
//
// A sign-in lands on the account its login belongs to and never switches: a
// new login is added below the others, a saved account's login is renewed in
// place, and only the selected account's renewal replaces the canonical
// credential. Each landing is named in a toast. The first sign-in on a machine
// that already has a native Claude login adopts that login first, as the
// selected account. Cards stay in the order the user keeps them; a drag or the
// arrow keys on a card's grip move it, the order survives a reload, and the
// Active tag stays on the selected card wherever it moves.
//
// Owns its backend: it adopts provider accounts, which is process-wide state
// no other spec should inherit. The cases are one sequence on one machine.

import { expect, test, type Page } from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';
import {
  SIGNIN_STEP_MS,
  cardOrder as drawnOrder,
  claudeAccountId,
  claudeAccounts as savedClaudeAccounts,
  claudeAccountsSection,
  completeClaudeSignIn,
  openClaudeAccounts as openAccounts,
  seedNativeClaudeLogin,
} from './provider-account-helpers.js';

const ACTIVE_EMAIL = 'active@agent-overflow.test';
const SECOND_EMAIL = 'second@agent-overflow.test';

let harness: HarnessApp;
let page: Page;
const consoleErrors: string[] = [];

const claudeAccounts = () => savedClaudeAccounts(harness);
const accountId = (email: string) => claudeAccountId(harness, email);
const toast = (text: string) => page.getByTestId('toast').filter({ hasText: text });
const section = () => claudeAccountsSection(page);
const cardOrder = () => drawnOrder(page);
const openClaudeAccounts = () => openAccounts(page);
const completeSignIn = (email: string) => completeClaudeSignIn(page, email);

test.describe.serial('Claude sign-in lands without switching, in the order the user keeps', () => {
  test.beforeAll(async ({ browser }) => {
    harness = await launchHarness();
    // A native Claude login already on this machine, five days from the end
    // of its session.
    await seedNativeClaudeLogin(harness, ACTIVE_EMAIL, Date.now() + 5 * 24 * 3600_000);
    page = await browser.newPage();
    page.on('console', (message) => {
      if (message.type() === 'error') consoleErrors.push(message.text());
    });
    await harness.open(page);
    await openClaudeAccounts();
  });

  test.afterAll(async () => {
    await page?.close();
    await harness?.close();
  });

  test('a sign-in for a new account adds it below the selected one', async () => {
    await section().getByRole('button', { name: 'Log in to another account' }).click();

    // Starting a sign-in saves the native login it would otherwise replace,
    // as the selected account.
    await expect(toast(`Added Claude account ${ACTIVE_EMAIL}.`)).toBeVisible({ timeout: SIGNIN_STEP_MS });
    await completeSignIn(SECOND_EMAIL);
    await expect(toast(`Added Claude account ${SECOND_EMAIL}.`)).toBeVisible();

    const activeId = await accountId(ACTIVE_EMAIL);
    const secondId = await accountId(SECOND_EMAIL);
    const saved = await claudeAccounts();
    expect(saved.map((account) => account.id)).toEqual([activeId, secondId]);
    expect(saved.find((account) => account.active)?.id, 'a sign-in must not switch').toBe(activeId);
    await expect.poll(cardOrder).toEqual([activeId, secondId]);
    await expect(section().getByTestId(`provider-account-${activeId}`)).toContainText('Active');
    await expect(section().getByTestId(`provider-account-${secondId}`)).not.toContainText('Active');
  });

  test('Sign in again is offered in the last week and renews whichever account signs in', async () => {
    const activeId = await accountId(ACTIVE_EMAIL);
    const secondId = await accountId(SECOND_EMAIL);
    const activeCard = section().getByTestId(`provider-account-${activeId}`);
    await expect(activeCard.getByTestId(`provider-account-login-expiry-${activeId}`)).toHaveText(/^Login expires in 4d 2\dh$/);
    await expect(section().getByTestId(`provider-account-sign-in-${secondId}`)).toHaveCount(0);
    const secondExpiry = (await claudeAccounts()).find((account) => account.id === secondId)?.refreshTokenExpiresAt;

    // The button on the selected card, answered with the other account's
    // login: that account is renewed, and nothing switches.
    await section().getByTestId(`provider-account-sign-in-${activeId}`).click();
    await completeSignIn(SECOND_EMAIL);
    await expect(toast(`Renewed the Claude sign-in for ${SECOND_EMAIL}.`)).toBeVisible();
    let saved = await claudeAccounts();
    expect(saved.find((account) => account.active)?.id).toBe(activeId);
    expect(saved.find((account) => account.id === secondId)?.refreshTokenExpiresAt).toBeGreaterThan(secondExpiry ?? 0);
    await expect(activeCard.getByTestId(`provider-account-login-expiry-${activeId}`)).toBeVisible();

    // Answered with the selected account's own login: its session restarts,
    // so the warning and the button go away.
    await section().getByTestId(`provider-account-sign-in-${activeId}`).click();
    await completeSignIn(ACTIVE_EMAIL);
    await expect(toast(`Renewed the Claude sign-in for ${ACTIVE_EMAIL}.`)).toBeVisible();
    await expect(activeCard.getByTestId(`provider-account-login-expiry-${activeId}`)).toHaveCount(0);
    await expect(section().getByTestId(`provider-account-sign-in-${activeId}`)).toHaveCount(0);
    saved = await claudeAccounts();
    expect(saved.map((account) => account.id)).toEqual([activeId, secondId]);
    expect(saved.find((account) => account.active)?.id).toBe(activeId);
  });

  test('a dragged card keeps its new place across a reload, and the Active tag goes with its card', async () => {
    const activeId = await accountId(ACTIVE_EMAIL);
    const secondId = await accountId(SECOND_EMAIL);
    const grip = section().getByTestId(`provider-account-reorder-${secondId}`);
    const target = section().getByTestId(`provider-account-${activeId}`);
    // Both cards on screen, so the drag stays clear of the edges that scroll.
    await section().evaluate((node) => node.scrollIntoView({ block: 'center' }));
    const from = (await grip.boundingBox())!;
    const over = (await target.boundingBox())!;
    await page.mouse.move(from.x + from.width / 2, from.y + from.height / 2);
    await page.mouse.down();
    await page.mouse.move(from.x + from.width / 2, over.y + 4, { steps: 12 });
    // While held, the other card has already slid out of the way.
    await expect.poll(async () => target.evaluate((card) => getComputedStyle(card).transform)).not.toBe('none');
    await page.mouse.up();

    await expect.poll(cardOrder).toEqual([secondId, activeId]);
    await expect.poll(async () => (await claudeAccounts()).map((account) => account.id)).toEqual([secondId, activeId]);
    expect((await claudeAccounts()).find((account) => account.active)?.id, 'reordering must not switch').toBe(activeId);
    await expect(section().getByTestId(`provider-account-${activeId}`)).toContainText('Active');

    await page.reload();
    await openClaudeAccounts();
    await expect.poll(cardOrder).toEqual([secondId, activeId]);
  });

  test('the arrow keys on a grip move its card one place', async () => {
    const activeId = await accountId(ACTIVE_EMAIL);
    const secondId = await accountId(SECOND_EMAIL);
    const grip = section().getByTestId(`provider-account-reorder-${secondId}`);
    await grip.focus();
    await page.keyboard.press('ArrowDown');

    await expect.poll(cardOrder).toEqual([activeId, secondId]);
    await expect.poll(async () => (await claudeAccounts()).map((account) => account.id)).toEqual([activeId, secondId]);
    // Focus follows the card, so the next press moves it again.
    await expect(grip).toBeFocused();
    await page.keyboard.press('ArrowUp');
    await expect.poll(cardOrder).toEqual([secondId, activeId]);
  });

  test('nothing above surfaced an error', async () => {
    await expect(page.getByTestId('toast').filter({ hasText: /not reordered|failed|Failed/ })).toHaveCount(0);
    expect(consoleErrors).toEqual([]);
  });
});
