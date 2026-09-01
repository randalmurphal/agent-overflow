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
import { getToasts } from '../../stores/toast.svelte';
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

  it('asks for the registration and nothing else, because proving is the transport’s', async () => {
    // The block calls the gated method plainly. Where the caller is a
    // remote screen, `transport/stepUp.ts` runs the ceremony for whatever
    // the backend refuses and dispatches the call again — so a ceremony
    // started HERE would be a second mechanism, and on the owner's own
    // machine (host presence satisfies the gate) it would be a prompt for
    // nothing.
    setBindingMock('ListPasskeys', async () => []);
    const begin = setBindingMock('BeginPasskeyRegistration', async () => ({
      ceremonyId: 'cer-1',
      options: { challenge: 'AQID' },
    }));
    const ceremony = setBindingMock('BeginPasskeyStepUp', async () => {
      throw new Error('must not be reached');
    });
    const { findByRole } = render(PasskeysBlock);

    await fireEvent.click(await findByRole('button', { name: 'Add a passkey' }));
    await vi.waitFor(() => expect(begin).toHaveBeenCalledTimes(1));
    expect(ceremony).not.toHaveBeenCalled();
  });

  it('reports nothing when the prompt is dismissed', async () => {
    // `installAuthenticator` answers `create` with null, which is a
    // dismissal — nothing went wrong, somebody changed their mind, and a
    // toast would accuse them of a fault. The same holds for a step-up
    // prompt dismissed under the transport's ceremony: what reaches here
    // is the original refusal, not the abandonment.
    setBindingMock('ListPasskeys', async () => []);
    setBindingMock('BeginPasskeyRegistration', async () => ({
      ceremonyId: 'cer-1',
      options: { challenge: 'AQID' },
    }));
    const before = getToasts().length;
    const { findByRole } = render(PasskeysBlock);

    const add = await findByRole('button', { name: 'Add a passkey' });
    await fireEvent.click(add);
    // Settled: the control is live again, which is the state the `finally`
    // restores after the abandoned ceremony.
    await vi.waitFor(() => expect((add as HTMLButtonElement).disabled).toBe(false));
    expect(getToasts().slice(before)).toEqual([]);
  });

  it('reports a refusal the transport could not satisfy', async () => {
    // The other half of the pair above: a change that did not go through
    // must say so. Reached when there is no passkey to prove with, or the
    // proof was dismissed.
    setBindingMock('ListPasskeys', async () => []);
    setBindingMock('BeginPasskeyRegistration', async () => {
      throw new Error('needs a fresh proof');
    });
    const { findByRole } = render(PasskeysBlock);

    await fireEvent.click(await findByRole('button', { name: 'Add a passkey' }));
    await vi.waitFor(() =>
      expect(getToasts().at(-1)?.message).toMatch(/^Failed to add a passkey:/),
    );
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
