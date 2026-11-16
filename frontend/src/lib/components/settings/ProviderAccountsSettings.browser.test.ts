// An account card's Active pill keeps its word whole however much room the
// card's controls take. The settings column wraps with
// `overflow-wrap: anywhere` (SettingsView), which lets a shrinkable flex item
// go narrower than a word, so the pill must not shrink: the name beside it
// truncates instead.
import { afterEach, beforeEach, expect, it } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/svelte';
import '../../../app.css';
import ProviderAccountsSettings from './ProviderAccountsSettings.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import { resetForTest as resetProviderAccountsForTest } from '../../stores/providerAccounts.svelte';
import { resetForTest as resetRateLimitsForTest, setProviderRateLimits } from '../../stores/rateLimitsInfo.svelte';

const DAY_MS = 24 * 3600_000;

function account(overrides: Record<string, unknown> = {}) {
  return {
    id: 'acct-1',
    provider: 'codex',
    email: 'mock@agent-overflow.test',
    orgName: 'e2e-workspace',
    addedAt: 1,
    active: true,
    generation: 1,
    needsLogin: false,
    signInRequired: false,
    autoSwitch: false,
    ...overrides,
  };
}

let host: HTMLDivElement;
beforeEach(() => {
  resetProviderAccountsForTest();
  host = document.createElement('div');
  document.body.append(host);
});
afterEach(() => {
  cleanup();
  host.remove();
  resetProviderAccountsForTest();
  resetRateLimitsForTest();
  setCompactLayoutForTest(false);
});

/** The lines a text node's characters sit on. */
function lineCount(text: Node): number {
  const range = document.createRange();
  range.selectNodeContents(text);
  return new Set([...range.getClientRects()].filter((r) => r.width > 0 && r.height > 0).map((r) => Math.round(r.top))).size;
}

// 380px is the phone panel at a 412px screen (SettingsView's compact:px-4).
it.each([
  ['a saved login that is gone', 380, { needsLogin: true }],
  ['a login about to expire', 380, { refreshTokenExpiresAt: Date.now() + 2 * DAY_MS }],
  ['a signed-in account', 380, {}],
  ['a login that is gone on a narrow phone', 320, { needsLogin: true }],
  ['a login that is gone on desktop', 768, { needsLogin: true }],
])('keeps "Active" on one line for %s at %ipx', async (_label, width, overrides) => {
  setBindingMock('ListProviderAccounts', async () => [account(overrides)]);
  setCompactLayoutForTest(width < 600);
  host.style.cssText = `width:${width}px;overflow-wrap:anywhere;background:var(--color-surface-0);color:var(--color-fg)`;
  const view = render(ProviderAccountsSettings, { target: host, props: { provider: 'codex' } });
  const card = await view.findByTestId('provider-account-acct-1');
  const pill = await waitFor(() => {
    const found = [...card.querySelectorAll('span')].find((el) => el.textContent?.trim() === 'Active');
    expect(found).toBeTruthy();
    return found!;
  });
  if ('needsLogin' in overrides || 'refreshTokenExpiresAt' in overrides) {
    expect(view.getByTestId('provider-account-sign-in-acct-1')).toBeTruthy();
  }
  const word = [...pill.childNodes].find((node) => node.nodeType === Node.TEXT_NODE && node.textContent?.trim())!;
  expect(lineCount(word)).toBe(1);
  // The name gives way instead, and nothing leaves the card.
  expect(card.scrollWidth).toBeLessThanOrEqual(card.clientWidth + 1);
});

// An email is how a person tells two saved logins apart, so on a phone the
// name takes the card's width and the controls move to a row beneath it.
const LONG_EMAIL = 'morgan.alvarez-kowalski@northwind-engineering.test';
// Each state lists its cards in order; every card is `acct-<position>`.
const ACCOUNT_STATES: [string, Record<string, unknown>[]][] = [
  ['a Codex login that is gone', [{ needsLogin: true }]],
  ['a Codex login with a long email that is gone', [{ needsLogin: true, email: LONG_EMAIL }]],
  ['a signed-in Codex account with a long email', [{ email: LONG_EMAIL }]],
  ['an inactive Codex account with usage shown', [{ email: LONG_EMAIL, active: false, usage: true }]],
  ['a Codex login about to expire', [{ refreshTokenExpiresAt: Date.now() + 2 * DAY_MS }]],
  [
    'several Claude accounts, one inactive and gone',
    [
      { provider: 'claude', email: LONG_EMAIL, orgName: 'Northwind', usage: true },
      { provider: 'claude', active: false, needsLogin: true, email: 'sam@northwind.test' },
    ],
  ],
];

