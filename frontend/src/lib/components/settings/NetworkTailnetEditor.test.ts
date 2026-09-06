import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import NetworkTailnetEditor from './NetworkTailnetEditor.svelte';
import type { NetworkSettings } from '../../stores/bindings';

// The tailnet block renders one thing: whatever the polled status says.
// Every case below is a state the node can really be in, and what the
// person looking at the screen is given to act on in it.

type TailnetStatus = NetworkSettings['tailnet'];

function tailnet(overrides: Partial<TailnetStatus> = {}): TailnetStatus {
  return {
    running: false,
    state: '',
    authUrl: '',
    dnsName: '',
    ips: [],
    url: '',
    https: false,
    hasState: false,
    lastError: '',
    ...overrides,
  } as TailnetStatus;
}

function settings(overrides: Partial<NetworkSettings> = {}): NetworkSettings {
  return {
    bindAll: false,
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
    tailnet: tailnet(),
    url: '',
    token: '',
    insecure: false,
    ...overrides,
  } as NetworkSettings;
}

// The save handler's ARGUMENT is what most of these cases assert on, so the
// mock is typed rather than inferred: `vi.fn(async () => {})` infers a
// zero-length parameter tuple and `calls[0][0]` stops type-checking.
type SaveDraft = { enabled: boolean; controlURL: string };
type SaveHandler = (draft: SaveDraft) => Promise<void>;

const noopSave: SaveHandler = async () => {};

function mount(overrides: Partial<NetworkSettings> = {}, busy = false) {
  const onsave = vi.fn(noopSave);
  const onforget = vi.fn(async () => {});
  const view = render(NetworkTailnetEditor, {
    props: { settings: settings(overrides), busy, onsave, onforget },
  });
  return { ...view, onsave, onforget };
}

