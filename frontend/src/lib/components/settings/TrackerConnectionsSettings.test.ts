import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import TrackerConnectionsSettings from './TrackerConnectionsSettings.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { pairWithScopes, resetToLocalPage } from '../../../test/helpers/scopes';
import { KeyFileState, trackerConnection } from '../../../test/helpers/trackerConnections';
import { TransportError } from '../../transport/wsClient';
import type { TrackerConnection } from '../../stores/bindings';

/** The computer's list: what TrackerConnections answers now. */
let rows: TrackerConnection[] = [];

beforeEach(() => {
  resetToLocalPage();
  rows = [];
  setBindingMock('TrackerConnections', async () => rows);
});
afterEach(() => {
  resetToLocalPage();
});

const fresh = (overrides: Partial<TrackerConnection> = {}): TrackerConnection =>
  trackerConnection({ verifiedAt: 0, verifiedName: '', verifiedAccountId: '', keyFileState: KeyFileState.StateEmpty, ...overrides });

async function openMenu(rowId: string): Promise<void> {
  const row = await screen.findByTestId(`tracker-connection-${rowId}`);
  await fireEvent.click(within(row).getAllByTestId('tracker-connection-menu')[0]);
}

describe('<TrackerConnectionsSettings>', () => {
  it('adds a Jira connection from its site and email, then shows where its token goes', async () => {
    const add = setBindingMock('AddTrackerConnection', async () => {
      rows = [fresh()];
      return rows[0];
    });
    render(TrackerConnectionsSettings);
    await screen.findByTestId('settings-tracker-connections-empty');

    await fireEvent.click(screen.getByTestId('tracker-connection-add'));
    const submit = screen.getByTestId('tracker-connection-add-submit') as HTMLButtonElement;
    expect(submit.disabled).toBe(true);
    await fireEvent.input(screen.getByTestId('tracker-connection-add-site'), { target: { value: ' example.atlassian.net ' } });
    await fireEvent.input(screen.getByTestId('tracker-connection-add-account'), { target: { value: 'user1@example.com' } });
    await fireEvent.click(submit);

    await waitFor(() => expect(add).toHaveBeenCalledWith({ kind: 'jira', site: 'example.atlassian.net', account: 'user1@example.com', label: '' }));
    const row = await screen.findByTestId('tracker-connection-jira-1');
    expect(within(row).getByTestId('tracker-connection-status').textContent).toContain('Needs a token');
    expect(within(row).getByTestId('tracker-connection-key-path').textContent).toBe(rows[0].keyFilePath);
    expect(within(row).getByTestId('tracker-connection-key-state').textContent).toBe('The key file is empty.');
    // The token page offers two kinds; only the one without scopes reaches the site.
    expect(within(row).getByTestId('tracker-connection-make-token').textContent).toBe(
      'On the token page, choose Create API token, not Create API token with scopes: a token with scopes cannot reach the Jira site, so the check fails. Make it while signed in as user1@example.com, and give it the longest life the page offers.',
    );
    expect(within(row).getByTestId('tracker-connection-open-token-page')).toBeTruthy();
    expect(within(row).getByTestId('tracker-connection-open-key-file')).toBeTruthy();
    await waitFor(() => expect(screen.queryByTestId('tracker-connection-add-form')).toBeNull());
    // Nothing on the page takes a token: the one input that names it takes its expiry date.
    for (const input of document.querySelectorAll('input')) {
      expect(input.type).not.toBe('password');
      if (input.type === 'date') continue;
      expect(`${input.getAttribute('aria-label') ?? ''} ${input.labels?.[0]?.textContent ?? ''}`.toLowerCase()).not.toContain('token');
    }
  });

  it('shows the computer’s refusal of a new connection as its sentence, and keeps what was typed', async () => {
    setBindingMock('AddTrackerConnection', async () => {
      throw new TransportError('tracker_connection_invalid', 'the Jira site must be an address such as https://example.atlassian.net: it has a path', {
        detail: { ref: 'r1', method: 'AddTrackerConnection', at: 1, chain: ['trackerapp: invalid tracker connection: the Jira site must be an address such as https://example.atlassian.net: it has a path'] },
      });
    });
    render(TrackerConnectionsSettings);
    await fireEvent.click(await screen.findByTestId('tracker-connection-add'));
    await fireEvent.input(screen.getByTestId('tracker-connection-add-site'), { target: { value: 'example.atlassian.net/jira' } });
    await fireEvent.input(screen.getByTestId('tracker-connection-add-account'), { target: { value: 'user1@example.com' } });
    await fireEvent.click(screen.getByTestId('tracker-connection-add-submit'));
    const error = await screen.findByTestId('tracker-connection-add-error');
    expect(error.textContent).toBe('The Jira site must be an address such as https://example.atlassian.net: it has a path.');
    expect((screen.getByTestId('tracker-connection-add-site') as HTMLInputElement).value).toBe('example.atlassian.net/jira');
  });

  it('checks a connection and shows who it is signed in as', async () => {
    rows = [fresh({ keyFileState: KeyFileState.StatePresent })];
    const verify = setBindingMock('VerifyTrackerConnection', async () => {
      rows = [trackerConnection()];
      return rows[0];
    });
    render(TrackerConnectionsSettings);
    const row = await screen.findByTestId('tracker-connection-jira-1');
    expect(within(row).getByTestId('tracker-connection-status').textContent).toContain('Not checked yet');
    await fireEvent.click(within(row).getByTestId('tracker-connection-setup-check'));
    await waitFor(() => expect(within(row).getByTestId('tracker-connection-status').textContent).toContain('Signed in as User 1 (user1@example.com)'));
    expect(verify).toHaveBeenCalledWith('jira-1');
    expect(within(row).queryByTestId('tracker-connection-setup')).toBeNull();
  });

  it('warns before the token lapses, shows a lapsed one as an error, and offers Renew with the steps', async () => {
    rows = [
      trackerConnection({ id: 'soon', tokenExpiresAt: Date.now() + 3 * 24 * 60 * 60 * 1000 }),
      trackerConnection({ id: 'gone', site: 'https://other.example.com', tokenExpiresAt: Date.now() - 1000 }),
      trackerConnection({ id: 'far', site: 'https://far.example.com', tokenExpiresAt: Date.now() + 90 * 24 * 60 * 60 * 1000 }),
    ];
    render(TrackerConnectionsSettings);
    const soon = await screen.findByTestId('tracker-connection-soon');
    expect(within(soon).getByTestId('tracker-connection-expiry').dataset.tone).toBe('warning');
    expect(within(soon).getByTestId('tracker-connection-expiry').textContent).toMatch(/^Token runs out in 2d 23h\.|^Token runs out in 3d\./);
    const gone = screen.getByTestId('tracker-connection-gone');
    expect(within(gone).getByTestId('tracker-connection-expiry').dataset.tone).toBe('error');
    const far = screen.getByTestId('tracker-connection-far');
    expect(within(far).getByTestId('tracker-connection-expiry').dataset.tone).toBe('muted');
    expect(within(far).queryByTestId('tracker-connection-renew')).toBeNull();

    await fireEvent.click(within(soon).getByTestId('tracker-connection-renew'));
    expect(within(soon).getByTestId('tracker-connection-setup')).toBeTruthy();
  });

  it('records the expiry date as the start of that day here, and clears it', async () => {
    rows = [trackerConnection()];
    const expiry = setBindingMock('SetTrackerConnectionTokenExpiry', async (_id: string, at: number) => {
      rows = [trackerConnection({ tokenExpiresAt: at })];
      return rows[0];
    });
    render(TrackerConnectionsSettings);
    await openMenu('jira-1');
    await fireEvent.click(await screen.findByTestId('tracker-connection-menu-expiry'));
    await fireEvent.input(screen.getByTestId('tracker-token-expiry-date'), { target: { value: '2031-05-20' } });
    await fireEvent.click(screen.getByTestId('tracker-token-expiry-save'));
    await waitFor(() => expect(expiry).toHaveBeenCalledWith('jira-1', new Date(2031, 4, 20).getTime()));
    await waitFor(() => expect(screen.getByTestId('tracker-connection-expiry').textContent).toMatch(/^Token good until /));

    await openMenu('jira-1');
    await fireEvent.click(await screen.findByTestId('tracker-connection-menu-expiry'));
    await fireEvent.click(screen.getByTestId('tracker-token-expiry-clear'));
    await waitFor(() => expect(expiry).toHaveBeenLastCalledWith('jira-1', 0));
    await waitFor(() => expect(screen.queryByTestId('tracker-connection-expiry')).toBeNull());
  });

  it('turns a connection off and on with its switch', async () => {
    rows = [trackerConnection()];
    const set = setBindingMock('SetTrackerConnectionEnabled', async (_id: string, on: boolean) => {
      rows = [trackerConnection({ enabled: on })];
      return rows[0];
    });
    render(TrackerConnectionsSettings);
    await fireEvent.click(await screen.findByRole('switch', { name: 'Turn off example.atlassian.net' }));
    await waitFor(() => expect(screen.getByTestId('tracker-connection-status').textContent).toBe('Off'));
    const on = screen.getByRole('switch', { name: 'Turn on example.atlassian.net' }) as HTMLButtonElement;
    await waitFor(() => expect(on.disabled).toBe(false));
    await fireEvent.click(on);
    await waitFor(() => expect(screen.getByTestId('tracker-connection-status').textContent).toContain('Signed in as'));
    expect(set.mock.calls).toEqual([['jira-1', false], ['jira-1', true]]);
  });

  it('renames a connection, and shows a failed rename under its row', async () => {
    rows = [trackerConnection()];
    const answers: (() => Promise<TrackerConnection>)[] = [
      async () => { throw new TransportError('tracker_connection_invalid', 'the label holds a control character', { detail: { ref: 'r', method: 'RenameTrackerConnection', at: 1, chain: [] } }); },
      async () => {
        rows = [trackerConnection({ label: 'Work' })];
        return rows[0];
      },
    ];
    const rename = setBindingMock('RenameTrackerConnection', () => answers.shift()!());
    render(TrackerConnectionsSettings);
    await openMenu('jira-1');
    await fireEvent.click(await screen.findByTestId('tracker-connection-menu-rename'));
    const input = screen.getByTestId('tracker-connection-rename-input');
    await fireEvent.input(input, { target: { value: 'Work' } });
    await fireEvent.submit(input.closest('form')!);
    expect((await screen.findByTestId('tracker-connection-error')).textContent).toBe('The label holds a control character.');
    await fireEvent.submit(input.closest('form')!);
    await waitFor(() => expect(screen.getByTestId('tracker-connection-name').textContent).toBe('Work'));
    expect(screen.queryByTestId('tracker-connection-error')).toBeNull();
    expect(rename.mock.calls).toEqual([['jira-1', 'Work'], ['jira-1', 'Work']]);
  });

  it('adds Tempo from its Jira row with the default address, and nests it there', async () => {
    rows = [trackerConnection()];
    const add = setBindingMock('AddTrackerConnection', async () => {
      rows = [rows[0], fresh({ id: 'tempo-1', kind: 'tempo', site: 'https://api.tempo.io', account: '', parentId: 'jira-1' })];
      return rows[1];
    });
    render(TrackerConnectionsSettings);
    await openMenu('jira-1');
    await fireEvent.click(await screen.findByTestId('tracker-connection-menu-add-tempo'));
    await waitFor(() => expect(add).toHaveBeenCalledWith({ kind: 'tempo', site: '', parentId: 'jira-1' }));
    const jira = screen.getByTestId('tracker-connection-jira-1');
    const tempo = await within(jira).findByTestId('tracker-connection-tempo-1');
    expect(within(tempo).getByTestId('tracker-connection-name').textContent).toBe('Tempo time logging');
    expect(within(tempo).getByTestId('tracker-connection-open-token-page').textContent).toContain('Open Jira');

    await openMenu('jira-1');
    expect(screen.queryByTestId('tracker-connection-menu-add-tempo')).toBeNull();
  });

  it('shows a checked Tempo connection as checked, naming nobody', async () => {
    rows = [trackerConnection(), trackerConnection({ id: 'tempo-1', kind: 'tempo', site: 'https://api.tempo.io', account: '', parentId: 'jira-1', verifiedAccountId: '', verifiedName: '', verifiedAt: Date.now() - 5 * 60_000 })];
    render(TrackerConnectionsSettings);
    const tempo = await screen.findByTestId('tracker-connection-tempo-1');
    expect(within(tempo).getByTestId('tracker-connection-status').textContent).toBe('Token checked · checked 5m ago');
  });

  it('removes a connection only once the dialog naming what goes is confirmed', async () => {
    rows = [trackerConnection(), fresh({ id: 'tempo-1', kind: 'tempo', parentId: 'jira-1', account: '' })];
    const remove = setBindingMock('RemoveTrackerConnection', async () => {
      rows = [];
    });
    render(TrackerConnectionsSettings);
    await openMenu('jira-1');
    await fireEvent.click(await screen.findByTestId('tracker-connection-menu-remove'));
    const dialog = await screen.findByRole('dialog');
    expect(dialog.textContent).toContain('deletes its key file, its Tempo time logging and every board a project lists from it');
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    expect(remove).not.toHaveBeenCalled();

    await openMenu('jira-1');
    await fireEvent.click(await screen.findByTestId('tracker-connection-menu-remove'));
    await fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Remove' }));
    await waitFor(() => expect(remove).toHaveBeenCalledWith('jira-1'));
    await screen.findByTestId('settings-tracker-connections-empty');
  });

  it('opens the token page and the key file on this computer', async () => {
    rows = [fresh()];
    const page = setBindingMock('OpenTrackerTokenPage', async () => undefined);
    const file = setBindingMock('EditTrackerKeyFile', async () => {
      rows = [fresh({ keyFileState: KeyFileState.StateEmpty })];
      return rows[0];
    });
    render(TrackerConnectionsSettings);
    await fireEvent.click(await screen.findByTestId('tracker-connection-open-token-page'));
    await fireEvent.click(screen.getByTestId('tracker-connection-open-key-file'));
    await waitFor(() => expect(page).toHaveBeenCalledWith('jira-1'));
    await waitFor(() => expect(file).toHaveBeenCalledWith('jira-1'));
  });

  it('elsewhere, says to make the token on the computer and keeps the path and Check', async () => {
    await pairWithScopes(['tracker:read', 'tracker:operate']);
    rows = [fresh()];
    const verify = setBindingMock('VerifyTrackerConnection', async () => rows[0]);
    render(TrackerConnectionsSettings);
    const row = await screen.findByTestId('tracker-connection-jira-1');
    expect(within(row).getByTestId('tracker-connection-setup-remote')).toBeTruthy();
    expect(within(row).queryByTestId('tracker-connection-open-token-page')).toBeNull();
    expect(within(row).queryByTestId('tracker-connection-open-key-file')).toBeNull();
    expect(within(row).getByTestId('tracker-connection-key-path').textContent).toBe(rows[0].keyFilePath);
    await fireEvent.click(within(row).getByTestId('tracker-connection-setup-check'));
    await waitFor(() => expect(verify).toHaveBeenCalledWith('jira-1'));
    await openMenu('jira-1');
    expect(screen.queryByTestId('tracker-connection-menu-key-file')).toBeNull();
  });

  it('shows the list but no control that changes it to a device that may only read', async () => {
    await pairWithScopes(['tracker:read']);
    rows = [trackerConnection()];
    render(TrackerConnectionsSettings);
    const row = await screen.findByTestId('tracker-connection-jira-1');
    expect((within(row).getByRole('switch') as HTMLButtonElement).disabled).toBe(true);
    expect((within(row).getByTestId('tracker-connection-check') as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByTestId('tracker-connection-add') as HTMLButtonElement).disabled).toBe(true);
  });

  it('says so to a device that may not read the connections, and asks nothing', async () => {
    await pairWithScopes(['threads:read']);
    const read = setBindingMock('TrackerConnections', vi.fn(async () => rows));
    render(TrackerConnectionsSettings);
    await screen.findByTestId('settings-tracker-connections-refused');
    expect(read).not.toHaveBeenCalled();
  });

  it('shows a failed read with Try again', async () => {
    const answers: (() => Promise<TrackerConnection[]>)[] = [
      async () => { throw new TransportError('method_error', 'store: disk I/O error', { detail: { ref: 'r', method: 'TrackerConnections', at: 1, chain: ['store: disk I/O error'] } }); },
      async () => [trackerConnection()],
    ];
    setBindingMock('TrackerConnections', () => answers.shift()!());
    render(TrackerConnectionsSettings);
    expect((await screen.findByTestId('settings-tracker-connections-error')).textContent).toBe('Store: disk I/O error.');
    await fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    await screen.findByTestId('tracker-connection-jira-1');
    expect(screen.queryByTestId('settings-tracker-connections-error')).toBeNull();
  });
});
