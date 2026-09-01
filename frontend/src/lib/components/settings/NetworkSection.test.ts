import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import NetworkSection from './NetworkSection.svelte';
import { setBindingMock, resetBindingMocks } from '../../../test/mocks/bindings-app';
import { setRunMode, resetRunMode } from '../../../test/runMode';

interface MockTLSStatus {
  serving: string;
  notAfter: number;
  renewing: boolean;
  lastError: string;
  selfSignedFingerprint: string;
}

interface MockNetworkSettings {
  bindAll: boolean;
  canonicalDomain: string;
  acmeDnsHook: string[];
  externalCertFile: string;
  externalKeyFile: string;
  tls: MockTLSStatus;
  url: string;
  token: string;
  insecure?: boolean;
}

function tlsStatus(overrides: Partial<MockTLSStatus> = {}): MockTLSStatus {
  return {
    serving: 'self-signed',
    notAfter: 0,
    renewing: false,
    lastError: '',
    selfSignedFingerprint: '',
    ...overrides,
  };
}

function networkSettings(overrides: Partial<MockNetworkSettings> = {}): MockNetworkSettings {
  return {
    bindAll: false,
    canonicalDomain: '',
    acmeDnsHook: [],
    externalCertFile: '',
    externalKeyFile: '',
    tls: tlsStatus(),
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

  it('carries the domain half through a bind toggle', async () => {
    // SetNetworkSettings writes the whole persisted record. A toggle that
    // sent only `bindAll` erased the domain, the hook and the certificate
    // paths, which un-served the certificate on the next boot.
    setBindingMock('GetNetworkSettings', async () =>
      networkSettings({
        canonicalDomain: 'ao.example.com',
        acmeDnsHook: ['dnstool', '--zone', 'example.com'],
        externalCertFile: '/etc/ssl/ao/fullchain.pem',
        externalKeyFile: '/etc/ssl/ao/privkey.pem',
      }),
    );
    const setMock = setBindingMock('SetNetworkSettings', async (next: unknown) => next);

    const { findByRole, findByLabelText } = render(NetworkSection);
    await findByLabelText('Application URL');
    const toggle = await findByRole('switch', { name: 'Toggle remote access' });
    await fireEvent.click(toggle);

    await waitFor(() => {
      expect(setMock).toHaveBeenCalledTimes(1);
    });
    const args = setMock.mock.calls[0][0] as MockNetworkSettings;
    expect(args.bindAll).toBe(true);
    expect(args.canonicalDomain).toBe('ao.example.com');
    expect(args.acmeDnsHook).toEqual(['dnstool', '--zone', 'example.com']);
    expect(args.externalCertFile).toBe('/etc/ssl/ao/fullchain.pem');
    expect(args.externalKeyFile).toBe('/etc/ssl/ao/privkey.pem');
  });

  it('saves the domain with the DNS hook split into an argv', async () => {
    setBindingMock('GetNetworkSettings', async () => networkSettings());
    const setMock = setBindingMock('SetNetworkSettings', async (next: unknown) => next);

    const { findByTestId } = render(NetworkSection);
    const domain = await findByTestId('network-canonical-domain');
    await fireEvent.input(domain, { target: { value: '  ao.example.com  ' } });
    const hook = await findByTestId('network-dns-hook');
    await fireEvent.input(hook, { target: { value: 'dnstool --zone "example com"' } });
    await fireEvent.click(await findByTestId('network-domain-save'));

    await waitFor(() => {
      expect(setMock).toHaveBeenCalledTimes(1);
    });
    const args = setMock.mock.calls[0][0] as MockNetworkSettings;
    expect(args.canonicalDomain).toBe('ao.example.com');
    expect(args.acmeDnsHook).toEqual(['dnstool', '--zone', 'example com']);
  });

  it('refuses to save a DNS hook that cannot be read as a command', async () => {
    setBindingMock('GetNetworkSettings', async () => networkSettings());
    const setMock = setBindingMock('SetNetworkSettings', async (next: unknown) => next);

    const { findByTestId, queryByTestId } = render(NetworkSection);
    const hook = await findByTestId('network-dns-hook');
    await fireEvent.input(hook, { target: { value: 'dnstool --zone "unclosed' } });

    await findByTestId('network-dns-hook-error');
    expect(((await findByTestId('network-domain-save')) as HTMLButtonElement).disabled).toBe(true);
    expect(setMock).not.toHaveBeenCalled();
    expect(queryByTestId('network-tls-error')).toBeNull();
  });

  it('checks the certificate without writing settings', async () => {
    setBindingMock('GetNetworkSettings', async () =>
      networkSettings({ canonicalDomain: 'ao.example.com' }),
    );
    const setMock = setBindingMock('SetNetworkSettings', async (next: unknown) => next);
    const renewMock = setBindingMock('RenewCanonicalDomainCert', async () =>
      networkSettings({
        canonicalDomain: 'ao.example.com',
        tls: tlsStatus({ serving: 'none', renewing: true }),
      }),
    );

    const { findByTestId } = render(NetworkSection);
    await fireEvent.click(await findByTestId('network-domain-renew'));

    await waitFor(() => {
      expect(renewMock).toHaveBeenCalledTimes(1);
    });
    expect(setMock).not.toHaveBeenCalled();
  });

  it('names the certificate that is serving and when it expires', async () => {
    setBindingMock('GetNetworkSettings', async () =>
      networkSettings({
        canonicalDomain: 'ao.example.com',
        url: 'https://ao.example.com/?t=test-token',
        tls: tlsStatus({ serving: 'acme', notAfter: Date.UTC(2027, 5, 10, 12) }),
      }),
    );

    const { findByTestId } = render(NetworkSection);
    const status = await findByTestId('network-tls-status');
    expect(status.textContent).toMatch(/ao\.example\.com/);
    expect(status.textContent).toMatch(/2027/);
  });

  it('surfaces the last certificate failure verbatim', async () => {
    setBindingMock('GetNetworkSettings', async () =>
      networkSettings({
        canonicalDomain: 'ao.example.com',
        tls: tlsStatus({ serving: 'self-signed', lastError: 'dns hook: exit status 2' }),
      }),
    );

    const { findByTestId } = render(NetworkSection);
    const failure = await findByTestId('network-tls-error');
    expect(failure.textContent).toMatch(/dns hook: exit status 2/);
  });

  it('describes an https share URL as the domain rather than the LAN IP', async () => {
    setBindingMock('GetNetworkSettings', async () =>
      networkSettings({
        bindAll: true,
        canonicalDomain: 'ao.example.com',
        url: 'https://ao.example.com/?t=test-token',
        insecure: false,
        tls: tlsStatus({ serving: 'acme', notAfter: Date.UTC(2027, 5, 10, 12) }),
      }),
    );

    const { findByText, queryByTestId, findByLabelText } = render(NetworkSection);
    await findByLabelText('Application URL');
    await findByText(/opens it over HTTPS with no warning/i);
    expect(queryByTestId('insecure-url-warning')).toBeNull();
  });

  it('re-reads while a certificate is being obtained, without wiping a half-typed field', async () => {
    vi.useFakeTimers();
    try {
      const getMock = setBindingMock('GetNetworkSettings', async () =>
        networkSettings({ tls: tlsStatus({ serving: 'self-signed', renewing: true }) }),
      );

      const { findByTestId, unmount } = render(NetworkSection);
      const domain = (await findByTestId('network-canonical-domain')) as HTMLInputElement;
      await fireEvent.input(domain, { target: { value: 'ao.example.com' } });
      expect(getMock).toHaveBeenCalledTimes(1);

      await vi.advanceTimersByTimeAsync(3100);
      expect(getMock.mock.calls.length).toBeGreaterThan(1);
      // The poll answers with the stored (still empty) domain. Adopting it
      // would delete what the user is in the middle of typing.
      expect(domain.value).toBe('ao.example.com');

      // Teardown stops the poll: an unmounted screen must not keep asking.
      unmount();
      const after = getMock.mock.calls.length;
      await vi.advanceTimersByTimeAsync(9000);
      expect(getMock.mock.calls.length).toBe(after);
    } finally {
      vi.useRealTimers();
    }
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