describe('<NetworkTailnetEditor>', () => {
  it('starts off, with nothing to save and nothing to forget', () => {
    const { getByTestId, queryByTestId } = mount();
    expect(getByTestId('network-tailnet-status').textContent).toContain('Off');
    expect(queryByTestId('network-tailnet-save')).toBeNull();
    expect(queryByTestId('network-tailnet-forget')).toBeNull();
    expect(queryByTestId('network-tailnet-auth')).toBeNull();
  });

  it('sends the toggle and the coordination server through one save', async () => {
    const { getByTestId, getByRole, onsave } = mount();

    await fireEvent.click(getByRole('switch', { name: 'Toggle tailnet access' }));
    await fireEvent.input(getByTestId('network-tailnet-control-url'), {
      target: { value: 'https://headscale.example ' },
    });
    await fireEvent.click(getByTestId('network-tailnet-save'));

    await waitFor(() => expect(onsave).toHaveBeenCalledTimes(1));
    expect(onsave.mock.calls[0][0]).toEqual({
      enabled: true,
      controlURL: 'https://headscale.example ',
    });
  });

  it('reverts an edit back to what the backend stored', async () => {
    const { getByTestId, queryByTestId } = mount();
    const field = getByTestId('network-tailnet-control-url') as HTMLInputElement;

    await fireEvent.input(field, { target: { value: 'https://typo.example' } });
    expect((getByTestId('network-tailnet-revert') as HTMLButtonElement).disabled).toBe(false);

    await fireEvent.click(getByTestId('network-tailnet-revert'));
    expect(field.value).toBe('');
    expect(queryByTestId('network-tailnet-save')).toBeNull();
  });

  it('offers the sign-in link while the node waits for approval', () => {
    const { getByTestId, queryByTestId } = mount({
      tailnetEnabled: true,
      tailnet: tailnet({ state: 'NeedsLogin', authUrl: 'https://login.example/a/abc123' }),
    });

    expect(getByTestId('network-tailnet-status').textContent).toContain('approve this machine');
    const link = getByTestId('network-tailnet-auth-link') as HTMLAnchorElement;
    expect(link.getAttribute('href')).toBe('https://login.example/a/abc123');
    expect(getByTestId('network-tailnet-auth-copy')).toBeTruthy();
    // No share URL yet: there is nothing on the tailnet to share.
    expect(queryByTestId('network-tailnet-url')).toBeNull();
  });

  it('names the node and says the connection is not HTTPS when the tailnet has it off', () => {
    const { getByTestId, queryByTestId } = mount({
      tailnetEnabled: true,
      tailnet: tailnet({
        running: true,
        state: 'Running',
        dnsName: 'agent-overflow.example.ts.net',
        ips: ['100.101.102.103'],
        url: 'http://agent-overflow.example.ts.net:54321/?t=ticket',
      }),
    });

    const status = getByTestId('network-tailnet-status').textContent ?? '';
    expect(status).toContain('agent-overflow.example.ts.net');
    expect(status).toContain('admin panel');
    expect((getByTestId('network-tailnet-url') as HTMLElement).textContent).not.toContain('http');
    expect(
      (getByTestId('network-tailnet-url').querySelector('input') as HTMLInputElement).value,
    ).toBe('http://agent-overflow.example.ts.net:54321/?t=ticket');
    expect(queryByTestId('network-tailnet-auth')).toBeNull();
  });

  it('says so plainly once the node also answers HTTPS', () => {
    const { getByTestId } = mount({
      tailnetEnabled: true,
      tailnet: tailnet({
        running: true,
        state: 'Running',
        https: true,
        dnsName: 'agent-overflow.example.ts.net',
        url: 'https://agent-overflow.example.ts.net/?t=ticket',
      }),
    });
    const status = getByTestId('network-tailnet-status').textContent ?? '';
    expect(status).toContain('over HTTPS');
    expect(status).not.toContain('admin panel');
  });

  it('renders a node failure verbatim', () => {
    const { getByTestId } = mount({
      tailnetEnabled: true,
      tailnet: tailnet({ state: 'Stopped', lastError: 'dial control server: connection refused' }),
    });
    expect(getByTestId('network-tailnet-error').textContent).toContain(
      'dial control server: connection refused',
    );
  });

  it('offers forget only once the feature is off, and only on the second press', async () => {
    const enabled = mount({
      tailnetEnabled: true,
      tailnet: tailnet({ running: true, state: 'Running', hasState: true, dnsName: 'a.b.ts.net' }),
    });
    expect(enabled.queryByTestId('network-tailnet-forget')).toBeNull();
    enabled.unmount();

    const { getByTestId, onforget } = mount({
      tailnetEnabled: false,
      tailnet: tailnet({ hasState: true }),
    });
    const button = getByTestId('network-tailnet-forget');
    expect(button.textContent).toContain('Forget this node');

    await fireEvent.click(button);
    expect(onforget).not.toHaveBeenCalled();
    expect(getByTestId('network-tailnet-forget').textContent).toContain('Confirm forget');

    await fireEvent.click(getByTestId('network-tailnet-forget'));
    await waitFor(() => expect(onforget).toHaveBeenCalledTimes(1));
  });

  it('says the identity is kept while the feature is off', () => {
    const { getByTestId } = mount({ tailnet: tailnet({ hasState: true }) });
    expect(getByTestId('network-tailnet-status').textContent).toContain('identity is kept');
  });

  it('disables every control while a save is in flight', () => {
    const { getByTestId, getByRole } = mount({ tailnet: tailnet({ hasState: true }) }, true);
    expect((getByTestId('network-tailnet-save') as HTMLButtonElement).disabled).toBe(true);
    expect((getByTestId('network-tailnet-revert') as HTMLButtonElement).disabled).toBe(true);
    expect((getByTestId('network-tailnet-forget') as HTMLButtonElement).disabled).toBe(true);
    expect((getByTestId('network-tailnet-control-url') as HTMLInputElement).disabled).toBe(true);
    expect((getByRole('switch', { name: 'Toggle tailnet access' }) as HTMLButtonElement).disabled).toBe(
      true,
    );
  });

  it('re-seeds the draft when the stored values move, and not on an unchanged poll', async () => {
    const onsave = vi.fn(noopSave);
    const onforget = vi.fn(async () => {});
    const view = render(NetworkTailnetEditor, {
      props: { settings: settings(), busy: false, onsave, onforget },
    });
    const field = view.getByTestId('network-tailnet-control-url') as HTMLInputElement;

    await fireEvent.input(field, { target: { value: 'https://half-typed.exa' } });
    // A poll answering the same stored values must not wipe what is being
    // typed: the status changed, the preference did not.
    await view.rerender({
      settings: settings({ tailnet: tailnet({ state: 'Starting' }) }),
      busy: false,
      onsave,
      onforget,
    });
    expect((view.getByTestId('network-tailnet-control-url') as HTMLInputElement).value).toBe(
      'https://half-typed.exa',
    );

    // A write that landed does re-seed it.
    await view.rerender({
      settings: settings({ tailnetControlUrl: 'https://headscale.example' }),
      busy: false,
      onsave,
      onforget,
    });
    expect((view.getByTestId('network-tailnet-control-url') as HTMLInputElement).value).toBe(
      'https://headscale.example',
    );
  });
});
