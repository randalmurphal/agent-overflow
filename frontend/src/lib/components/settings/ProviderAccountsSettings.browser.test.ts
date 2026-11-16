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
