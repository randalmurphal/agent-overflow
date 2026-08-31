import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import NetworkSection from './NetworkSection.svelte';
import { setBindingMock, resetBindingMocks } from '../../../test/mocks/bindings-app';
import { setRunMode, resetRunMode } from '../../../test/runMode';

interface MockNetworkSettings {
  bindAll: boolean;
  url: string;
  token: string;
  insecure?: boolean;
}

function networkSettings(overrides: Partial<MockNetworkSettings> = {}): MockNetworkSettings {
  return {
    bindAll: false,
    url: 'http://127.0.0.1:54321/?t=test-token',
    token: 'test-token',
    insecure: false,
    ...overrides,
  };
}

describe('<NetworkSection>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetRunMode();
  });

  afterEach(() => {
    resetBindingMocks();
    resetRunMode();
  });

  it('renders the toggle in the loaded state', async () => {
    setBindingMock('GetNetworkSettings', async () => networkSettings({ bindAll: false }));
    const { findByRole, findByLabelText } = render(NetworkSection);
    // Wait for the URL field to render; that's the signal the load
    // effect has resolved and the reactive state reflects the backend.
    await findByLabelText('Application URL');
    const toggle = await findByRole('switch', { name: 'Toggle remote access' });
    expect(toggle.getAttribute('aria-checked')).toBe('false');
  });

  it('reflects the bind-all preference loaded from the backend', async () => {
    setBindingMock('GetNetworkSettings', async () =>
      networkSettings({ bindAll: true, url: 'http://192.168.1.10:54321/?t=test-token' }),
    );
    const { findByRole, findByLabelText } = render(NetworkSection);
    const url = await findByLabelText('Application URL');
    expect((url as HTMLInputElement).value).toBe('http://192.168.1.10:54321/?t=test-token');

    const toggle = await findByRole('switch', { name: 'Toggle remote access' });
    await waitFor(() => {
      expect(toggle.getAttribute('aria-checked')).toBe('true');
    });
  });

  it('says what the share URL actually carries, on both binds', async () => {
    // The URL carries a one-time page ticket that only loads the page;
    // a device reaching this backend over the network still has to pair.
    // Copy that promises a standing credential on the URL would be
    // describing the shape this stopped having.
    for (const bindAll of [true, false]) {
      resetBindingMocks();
      setBindingMock('GetNetworkSettings', async () => networkSettings({ bindAll }));
      const { findByText, unmount } = render(NetworkSection);
      await findByText(/one-time ticket that only loads the page/i);
      await findByText(/still has to pair/i);
      unmount();
    }
  });

  it('flips the toggle through SetNetworkSettings on click', async () => {
    setBindingMock('GetNetworkSettings', async () => networkSettings({ bindAll: false }));
    const setMock = setBindingMock('SetNetworkSettings', async (next: unknown) => {
      const n = next as MockNetworkSettings;
      return networkSettings({
        bindAll: n.bindAll,
        url: n.bindAll
          ? 'http://192.168.1.10:54321/?t=test-token'
          : 'http://127.0.0.1:54321/?t=test-token',
      });
    });

    const { findByRole, findByLabelText } = render(NetworkSection);
    // Wait until the load effect resolves so the toggle's disabled
    // gate ({!settings || saving}) opens — clicking before that point
    // is a no-op.
    await findByLabelText('Application URL');
    const toggle = await findByRole('switch', { name: 'Toggle remote access' });
    await fireEvent.click(toggle);

    await waitFor(() => {
      expect(setMock).toHaveBeenCalledTimes(1);
    });
    const args = setMock.mock.calls[0][0] as MockNetworkSettings;
    expect(args.bindAll).toBe(true);
  });

  it('reverts the toggle and surfaces an error when SetNetworkSettings fails', async () => {
    setBindingMock('GetNetworkSettings', async () => networkSettings({ bindAll: false }));
    const setMock = setBindingMock('SetNetworkSettings', async () => {
      throw new Error('rebind failed');
    });

    const { findByRole, findByLabelText } = render(NetworkSection);
    await findByLabelText('Application URL');
    const toggle = await findByRole('switch', { name: 'Toggle remote access' });

    // Pre-flight: toggle starts at false. Click drives the optimistic
    // flip to true; the failed SetNetworkSettings must put it back.
    expect(toggle.getAttribute('aria-checked')).toBe('false');
    await fireEvent.click(toggle);

    // SetNetworkSettings was called with the requested next state.
    await waitFor(() => {
      expect(setMock).toHaveBeenCalledTimes(1);
    });

    // After the rejection settles, the toggle must report the old state
    // again — otherwise the UI lies about the persisted preference.
    await waitFor(() => {
      expect(toggle.getAttribute('aria-checked')).toBe('false');
    });

    // The disabled gate ({!settings || saving}) must reopen so the
    // user can retry. ToggleSwitch sets disabled directly on the
    // underlying button.
    await waitFor(() => {
      expect((toggle as HTMLButtonElement).disabled).toBe(false);
    });
  });

  it('hides the toggle and renders a placeholder in --connect (client) mode', async () => {
    setRunMode('client');
    // The component should never call GetNetworkSettings in client
    // mode — the RPC would query the *remote* server's bind preference
    // and rendering it here would be misleading.
    const getMock = setBindingMock('GetNetworkSettings', async () => ({
      bindAll: true,
      url: 'http://remote-backend:1234',
      token: 'should-not-render',
    }));

    const { findByText, queryByRole } = render(NetworkSection);
    await findByText(/Network binding can only be edited from your local install/i);
    expect(queryByRole('switch', { name: 'Toggle remote access' })).toBeNull();
    expect(getMock).not.toHaveBeenCalled();
  });

  it('renders the plaintext-LAN warning when settings.insecure is true', async () => {
    setBindingMock('GetNetworkSettings', async () =>
      networkSettings({
        bindAll: true,
        url: 'http://10.0.0.5:54321/?t=test-token',
        insecure: true,
      }),
    );

    const { findByTestId } = render(NetworkSection);
    const warning = await findByTestId('insecure-url-warning');
    // The warning prose names the actionable mitigation so the user
    // knows what to do — assert the key tokens that pin the message.
    expect(warning.textContent).toMatch(/plaintext over LAN/i);
    expect(warning.textContent).toMatch(/Tailscale|SSH tunnel|reverse proxy/i);
  });

  it('omits the plaintext-LAN warning when settings.insecure is false', async () => {
    setBindingMock('GetNetworkSettings', async () =>
      networkSettings({ bindAll: false, insecure: false }),
    );

    const { findByLabelText, queryByTestId } = render(NetworkSection);
    await findByLabelText('Application URL');
    expect(queryByTestId('insecure-url-warning')).toBeNull();
  });

  it('writes the URL to the clipboard when Copy is clicked', async () => {
    setBindingMock('GetNetworkSettings', async () =>
      networkSettings({ url: 'http://10.0.0.5:54321/?t=test-token' }),
    );

    const writeText = vi.fn(async () => undefined);
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    });

    const { findByRole, findByLabelText } = render(NetworkSection);
    await findByLabelText('Application URL');
    const copyButton = await findByRole('button', { name: 'Copy' });
    await fireEvent.click(copyButton);

    await waitFor(() => {
      expect(writeText).toHaveBeenCalledWith('http://10.0.0.5:54321/?t=test-token');
    });
  });
});
