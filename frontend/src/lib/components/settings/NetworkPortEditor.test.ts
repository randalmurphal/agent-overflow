import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import NetworkPortEditor from './NetworkPortEditor.svelte';
import type { NetworkSettings } from '../../stores/bindings';

// The port field has one job and one hazard. The job is to send a number
// the backend will accept. The hazard is a certificate poll landing while
// somebody is mid-type, which is what the draft/seeded pair exists for.

function settings(overrides: Partial<NetworkSettings> = {}): NetworkSettings {
  return {
    bindAll: false,
    listenPort: 0,
    canonicalDomain: '',
    acmeDnsHook: [],
    externalCertFile: '',
    externalKeyFile: '',
    tailnetEnabled: false,
    tailnetControlUrl: '',
    tls: {
      serving: 'self-signed',
      notAfter: 0,
      renewing: false,
      lastError: '',
      selfSignedFingerprint: '',
    },
    tailnet: {
      running: false,
      state: '',
      authUrl: '',
      dnsName: '',
      ips: [],
      url: '',
      https: false,
      hasState: false,
      lastError: '',
    },
    url: '',
    token: '',
    insecure: false,
    ...overrides,
  } as NetworkSettings;
}

type SaveHandler = (port: number) => Promise<void>;
const noopSave: SaveHandler = async () => {};

function mount(overrides: Partial<NetworkSettings> = {}, busy = false) {
  const onsave = vi.fn(noopSave);
  const view = render(NetworkPortEditor, {
    props: { settings: settings(overrides), busy, onsave },
  });
  return { ...view, onsave };
}

describe('<NetworkPortEditor>', () => {
  // Zero is "automatic" on the wire. A literal 0 in the box would read as
  // a port somebody chose.
  it('shows an empty field for automatic, with nothing to save', () => {
    const { getByTestId } = mount();
    expect((getByTestId('network-listen-port') as HTMLInputElement).value).toBe('');
    expect((getByTestId('network-port-save') as HTMLButtonElement).disabled).toBe(true);
    expect((getByTestId('network-port-revert') as HTMLButtonElement).disabled).toBe(true);
  });

  it('shows a chosen port as its number', () => {
    const { getByTestId } = mount({ listenPort: 7777 });
    expect((getByTestId('network-listen-port') as HTMLInputElement).value).toBe('7777');
  });

  it('sends the typed port as a number', async () => {
    const { getByTestId, onsave } = mount();

    await fireEvent.input(getByTestId('network-listen-port'), { target: { value: ' 7777 ' } });
    await fireEvent.click(getByTestId('network-port-save'));

    await waitFor(() => expect(onsave).toHaveBeenCalledTimes(1));
    expect(onsave.mock.calls[0][0]).toBe(7777);
  });

  // Clearing the field is how "automatic" is expressed, so it has to be a
  // savable edit rather than a no-op.
  it('sends zero when the field is cleared', async () => {
    const { getByTestId, onsave } = mount({ listenPort: 7777 });

    await fireEvent.input(getByTestId('network-listen-port'), { target: { value: '' } });
    await fireEvent.click(getByTestId('network-port-save'));

    await waitFor(() => expect(onsave).toHaveBeenCalledTimes(1));
    expect(onsave.mock.calls[0][0]).toBe(0);
  });

  it('refuses to send anything the backend would reject', async () => {
    for (const value of ['abc', '0', '-1', '65536', '80.5', '8 0']) {
      const { getByTestId, onsave, unmount } = mount();
      await fireEvent.input(getByTestId('network-listen-port'), { target: { value } });
      expect(getByTestId('network-listen-port-error')).toBeTruthy();
      expect((getByTestId('network-port-save') as HTMLButtonElement).disabled).toBe(true);
      expect(onsave).not.toHaveBeenCalled();
      unmount();
    }
  });

  // Privileged ports are legitimate on a host with the capability, so the
  // field must not second-guess them.
  it('accepts the whole port range', async () => {
    for (const value of ['1', '443', '65535']) {
      const { getByTestId, queryByTestId, unmount } = mount();
      await fireEvent.input(getByTestId('network-listen-port'), { target: { value } });
      expect(queryByTestId('network-listen-port-error')).toBeNull();
      expect((getByTestId('network-port-save') as HTMLButtonElement).disabled).toBe(false);
      unmount();
    }
  });

  it('reverts an edit back to what the backend stored', async () => {
    const { getByTestId } = mount({ listenPort: 7777 });
    const field = getByTestId('network-listen-port') as HTMLInputElement;

    await fireEvent.input(field, { target: { value: '9999' } });
    await fireEvent.click(getByTestId('network-port-revert'));

    expect(field.value).toBe('7777');
    expect((getByTestId('network-port-save') as HTMLButtonElement).disabled).toBe(true);
  });

  it('disables every control while a save is in flight', () => {
    const { getByTestId } = mount({ listenPort: 7777 }, true);
    expect((getByTestId('network-listen-port') as HTMLInputElement).disabled).toBe(true);
    expect((getByTestId('network-port-save') as HTMLButtonElement).disabled).toBe(true);
    expect((getByTestId('network-port-revert') as HTMLButtonElement).disabled).toBe(true);
  });

  it('re-seeds the draft when the stored port moves, and not on an unchanged poll', async () => {
    const onsave = vi.fn(noopSave);
    const view = render(NetworkPortEditor, {
      props: { settings: settings(), busy: false, onsave },
    });
    const field = () => view.getByTestId('network-listen-port') as HTMLInputElement;

    await fireEvent.input(field(), { target: { value: '77' } });
    // A certificate poll answers the same stored port; a half-typed number
    // must survive it.
    await view.rerender({
      settings: settings({ tls: { ...settings().tls, renewing: true } }),
      busy: false,
      onsave,
    });
    expect(field().value).toBe('77');

    // A write that landed does re-seed it.
    await view.rerender({ settings: settings({ listenPort: 7777 }), busy: false, onsave });
    expect(field().value).toBe('7777');
  });
});