function renderAccounts(width: number, rows: Record<string, unknown>[]) {
  const provider = (rows[0].provider as 'codex' | 'claude' | undefined) ?? 'codex';
  const accounts = rows.map(({ usage, ...overrides }, index) => {
    const id = `acct-${index + 1}`;
    if (usage) {
      setProviderRateLimits({
        provider,
        accountId: id,
        updatedAt: Date.now(),
        limits: [
          { limitId: 'primary', limitName: '', usedPercent: 42, windowMins: 300, resetsAt: Date.now() / 1000 + 3600 },
          { limitId: 'secondary', limitName: '', usedPercent: 7, windowMins: 10080, resetsAt: Date.now() / 1000 + DAY_MS / 1000 },
        ],
      });
    }
    return account({ id, provider, ...overrides });
  });
  setBindingMock('ListProviderAccounts', async () => accounts);
  setCompactLayoutForTest(width < 600);
  host.style.cssText = `width:${width}px;overflow-wrap:anywhere;background:var(--color-surface-0);color:var(--color-fg)`;
  return render(ProviderAccountsSettings, { target: host, props: { provider } });
}

/** The card's width inside its padding, which the name competes for. */
function innerWidth(card: HTMLElement): number {
  const style = getComputedStyle(card);
  return card.clientWidth - parseFloat(style.paddingLeft) - parseFloat(style.paddingRight);
}

it.each(ACCOUNT_STATES.flatMap(([label, rows]) => [320, 380].map((width) => [label, width, rows] as const)))(
  'gives the name the card width for %s at %ipx',
  async (_label, width, rows) => {
    const view = renderAccounts(width, rows);
    for (let index = 0; index < rows.length; index++) {
      const id = `acct-${index + 1}`;
      const card = await view.findByTestId(`provider-account-${id}`);
      const name = view.getByTestId(`provider-account-name-${id}`);
      const controls = view.getByTestId(`provider-account-controls-${id}`);
      expect(card.textContent?.includes('Usage not checked yet.')).toBe(!rows[index].usage);
      // A name that fits is shown whole; a longer one keeps most of the card
      // (the rest of its row is the grip, the status dot and the pill).
      expect(name.clientWidth).toBeGreaterThanOrEqual(Math.min(name.scrollWidth, innerWidth(card) * 0.6));
      // Its column runs to the card's edge, so the explanation lines below
      // it get the same width, and the controls sit on their own row
      // beneath, inside the card.
      const column = name.closest('button')!.getBoundingClientRect();
      const contentRight = card.getBoundingClientRect().right - parseFloat(getComputedStyle(card).paddingRight) - 1;
      expect(column.right).toBeCloseTo(contentRight, 0);
      expect(controls.getBoundingClientRect().top).toBeGreaterThanOrEqual(name.getBoundingClientRect().bottom);
      expect(controls.getBoundingClientRect().right).toBeLessThanOrEqual(card.getBoundingClientRect().right);
      expect(card.scrollWidth).toBeLessThanOrEqual(card.clientWidth + 1);
    }
  },
);

it('keeps the phone layout while an account is refreshing', async () => {
  setBindingMock('RefreshProviderAccountUsage', () => new Promise<void>(() => {}));
  const view = renderAccounts(380, [{ email: LONG_EMAIL }]);
  const card = await view.findByTestId('provider-account-acct-1');
  view.getByRole('button', { name: `Refresh usage for ${LONG_EMAIL}` }).click();
  await waitFor(() => expect(card.querySelector('.animate-spin')).toBeTruthy());
  const name = view.getByTestId('provider-account-name-acct-1');
  expect(name.clientWidth).toBeGreaterThanOrEqual(innerWidth(card) * 0.6);
  expect(view.getByTestId('provider-account-controls-acct-1').getBoundingClientRect().top)
    .toBeGreaterThanOrEqual(name.getBoundingClientRect().bottom);
});

// Desktop keeps one row: the name column on the left, the controls beside it
// at the top right.
it.each(ACCOUNT_STATES.flatMap(([label, rows]) => [768, 1024].map((width) => [label, width, rows] as const)))(
  'keeps the controls beside the name for %s at %ipx',
  async (_label, width, rows) => {
    const view = renderAccounts(width, rows);
    for (let index = 0; index < rows.length; index++) {
      const id = `acct-${index + 1}`;
      const card = await view.findByTestId(`provider-account-${id}`);
      const name = view.getByTestId(`provider-account-name-${id}`).getBoundingClientRect();
      const controls = view.getByTestId(`provider-account-controls-${id}`).getBoundingClientRect();
      const cardBox = card.getBoundingClientRect();
      const padding = parseFloat(getComputedStyle(card).paddingTop);
      expect(controls.top).toBeCloseTo(cardBox.top + padding + 1, 0);
      expect(controls.left).toBeGreaterThanOrEqual(name.right);
      expect(cardBox.right - controls.right).toBeCloseTo(parseFloat(getComputedStyle(card).paddingRight) + 1, 0);
    }
  },
);
