import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import AccountSwitcher from './AccountSwitcher.svelte';
import {
  resetForTest as resetProviderAccounts,
} from '../../stores/providerAccounts.svelte';
import { resetForTest as resetAccountInfo } from '../../stores/accountInfo.svelte';
import { resetForTest as resetRateLimits } from '../../stores/rateLimitsInfo.svelte';
import { getToasts, removeToast } from '../../stores/toast.svelte';
import {
  getSettingsSection,
  isSettingsOpen,
  resetSettingsOverlayForTest,
} from '../../stores/settingsOverlay.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { account } from '../../../test/helpers/providerAccounts';
import type { ManagedProviderAccount } from '../../stores/bindings';

const LISTING: ManagedProviderAccount[] = [
  account({ id: 'claude-a', displayName: 'Work', active: true }),
  account({ id: 'claude-b', displayName: 'Personal' }),
  account({ id: 'codex-a', provider: 'codex', displayName: 'Codex Team' }),
];

beforeEach(() => {
  for (const toast of getToasts()) removeToast(toast.id);
  resetBindingMocks();
  resetProviderAccounts();
  resetAccountInfo();
  resetRateLimits();
  setBindingMock('ListProviderAccounts', async () => LISTING);
});

afterEach(() => {
  resetProviderAccounts();
  resetAccountInfo();
  resetRateLimits();
  vi.restoreAllMocks();
});

describe('<AccountSwitcher> — visibility', () => {
  it('renders nothing while closed and does not load', () => {
    const listMock = setBindingMock('ListProviderAccounts', async () => LISTING);
    const { queryByTestId } = render(AccountSwitcher, { open: false, onClose: () => {} });

    expect(queryByTestId('account-switcher')).toBeNull();
    expect(listMock).not.toHaveBeenCalled();
  });

  it('loads a fresh listing when opened and sections it per provider', async () => {
    const listMock = setBindingMock('ListProviderAccounts', async () => LISTING);
    const { findByTestId, getByTestId } = render(AccountSwitcher, {
      open: true,
      onClose: () => {},
    });

    await findByTestId('account-switcher-group-claude');
    expect(listMock).toHaveBeenCalledOnce();
    expect(getByTestId('account-switcher-group-codex')).toBeTruthy();
    expect(getByTestId('account-switcher-row-claude-a').getAttribute('data-active')).toBe('true');
    expect(getByTestId('account-switcher-row-claude-b').getAttribute('data-active')).toBe('false');
  });

  it('offers a route into provider settings when nothing is saved', async () => {
    setBindingMock('ListProviderAccounts', async () => []);
    resetSettingsOverlayForTest();
    const onClose = vi.fn();
    const { findByTestId } = render(AccountSwitcher, { open: true, onClose });

    const empty = await findByTestId('account-switcher-empty');
    await fireEvent.click(empty.querySelector('button') as HTMLButtonElement);

    expect(onClose).toHaveBeenCalled();
    expect(isSettingsOpen()).toBe(true);
    expect(getSettingsSection()).toBe('providers');
  });
});

describe('<AccountSwitcher> — selection', () => {
  it('switches and closes when a non-active account is picked', async () => {
    const switchMock = setBindingMock('SwitchProviderAccount', async () => undefined);
    const onClose = vi.fn();
    const { findByTestId } = render(AccountSwitcher, { open: true, onClose });

    const row = await findByTestId('account-switcher-row-claude-b');
    await fireEvent.click(row.querySelector('button') as HTMLButtonElement);

    await waitFor(() => expect(switchMock).toHaveBeenCalledWith('claude', 'claude-b'));
    await waitFor(() => expect(onClose).toHaveBeenCalled());
  });

  it('stays open when the switch fails, with the error surfaced', async () => {
    setBindingMock('SwitchProviderAccount', async () => {
      throw new Error('credential slot locked');
    });
    const onClose = vi.fn();
    const { findByTestId } = render(AccountSwitcher, { open: true, onClose });

    const row = await findByTestId('account-switcher-row-claude-b');
    await fireEvent.click(row.querySelector('button') as HTMLButtonElement);

    await waitFor(() =>
      expect(getToasts().some((t) => t.message.includes('Credential slot locked'))).toBe(true),
    );
    expect(onClose).not.toHaveBeenCalled();
  });

  it('starts a sign-in instead of a switch for a needs-login account, and shows its link', async () => {
    setBindingMock('ListProviderAccounts', async () => [
      account({ id: 'claude-a', displayName: 'Work', active: true }),
      account({ id: 'claude-b', displayName: 'Stale', needsLogin: true }),
    ]);
    const loginMock = setBindingMock('StartProviderLogin', async () => ({
      provider: 'claude',
      phase: 'awaiting_code',
      method: 'remote',
      authorizeUrl: 'https://claude.ai/oauth/authorize?state=abc',
    }));
    const switchMock = setBindingMock('SwitchProviderAccount', async () => undefined);
    const onClose = vi.fn();
    const { findByTestId } = render(AccountSwitcher, { open: true, onClose });

    const row = await findByTestId('account-switcher-row-claude-b');
    await fireEvent.click(row.querySelector('button') as HTMLButtonElement);

    await waitFor(() => expect(loginMock).toHaveBeenCalledWith('claude', expect.any(String)));
    // The picker stays open around the flow: the link it just produced is the
    // only thing that can finish the sign-in.
    const flow = await findByTestId('provider-login-flow-claude');
    expect((flow.querySelector('[data-testid="provider-login-url"]') as HTMLInputElement).value)
      .toBe('https://claude.ai/oauth/authorize?state=abc');
    expect(switchMock).not.toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();
  });

  it('closes without an RPC when the already-active account is picked', async () => {
    const switchMock = setBindingMock('SwitchProviderAccount', async () => undefined);
    const onClose = vi.fn();
    const { findByTestId } = render(AccountSwitcher, { open: true, onClose });

    const row = await findByTestId('account-switcher-row-claude-a');
    await fireEvent.click(row.querySelector('button') as HTMLButtonElement);

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(switchMock).not.toHaveBeenCalled();
  });

  it('refreshes one account’s usage without leaving the picker', async () => {
    const refreshMock = setBindingMock('RefreshProviderAccountUsage', async () => undefined);
    const onClose = vi.fn();
    const { findByTestId } = render(AccountSwitcher, { open: true, onClose });

    await fireEvent.click(await findByTestId('account-switcher-refresh-claude-b'));

    await waitFor(() => expect(refreshMock).toHaveBeenCalledWith('claude', 'claude-b'));
    expect(onClose).not.toHaveBeenCalled();
  });
});

