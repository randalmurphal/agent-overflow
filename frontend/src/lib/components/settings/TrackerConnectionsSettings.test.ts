import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import TrackerConnectionsSettings from './TrackerConnectionsSettings.svelte';
import ComputerSettingsPage from './ComputerSettingsPage.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { pairWithScopes, resetToLocalPage } from '../../../test/helpers/scopes';
import { KeyFileState, trackerConnection, trackerProblem } from '../../../test/helpers/trackerConnections';
import { SCOPES_AT_V0_0_19, answerAsV0019 } from '../../../test/helpers/olderComputer';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { DisconnectedError, TransportError } from '../../transport/wsClient';
import { setCarriedSessionScopes } from '../../transport/scopes';
import { HOME_BACKEND } from '../../transport/backendKey';
import { applyTrackerConnectionsChanged } from '../../stores/trackerConnections.svelte';
import type { TrackerConnection } from '../../stores/bindings';
import { FailureKind } from '../../../../bindings/agent-overflow/internal/trackerapp/models';

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
  trackerConnection({ verifiedAt: 0, verifiedName: '', verifiedAccountId: '', checkedTokenInFile: false, keyFileState: KeyFileState.StateEmpty, ...overrides });

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
    expect(within(row).getByTestId('tracker-connection-status').textContent).toBe('Not checked');
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

  // Renewing means a new token's expiry is saved before the check: the
  // steps stay open until the check, or Done.
  it('keeps the renewal steps open through a saved expiry until the check closes them', async () => {
    const soon = Date.now() + 3 * 24 * 60 * 60 * 1000;
    rows = [trackerConnection({ tokenExpiresAt: soon })];
    setBindingMock('SetTrackerConnectionTokenExpiry', async (_id: string, at: number) => {
      rows = [trackerConnection({ tokenExpiresAt: at })];
      return rows[0];
    });
    const verify = setBindingMock('VerifyTrackerConnection', async () => rows[0]);
    render(TrackerConnectionsSettings);
    const row = await screen.findByTestId('tracker-connection-jira-1');
    await fireEvent.click(within(row).getByTestId('tracker-connection-renew'));
    const setup = within(row).getByTestId('tracker-connection-setup');
    await fireEvent.input(within(setup).getByTestId('tracker-token-expiry-date'), { target: { value: '2031-05-20' } });
    await fireEvent.click(within(setup).getByTestId('tracker-token-expiry-save'));
    await waitFor(() => expect(within(row).getByTestId('tracker-connection-expiry').textContent).toMatch(/^Token good until /));
    expect(within(row).getByTestId('tracker-connection-setup')).toBeTruthy();
    const checkStep = within(row).getByTestId('tracker-connection-setup-check') as HTMLButtonElement;
    await waitFor(() => expect(checkStep.disabled).toBe(false));
    await fireEvent.click(checkStep);
    await waitFor(() => expect(verify).toHaveBeenCalledWith('jira-1'));
    await waitFor(() => expect(within(row).queryByTestId('tracker-connection-setup')).toBeNull());
  });

  // Both Check buttons are one check: renewal ends when it verified, never
  // on a check whose answer carries a problem.
  it('keeps the renewal steps open after a check that did not verify, from either Check, and closes them on one that did', async () => {
    const base = trackerConnection({ tokenExpiresAt: Date.now() + 3 * 24 * 60 * 60 * 1000 });
    rows = [base];
    const failed = (): TrackerConnection => (rows = [{ ...base, problem: trackerProblem(FailureKind.FailureTransient, 'The tracker did not answer in time') }])[0];
    const verified = (): TrackerConnection => (rows = [base])[0];
    const answers: (() => TrackerConnection)[] = [failed, failed, verified];
    const verify = setBindingMock('VerifyTrackerConnection', async () => answers.shift()!());
    render(TrackerConnectionsSettings);
    const row = await screen.findByTestId('tracker-connection-jira-1');
    await fireEvent.click(within(row).getByTestId('tracker-connection-renew'));
    const rowCheck = within(row).getByTestId('tracker-connection-check') as HTMLButtonElement;
    const stepCheck = (): HTMLButtonElement => within(row).getByTestId('tracker-connection-setup-check') as HTMLButtonElement;

    await fireEvent.click(stepCheck());
    await waitFor(() => expect(verify).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(stepCheck().disabled).toBe(false));
    expect(within(row).getByTestId('tracker-connection-setup')).toBeTruthy();

    await fireEvent.click(rowCheck);
    await waitFor(() => expect(verify).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(rowCheck.disabled).toBe(false));
    expect(within(row).getByTestId('tracker-connection-setup')).toBeTruthy();

    await fireEvent.click(rowCheck);
    await waitFor(() => expect(verify).toHaveBeenCalledTimes(3));
    await waitFor(() => expect(within(row).queryByTestId('tracker-connection-setup')).toBeNull());
  });

  it('says a token in the key file that the last check did not verify is not checked, with its Check step, until a check verifies it', async () => {
    rows = [trackerConnection({ checkedTokenInFile: false })];
    const verify = setBindingMock('VerifyTrackerConnection', async () => {
      rows = [trackerConnection({ verifiedAt: 1_700_000_006_000, verifiedName: 'User 2', verifiedAccountId: 'acc-user2' })];
      return rows[0];
    });
    render(TrackerConnectionsSettings);
    const row = await screen.findByTestId('tracker-connection-jira-1');
    expect(within(row).getByTestId('tracker-connection-status').textContent).toBe('Token not checked');
    expect(within(row).getByTestId('tracker-connection-detail').textContent).toBe(
      'It was last checked as User 1 (user1@example.com). The token in the key file now has not been checked; check the connection to see whose it is.',
    );
    await fireEvent.click(within(row).getByTestId('tracker-connection-setup-check'));
    await waitFor(() => expect(verify).toHaveBeenCalledWith('jira-1'));
    await waitFor(() => expect(within(row).getByTestId('tracker-connection-status').textContent).toMatch(/^Signed in as User 2 /));
    expect(within(row).queryByTestId('tracker-connection-setup')).toBeNull();
  });

  it('reads a refusal as history once a new token is saved, and shows the new token refused when a check refuses it', async () => {
    const refused = trackerProblem(FailureKind.FailureSetup, 'Jira refused the token in the key file; put a valid token in it');
    rows = [trackerConnection({ checkedTokenInFile: false, problem: refused })];
    render(TrackerConnectionsSettings);
    const row = await screen.findByTestId('tracker-connection-jira-1');
    expect(within(row).getByTestId('tracker-connection-status').textContent).toBe('Needs setup');
    // The person saves a new token in the editor and comes back.
    rows = [trackerConnection({ checkedTokenInFile: false, problem: { ...refused, current: false } })];
    window.dispatchEvent(new Event('focus'));
    await waitFor(() => expect(within(row).getByTestId('tracker-connection-status').textContent).toBe('Key file changed since the last failure'));
    expect(within(row).getByTestId('tracker-connection-detail').textContent).toMatch(/^The token in it now has not been checked\. Before the change: Jira refused/);
    const verify = setBindingMock('VerifyTrackerConnection', async () => {
      rows = [trackerConnection({ checkedTokenInFile: false, problem: refused })];
      return rows[0];
    });
    await fireEvent.click(within(row).getByTestId('tracker-connection-setup-check'));
    await waitFor(() => expect(verify).toHaveBeenCalledWith('jira-1'));
    await waitFor(() => expect(within(row).getByTestId('tracker-connection-status').textContent).toBe('Needs setup'));
    expect(within(row).getByTestId('tracker-connection-detail').textContent).toMatch(/^Jira refused the token in the key file; put a valid token in it\./);
    expect(within(row).getByTestId('tracker-connection-setup')).toBeTruthy();
  });

  it('closes the renewal steps when the connection is turned off, and does not reopen them when it is on again', async () => {
    rows = [trackerConnection({ tokenExpiresAt: Date.now() + 3 * 24 * 60 * 60 * 1000 })];
    render(TrackerConnectionsSettings);
    const row = await screen.findByTestId('tracker-connection-jira-1');
    await fireEvent.click(within(row).getByTestId('tracker-connection-renew'));
    expect(within(row).getByTestId('tracker-connection-setup')).toBeTruthy();
    rows = [trackerConnection({ enabled: false, tokenExpiresAt: rows[0].tokenExpiresAt })];
    applyTrackerConnectionsChanged(HOME_BACKEND);
    await waitFor(() => expect(within(row).queryByTestId('tracker-connection-setup')).toBeNull());
    rows = [trackerConnection({ tokenExpiresAt: rows[0].tokenExpiresAt })];
    applyTrackerConnectionsChanged(HOME_BACKEND);
    await waitFor(() => expect(within(row).getByTestId('tracker-connection-renew')).toBeTruthy());
    expect(within(row).queryByTestId('tracker-connection-setup')).toBeNull();
  });

  it('offers Renew only where clicking it opens the steps: not on a connection that is off', async () => {
    rows = [trackerConnection({ enabled: false, tokenExpiresAt: Date.now() - 1000 })];
    render(TrackerConnectionsSettings);
    const row = await screen.findByTestId('tracker-connection-jira-1');
    expect(within(row).getByTestId('tracker-connection-expiry').dataset.tone).toBe('error');
    expect(within(row).queryByTestId('tracker-connection-renew')).toBeNull();
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
    // The removal takes the Tempo connection and its own key file too (trackerapp/connections.go Remove).
    expect(dialog.textContent).toContain(
      'This computer removes example.atlassian.net and its Tempo time logging connection, deletes both of their key files, and drops every board a project lists from example.atlassian.net.',
    );
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

  it('says so to a device the computer refuses, once it refused', async () => {
    await pairWithScopes(['threads:read']);
    const read = setBindingMock('TrackerConnections', vi.fn(async () => {
      throw new TransportError('scope_required', 'tracker:read required', { scope: 'tracker:read' });
    }));
    render(TrackerConnectionsSettings);
    await screen.findByTestId('settings-tracker-connections-refused');
    expect(read).toHaveBeenCalledTimes(1);
  });

  it('says a home computer on v0.0.19 has no issue trackers, with the grants that release gives', async () => {
    await pairWithScopes(SCOPES_AT_V0_0_19);
    answerAsV0019();
    render(TrackerConnectionsSettings);
    expect((await screen.findByTestId('settings-older-computer')).textContent).toBe('This computer runs an older version: it has no issue trackers.');
    expect(screen.queryByTestId('settings-tracker-connections-refused')).toBeNull();
  });

  it('shows a failed read with Try again', async () => {
    const answers: (() => Promise<TrackerConnection[]>)[] = [
      async () => { throw new TransportError('method_error', 'store: disk I/O error', { detail: { ref: 'r', method: 'TrackerConnections', at: 1, chain: ['store: disk I/O error'] } }); },
      async () => [trackerConnection()],
    ];
    setBindingMock('TrackerConnections', () => answers.shift()!());
    render(TrackerConnectionsSettings);
    expect((await screen.findByTestId('settings-tracker-connections-error')).textContent).toBe('Store: disk I/O error.');
    // A read that failed is not "no connections", and nothing can be added from it.
    expect(screen.queryByTestId('settings-tracker-connections-empty')).toBeNull();
    expect(screen.queryByTestId('tracker-connection-add')).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    await screen.findByTestId('tracker-connection-jira-1');
    expect(screen.queryByTestId('settings-tracker-connections-error')).toBeNull();
  });

  describe('on an attached computer', () => {
    beforeEach(() => {
      stageBackend();
    });
    afterEach(() => {
      resetStagedBackends();
    });

    // v0.0.19 grants a full-access device every scope it has, none of
    // them a tracker scope, and answers the tracker methods as missing.
    it('says a computer on v0.0.19 has no issue trackers, not that this device may not read them', async () => {
      setCarriedSessionScopes('laptop', SCOPES_AT_V0_0_19);
      answerAsV0019();
      const view = render(ComputerSettingsPage, { props: { backend: 'laptop', Page: TrackerConnectionsSettings, needsComputer: false } });
      expect((await view.findByTestId('settings-older-computer')).textContent).toBe('Laptop runs an older version: it has no issue trackers.');
      expect(view.queryByTestId('settings-tracker-connections-refused')).toBeNull();
      expect(view.queryByTestId('tracker-connection-add')).toBeNull();
    });

    it('shows an unreachable computer whose grant lacks the tracker scopes as a failure with Try again', async () => {
      setCarriedSessionScopes('laptop', SCOPES_AT_V0_0_19);
      const answers: (() => Promise<TrackerConnection[] | null>)[] = [
        async () => { throw new DisconnectedError('socket closed'); },
        async () => [trackerConnection()],
      ];
      setBindingMock('TrackerConnections', () => answers.shift()!());
      const view = render(ComputerSettingsPage, { props: { backend: 'laptop', Page: TrackerConnectionsSettings, needsComputer: false } });
      await view.findByTestId('settings-tracker-connections-error');
      expect(view.queryByTestId('settings-tracker-connections-refused')).toBeNull();
      await fireEvent.click(view.getByRole('button', { name: 'Try again' }));
      await view.findByTestId('tracker-connection-jira-1');
    });

    it('keeps a current computer’s refusal of tracker:read a refusal', async () => {
      setCarriedSessionScopes('laptop', ['threads:read']);
      setBindingMock('TrackerConnections', async () => {
        throw new TransportError('scope_required', 'tracker:read required', { scope: 'tracker:read' });
      });
      const view = render(ComputerSettingsPage, { props: { backend: 'laptop', Page: TrackerConnectionsSettings, needsComputer: false } });
      await view.findByTestId('settings-tracker-connections-refused');
      expect(view.queryByTestId('settings-older-computer')).toBeNull();
    });
  });
});
