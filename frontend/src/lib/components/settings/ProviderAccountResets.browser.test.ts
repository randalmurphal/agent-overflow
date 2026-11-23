// The resets menu from the keyboard, in a real browser: the button opens the
// menu with focus on its first line, the arrows move between lines, Enter on
// a Use line opens the confirm and Enter on Finish sends it. Escape, a
// cancelled confirm and Finish each leave focus on the button.
import { afterEach, beforeEach, expect, it } from 'vitest';
import { page, userEvent } from 'vitest/browser';
import { cleanup, render, waitFor } from '@testing-library/svelte';
import '../../../app.css';
import ProviderAccountResets from './ProviderAccountResets.svelte';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';
import { account } from '../../../test/helpers/providerAccounts';
import { resetForTest as resetProviderAccounts } from '../../stores/providerAccounts.svelte';
import type { ManagedProviderAccount, ProviderAccountResetOption } from '../../stores/bindings';
import { HOME_BACKEND } from '../../transport/backendKey';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';

function option(overrides: Partial<ProviderAccountResetOption>): ProviderAccountResetOption {
  return { id: '', kind: 'full', label: 'Full reset', refills: '', expiresAt: 0, usable: true, ...overrides } as ProviderAccountResetOption;
}

const TWO_KINDS = [option({ id: 'a' }), option({ id: 'b', kind: 'weekly', label: 'Weekly reset' })];
const UNSETTLED = { id: 'spend-1', label: 'Full reset', refills: '', startedAt: 1_800_000_000_000 };

function renderResets(extra: Partial<ManagedProviderAccount> = {}) {
  const card = account({
    id: 'codex-a', provider: 'codex', email: 'a@example.com',
    usageResets: { revision: 4, options: TWO_KINDS },
    ...extra,
  } as Partial<ManagedProviderAccount>);
  const view = render(ProviderAccountResets, { props: { account: card, backend: HOME_BACKEND, admin: true, credentialOpInFlight: false } });
  return { view, trigger: view.getByTestId('provider-account-resets-codex-a') };
}

const menu = () => document.querySelector('[data-testid="provider-account-resets-list-codex-a"]');
const focusedLabel = () => document.activeElement?.textContent?.replace(/\s+/g, ' ').trim() ?? '';

beforeEach(() => {
  resetProviderAccounts();
  setBindingMock('ListProviderAccounts', async () => []);
  setBindingMock('SpendProviderAccountReset', async () => ({ message: 'Reset used.', spent: true, readingLanded: true }));
  setBindingMock('FinishProviderAccountReset', async () => ({ message: 'Reset used.', spent: true, readingLanded: true }));
});
afterEach(() => {
  cleanup();
  resetProviderAccounts();
});

async function openFromKeyboard(trigger: HTMLElement): Promise<void> {
  trigger.focus();
  await userEvent.keyboard('{Enter}');
  await waitFor(() => expect(menu()).not.toBeNull());
  await waitFor(() => expect(document.activeElement?.getAttribute('role')).toBe('menuitem'));
}

it('opens from the button, moves between lines with the arrows, and opens the confirm with Enter', async () => {
  const { trigger } = renderResets();
  await openFromKeyboard(trigger);
  expect(focusedLabel()).toMatch(/^Full reset/);
  await userEvent.keyboard('{ArrowDown}');
  expect(focusedLabel()).toMatch(/^Weekly reset/);
  await userEvent.keyboard('{ArrowDown}');
  expect(focusedLabel(), 'the arrows wrap').toMatch(/^Full reset/);
  await userEvent.keyboard('{ArrowUp}');
  await userEvent.keyboard('{Enter}');
  const dialog = await waitFor(() => {
    const el = document.querySelector('[role="dialog"]');
    if (!el) throw new Error('no confirm');
    return el;
  });
  expect(dialog.textContent).toContain('Use a Weekly reset?');
  expect(menu()).toBeNull();
  expect(getBindingMock('SpendProviderAccountReset')).not.toHaveBeenCalled();
});

it('closes on Escape with focus back on the button', async () => {
  const { trigger } = renderResets();
  await openFromKeyboard(trigger);
  await userEvent.keyboard('{Escape}');
  await waitFor(() => expect(menu()).toBeNull());
  expect(document.activeElement).toBe(trigger);
});

it('puts focus back on the button when the confirm is cancelled', async () => {
  const { trigger } = renderResets();
  await openFromKeyboard(trigger);
  await userEvent.keyboard('{Enter}');
  await waitFor(() => expect(document.querySelector('[role="dialog"]')).not.toBeNull());
  await userEvent.keyboard('{Escape}');
  await waitFor(() => expect(document.querySelector('[role="dialog"]')).toBeNull());
  await waitFor(() => expect(document.activeElement).toBe(trigger));
  expect(getBindingMock('SpendProviderAccountReset')).not.toHaveBeenCalled();
});

// Codex's reason, the unconfirmed line's explanation and the unlisted line
// are shown whole: they wrap inside a menu that stays on a phone's screen.
it('wraps long lines whole inside a menu that fits a phone screen', async () => {
  await page.viewport(360, 700);
  setCompactLayoutForTest(true);
  try {
    const reason = 'Codex is still applying an earlier reset to this account, so this one can be used once that finishes.';
    const { trigger } = renderResets({
      pendingReset: UNSETTLED,
      usageResets: { revision: 4, options: [option({ id: 'a', usable: false, reason })], unlisted: 2 },
    } as Partial<ManagedProviderAccount>);
    trigger.click();
    await waitFor(() => expect(menu()).not.toBeNull());
    await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    const box = menu()!.getBoundingClientRect();
    expect(box.left).toBeGreaterThanOrEqual(0);
    expect(box.right).toBeLessThanOrEqual(360);
    expect(menu()!.textContent).toContain(reason);
    const lines = [...menu()!.querySelectorAll<HTMLElement>('[role="menuitem"] span')];
    expect(lines.length).toBeGreaterThan(0);
    for (const el of lines) {
      expect(el.scrollWidth, `"${el.textContent?.slice(0, 40)}" is whole`).toBeLessThanOrEqual(el.clientWidth + 1);
    }
  } finally {
    setCompactLayoutForTest(false);
  }
});

it('sends Finish for the unconfirmed reset with Enter and leaves focus on the button', async () => {
  const { trigger } = renderResets({ pendingReset: UNSETTLED } as Partial<ManagedProviderAccount>);
  await openFromKeyboard(trigger);
  expect(focusedLabel()).toMatch(/^Full reset not confirmed/);
  // No Use while a reset is unconfirmed: Finish is the only line to land on.
  await userEvent.keyboard('{ArrowDown}');
  expect(focusedLabel()).toMatch(/^Full reset not confirmed/);
  await userEvent.keyboard('{Enter}');
  await waitFor(() => expect(getBindingMock('FinishProviderAccountReset')).toHaveBeenCalledWith('codex', 'codex-a', 'spend-1'));
  await waitFor(() => expect(menu()).toBeNull());
  expect(document.activeElement).toBe(trigger);
});
