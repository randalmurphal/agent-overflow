// Settings → Network → Devices → Passkeys.
//
// The cases worth pinning are the ones where the block has to say
// something true that a simpler list would get wrong: a credential that
// outlived its domain, a counter anomaly, and a removal that ends no
// session. The registration ceremony itself is passkey.test.ts's subject.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { setPasskeysAvailableFromBootstrap } from '../../transport/passkey';
import PasskeysBlock from './PasskeysBlock.svelte';

function passkey(overrides: Record<string, unknown> = {}) {
  return {
    id: 'pk-1',
    label: 'iPhone',
    createdAtMs: Date.now() - 86_400_000,
    lastUsedAtMs: Date.now() - 3_600_000,
    relyingPartyId: 'ao.example.com',
    usable: true,
    cloneWarning: false,
    backedUp: true,
    transports: ['internal', 'hybrid'],
    ...overrides,
  };
}

/** A page that can run a ceremony, so `usable` is true. */
function installAuthenticator(): void {
  Object.defineProperty(window, 'PublicKeyCredential', {
    configurable: true,
    value: function PublicKeyCredential() {},
  });
  Object.defineProperty(navigator, 'credentials', {
    configurable: true,
    value: { create: () => Promise.resolve(null), get: () => Promise.resolve(null) },
  });
  setPasskeysAvailableFromBootstrap(true);
}

beforeEach(() => {
  resetBindingMocks();
  installAuthenticator();
});

afterEach(() => {
  cleanup();
  resetBindingMocks();
  setPasskeysAvailableFromBootstrap(false);
  Reflect.deleteProperty(window, 'PublicKeyCredential');
  Reflect.deleteProperty(navigator, 'credentials');
  vi.restoreAllMocks();
});

describe('<PasskeysBlock>', () => {
  it('says removing one signs nothing out, because it does not', async () => {
    setBindingMock('ListPasskeys', async () => []);
    const { findByText } = render(PasskeysBlock);
    // The sentence is load-bearing: the control sits beside device revokes
    // that DO sign a device out, and a person reading the two together
    // would otherwise assume this one does too.
    await findByText(/Removing one does not sign any device out/);
  });

  it('lists a credential with what it is, not with its bytes', async () => {
    setBindingMock('ListPasskeys', async () => [passkey()]);
    const { findByText, queryByTestId } = render(PasskeysBlock);
    await findByText('iPhone');
    expect(queryByTestId('passkey-clone-warning')).toBeNull();
  });

  it('keeps a credential that outlived its domain, and says why it is inert', async () => {
    setBindingMock('ListPasskeys', async () => [
      passkey({ usable: false, relyingPartyId: 'old.example.com' }),
    ]);
    const { findByText } = render(PasskeysBlock);
    // Listed rather than hidden: the authenticator still offers it, so
    // hiding it would leave somebody unable to remove what they can see.
    await findByText(/Registered for old\.example\.com/);
  });

  it('shows a stalled counter as an anomaly, never as a refusal', async () => {
    setBindingMock('ListPasskeys', async () => [passkey({ cloneWarning: true })]);
    const { findByTestId, findByText } = render(PasskeysBlock);
    const warning = await findByTestId('passkey-clone-warning');
    expect(warning.textContent).toMatch(/It still works/);
    // And the credential is still a normal row with a normal control.
    await findByText('iPhone');
  });

  it('removes on the second click and never on the first', async () => {
    setBindingMock('ListPasskeys', async () => [passkey()]);
    const removed = setBindingMock('DeletePasskey', async () => true);
    const { findByRole } = render(PasskeysBlock);

    await fireEvent.click(await findByRole('button', { name: 'Remove' }));
    expect(removed).not.toHaveBeenCalled();

    await fireEvent.click(await findByRole('button', { name: 'Confirm remove' }));
    expect(removed).toHaveBeenCalledWith('pk-1');
  });

  it('goes inert rather than absent when this backend has no domain', async () => {
    setPasskeysAvailableFromBootstrap(false);
    setBindingMock('ListPasskeys', async () => []);
    const begin = setBindingMock('BeginPasskeyRegistration', async () => {
      throw new Error('must not be reached');
    });
    const { findByRole, findByText } = render(PasskeysBlock);

    // The control stays mounted and disabled, with the remedy beside it —
    // a screen that lost half its buttons reads as broken.
    const add = await findByRole('button', { name: 'Add a passkey' });
    expect((add as HTMLButtonElement).disabled).toBe(true);
    await findByText(/need a domain name for this backend/);
    await fireEvent.click(add);
    expect(begin).not.toHaveBeenCalled();
  });
});