describe('<AccountSwitcher> — keyboard', () => {
  it('walks every provider’s rows with the arrow keys and selects with Enter', async () => {
    const switchMock = setBindingMock('SwitchProviderAccount', async () => undefined);
    const { findByTestId, getByTestId } = render(AccountSwitcher, {
      open: true,
      onClose: () => {},
    });

    await findByTestId('account-switcher-row-codex-a');
    const list = getByTestId('account-switcher');
    // claude-a (0) → claude-b (1) → codex-a (2): arrows cross the section
    // boundary because the index is flat across groups.
    await fireEvent.keyDown(list, { key: 'ArrowDown' });
    await fireEvent.keyDown(list, { key: 'ArrowDown' });
    expect(
      getByTestId('account-switcher-row-codex-a')
        .querySelector('button')
        ?.getAttribute('aria-current'),
    ).toBe('true');

    await fireEvent.keyDown(list, { key: 'Enter' });
    await waitFor(() => expect(switchMock).toHaveBeenCalledWith('codex', 'codex-a'));
  });

  it('leaves Enter to whatever is Tab-focused inside the list', async () => {
    // The highlight stops tracking DOM focus the moment Tab moves into a row,
    // so a list-level Enter handler would switch the account the highlight
    // points at — not the one the user is standing on — and preventDefault
    // would eat the focused button's own activation.
    const switchMock = setBindingMock('SwitchProviderAccount', async () => undefined);
    const refreshMock = setBindingMock('RefreshProviderAccountUsage', async () => undefined);
    const { findByTestId, getByTestId } = render(AccountSwitcher, {
      open: true,
      onClose: () => {},
    });

    // Park the highlight on a switchable row, then Tab away from it: the two
    // now disagree, which is exactly when acting on the highlight is wrong.
    await findByTestId('account-switcher-row-codex-a');
    await fireEvent.mouseEnter(
      getByTestId('account-switcher-row-claude-b').querySelector('button') as HTMLButtonElement,
    );
    const refresh = getByTestId('account-switcher-refresh-codex-a') as HTMLButtonElement;
    refresh.focus();
    await fireEvent.keyDown(refresh, { key: 'Enter', bubbles: true });

    expect(switchMock).not.toHaveBeenCalled();
    // The button's own activation still works.
    await fireEvent.click(refresh);
    await waitFor(() => expect(refreshMock).toHaveBeenCalledWith('codex', 'codex-a'));
  });

  it('mirrors the arrows with j and k', async () => {
    const { findByTestId, getByTestId } = render(AccountSwitcher, {
      open: true,
      onClose: () => {},
    });

    await findByTestId('account-switcher-row-codex-a');
    const list = getByTestId('account-switcher');
    await fireEvent.keyDown(list, { key: 'j' });
    expect(
      getByTestId('account-switcher-row-claude-b')
        .querySelector('button')
        ?.getAttribute('aria-current'),
    ).toBe('true');

    await fireEvent.keyDown(list, { key: 'k' });
    expect(
      getByTestId('account-switcher-row-claude-a')
        .querySelector('button')
        ?.getAttribute('aria-current'),
    ).toBe('true');
  });

  it('wraps from the first row to the last on ArrowUp', async () => {
    const { findByTestId, getByTestId } = render(AccountSwitcher, {
      open: true,
      onClose: () => {},
    });

    await findByTestId('account-switcher-row-codex-a');
    const list = getByTestId('account-switcher');
    await fireEvent.keyDown(list, { key: 'ArrowUp' });

    expect(
      getByTestId('account-switcher-row-codex-a')
        .querySelector('button')
        ?.getAttribute('aria-current'),
    ).toBe('true');
  });
});
