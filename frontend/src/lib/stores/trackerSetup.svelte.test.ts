// A project's tracker setup as this window holds it: one read for every
// surface holding it, read again on tracker:setup_updated only while held,
// a save sent with the held revision and the edit of the held setup, a
// refusal that shows what is stored, a failure that never reports saved
// and reads again, and one save at a time.

import { describe, expect, it } from 'vitest';
import type { ForgeAccountInput, ForgeAccountsView, SetupView, SiteStatuses } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { trackerSetupUpdated } from './trackerBoardEvents';
import {
  acquireForgeAccounts,
  acquireTrackerSetup,
  acquireTrackerStatuses,
  acquireTrackerTicketCheck,
  editTrackerSetup,
  forgeAccountKey,
  forgeAccountsOf,
  refreshForgeAccounts,
  sayForgeAccount,
  trackerPeople,
  trackerSetupOf,
  trackerStatusesOf,
  trackerTicketCheckOf,
} from './trackerSetup.svelte';
import { __setBackendStatusForTest } from './transportStatus.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import { getPinnedBackend } from '../transport/backends';
import { TransportError } from '../transport/wsClient';
import { setBindingMock } from '../../test/mocks/bindings-app';

const SITE = 'https://example.atlassian.net';

function view(revision: string, groups: Record<string, string> = {}): SetupView {
  return {
    revision,
    setup: { status_groups: Object.keys(groups).length ? [{ tracker: 'jira', site: SITE, statuses: groups }] : [], ticket_pins: [], start_work: [] },
  } as SetupView;
}

async function flush(): Promise<void> {
  for (let i = 0; i < 5; i++) await Promise.resolve();
}

describe('the tracker setup store', () => {
  it('reads once for every holder, and again on the event only while held', async () => {
    const reads = setBindingMock('ProjectTrackerSetup', async () => view('r1'));
    const first = acquireTrackerSetup('p1');
    const second = acquireTrackerSetup('p1');
    await flush();
    expect(reads).toHaveBeenCalledTimes(1);
    expect(trackerSetupOf('p1')).toMatchObject({ view: { revision: 'r1' }, current: true, error: null });

    reads.mockImplementation(async () => view('r2', { s1: 'done' }));
    trackerSetupUpdated(HOME_BACKEND);
    await flush();
    expect(reads).toHaveBeenCalledTimes(2);
    expect(trackerSetupOf('p1').view?.revision).toBe('r2');

    first();
    second();
    expect(trackerSetupOf('p1').view).toBeNull();
    trackerSetupUpdated(HOME_BACKEND);
    await flush();
    expect(reads, 'nothing is read while no surface holds it').toHaveBeenCalledTimes(2);
  });

  it('keeps the view a failed read had, and says why', async () => {
    const reads = setBindingMock('ProjectTrackerSetup', async () => view('r1'));
    const release = acquireTrackerSetup('p1');
    await flush();
    const unreadable = new TransportError('tracker_setup_unreadable', "this project's tracker setup cannot be read from /x/tracker.json");
    reads.mockImplementation(async () => { throw unreadable; });
    trackerSetupUpdated(HOME_BACKEND);
    await flush();
    expect(trackerSetupOf('p1')).toMatchObject({ view: { revision: 'r1' }, current: false, error: unreadable });
    release();
  });

  it('saves the edit of the held setup under its revision, and shows what is stored when refused', async () => {
    setBindingMock('ProjectTrackerSetup', async () => view('r1', { s1: 'done' }));
    const release = acquireTrackerSetup('p1');
    await flush();
    const save = setBindingMock('SetProjectTrackerSetup', async () => ({
      saved: false,
      refused: "this change was not saved: the project's tracker setup changed after it was read; it is shown as it is now",
      stored: view('r2', { s2: 'left_out' }),
    }));
    const outcome = await editTrackerSetup('p1', (setup) => ({ ...setup, ticket_pins: [{ tracker: 'jira', site: SITE, fields: ['customfield_1'] }] }));
    expect(save).toHaveBeenCalledWith('p1', 'r1', {
      status_groups: [{ tracker: 'jira', site: SITE, statuses: { s1: 'done' } }],
      ticket_pins: [{ tracker: 'jira', site: SITE, fields: ['customfield_1'] }],
      start_work: [],
    });
    expect(outcome).toEqual({ kind: 'refused', sentence: expect.stringContaining('changed after it was read') });
    expect(trackerSetupOf('p1')).toMatchObject({ view: { revision: 'r2' }, current: true, saving: false });
    release();
  });

  it('never reports a failed save as saved, and reads the file again after it', async () => {
    const reads = setBindingMock('ProjectTrackerSetup', async () => view('r1'));
    const release = acquireTrackerSetup('p1');
    await flush();
    const unsure = new TransportError('tracker_setup_unsure', 'this change may or may not be stored');
    setBindingMock('SetProjectTrackerSetup', async () => { throw unsure; });
    reads.mockImplementation(async () => view('r3', { s1: 'done' }));
    await expect(editTrackerSetup('p1', (setup) => setup)).rejects.toBe(unsure);
    await flush();
    expect(reads).toHaveBeenCalledTimes(2);
    expect(trackerSetupOf('p1')).toMatchObject({ view: { revision: 'r3' }, current: true });
    release();
  });

  it('runs one save at a time for a project', async () => {
    setBindingMock('ProjectTrackerSetup', async () => view('r1'));
    const release = acquireTrackerSetup('p1');
    await flush();
    let answer!: () => void;
    setBindingMock('SetProjectTrackerSetup', () => new Promise((resolve) => { answer = () => resolve({ saved: true, stored: view('r2') }); }));
    const first = editTrackerSetup('p1', (setup) => setup);
    await flush();
    expect(trackerSetupOf('p1').saving).toBe(true);
    await expect(editTrackerSetup('p1', (setup) => setup)).rejects.toThrow(/still saving/);
    answer();
    await expect(first).resolves.toEqual({ kind: 'saved' });
    expect(trackerSetupOf('p1')).toMatchObject({ view: { revision: 'r2' }, saving: false });
    release();
  });

  it('reads a change told while a save runs once the save has settled, and lands that read', async () => {
    const reads = setBindingMock('ProjectTrackerSetup', async () => view('r1'));
    const release = acquireTrackerSetup('p1');
    await flush();
    let answer!: () => void;
    setBindingMock('SetProjectTrackerSetup', () => new Promise((resolve) => { answer = () => resolve({ saved: true, stored: view('r2', { s1: 'done' }) }); }));
    const saving = editTrackerSetup('p1', (setup) => setup);
    await flush();
    // Another window saved after this save's write; its answer lacks that.
    reads.mockImplementation(async () => view('r3', { s1: 'done', s2: 'left_out' }));
    trackerSetupUpdated(HOME_BACKEND);
    await flush();
    expect(reads, 'no read beside the save').toHaveBeenCalledTimes(1);
    answer();
    await expect(saving).resolves.toEqual({ kind: 'saved' });
    await flush();
    expect(reads, 'one read once it settled').toHaveBeenCalledTimes(2);
    expect(trackerSetupOf('p1')).toMatchObject({ view: { revision: 'r3' }, current: true });
    release();
  });

  it('runs one save at a time while the setup it is built from is still being read', async () => {
    let answerRead!: () => void;
    setBindingMock('ProjectTrackerSetup', () => new Promise((resolve) => { answerRead = () => resolve(view('r1')); }));
    const save = setBindingMock('SetProjectTrackerSetup', async () => ({ saved: true, stored: view('r2') }));
    const first = editTrackerSetup('p1', (setup) => setup);
    const second = editTrackerSetup('p1', (setup) => setup);
    await flush();
    answerRead();
    await expect(first).resolves.toEqual({ kind: 'saved' });
    await expect(second).rejects.toThrow(/still saving/);
    expect(save).toHaveBeenCalledTimes(1);
  });

  it('reads the setup first for a save no surface holds, and holds nothing after it', async () => {
    const reads = setBindingMock('ProjectTrackerSetup', async () => view('r5'));
    const save = setBindingMock('SetProjectTrackerSetup', async () => ({ saved: true, stored: view('r6') }));
    await expect(editTrackerSetup('p1', (setup) => setup)).resolves.toEqual({ kind: 'saved' });
    expect(reads).toHaveBeenCalledTimes(1);
    expect(save).toHaveBeenCalledWith('p1', 'r5', expect.anything());
    expect(trackerSetupOf('p1').view).toBeNull();
  });

  it('reads the setup and the statuses again when their computer connects again, only while held', async () => {
    const reads = setBindingMock('ProjectTrackerSetup', async () => view('r1'));
    const statusReads = setBindingMock('ProjectTrackerStatuses', async () => ({ sites: [], places: [] }));
    const releaseSetup = acquireTrackerSetup('p1');
    const releaseStatuses = acquireTrackerStatuses('p1');
    await flush();
    __setBackendStatusForTest(HOME_BACKEND, { status: 'disconnected', nextAttemptAt: null });
    reads.mockImplementation(async () => view('r2'));
    __setBackendStatusForTest(HOME_BACKEND, { status: 'connected', nextAttemptAt: null });
    await flush();
    expect([reads.mock.calls.length, statusReads.mock.calls.length]).toEqual([2, 2]);
    expect(trackerSetupOf('p1').view?.revision).toBe('r2');
    releaseSetup();
    releaseStatuses();
    __setBackendStatusForTest(HOME_BACKEND, { status: 'disconnected', nextAttemptAt: null });
    __setBackendStatusForTest(HOME_BACKEND, { status: 'connected', nextAttemptAt: null });
    await flush();
    expect([reads.mock.calls.length, statusReads.mock.calls.length]).toEqual([2, 2]);
  });

  it('holds the statuses while shown and reads them again on the event', async () => {
    const sites: SiteStatuses[] = [{ tracker: 'jira', site: SITE, statuses: [{ id: 's1', name: 'To Do', group: 'todo' }] }];
    const reads = setBindingMock('ProjectTrackerStatuses', async () => ({ sites, places: [] }));
    const release = acquireTrackerStatuses('p1');
    await flush();
    expect(trackerStatusesOf('p1').sites).toEqual(sites);
    trackerSetupUpdated(HOME_BACKEND);
    await flush();
    expect(reads).toHaveBeenCalledTimes(2);
    release();
    trackerSetupUpdated(HOME_BACKEND);
    await flush();
    expect(reads).toHaveBeenCalledTimes(2);
    expect(trackerStatusesOf('p1').sites).toBeNull();
  });

  it('checks tickets once for every holder on their computer, again when it connects again, not on the event, only while held', async () => {
    const pins: (string | null)[] = [];
    const lookups = setBindingMock('TrackerLookup', async () => {
      pins.push(getPinnedBackend());
      return { found: [], unmatched: ['OPS-1'] };
    });
    const first = acquireTrackerTicketCheck('p1', HOME_BACKEND, 'jira-1', ['OPS-1']);
    const second = acquireTrackerTicketCheck('p1', HOME_BACKEND, 'jira-1', ['OPS-1']);
    await flush();
    expect(lookups.mock.calls).toEqual([['jira-1', ['OPS-1']]]);
    expect(pins).toEqual([HOME_BACKEND]);
    expect(trackerTicketCheckOf('p1', HOME_BACKEND, 'jira-1', ['OPS-1']).found?.unmatched).toEqual(['OPS-1']);
    trackerSetupUpdated(HOME_BACKEND);
    await flush();
    expect(lookups).toHaveBeenCalledTimes(1);
    __setBackendStatusForTest(HOME_BACKEND, { status: 'disconnected', nextAttemptAt: null });
    __setBackendStatusForTest(HOME_BACKEND, { status: 'connected', nextAttemptAt: null });
    await flush();
    expect(lookups).toHaveBeenCalledTimes(2);
    first();
    second();
    expect(trackerTicketCheckOf('p1', HOME_BACKEND, 'jira-1', ['OPS-1']).found).toBeNull();
    __setBackendStatusForTest(HOME_BACKEND, { status: 'disconnected', nextAttemptAt: null });
    __setBackendStatusForTest(HOME_BACKEND, { status: 'connected', nextAttemptAt: null });
    await flush();
    expect(lookups).toHaveBeenCalledTimes(2);
  });
});

// A connection's forge accounts as the Points and credit page holds them:
// one read for every holder on the connection's computer, again when it
// connects again and on Read again; a person's word shows what the
// computer answered; the words of a connection go one at a time, a read
// asked for meanwhile runs once they have settled, and a word that failed
// rejects and reads what the computer holds.
describe('the forge accounts of a connection', () => {
  const accounts = (...logins: string[]): ForgeAccountsView =>
    ({ accounts: logins.map((login) => ({ host: 'github.com', login, how: 'nobody' })) }) as ForgeAccountsView;
  const word = (login: string): ForgeAccountInput => ({ host: 'github.com', login, who: 'not_a_person' }) as ForgeAccountInput;
  const logins = () => forgeAccountsOf(HOME_BACKEND, 'jira-1').view?.accounts.map((a) => a.login) ?? null;

  /** A binding whose every call waits for the test to answer it. */
  function held<T>(name: string) {
    const calls: { args: unknown[]; resolve: (value: T) => void; reject: (err: unknown) => void }[] = [];
    setBindingMock(name, (...args: unknown[]) => new Promise<T>((resolve, reject) => { calls.push({ args, resolve, reject }); }));
    return calls;
  }

  it('reads once for every holder on its computer, again on Read again and when the computer connects again, only while held', async () => {
    const pins: (string | null)[] = [];
    const reads = setBindingMock('TrackerForgeAccounts', async () => {
      pins.push(getPinnedBackend());
      return accounts('ghost');
    });
    const first = acquireForgeAccounts('p1', HOME_BACKEND, 'jira-1');
    const second = acquireForgeAccounts('p1', HOME_BACKEND, 'jira-1');
    expect(forgeAccountsOf(HOME_BACKEND, 'jira-1').reading).toBe(true);
    await flush();
    expect(reads.mock.calls).toEqual([['jira-1']]);
    expect(pins).toEqual([HOME_BACKEND]);
    expect(forgeAccountsOf(HOME_BACKEND, 'jira-1')).toMatchObject({ error: null, reading: false });
    expect(logins()).toEqual(['ghost']);

    reads.mockImplementation(async () => accounts('ghost', 'ci'));
    await refreshForgeAccounts(HOME_BACKEND, 'jira-1');
    expect(logins()).toEqual(['ghost', 'ci']);
    trackerSetupUpdated(HOME_BACKEND);
    await flush();
    expect(reads, 'a change of the setup reads no accounts').toHaveBeenCalledTimes(2);
    __setBackendStatusForTest(HOME_BACKEND, { status: 'disconnected', nextAttemptAt: null });
    __setBackendStatusForTest(HOME_BACKEND, { status: 'connected', nextAttemptAt: null });
    await flush();
    expect(reads).toHaveBeenCalledTimes(3);

    const failure = new TransportError('tracker_connection_missing', 'this tracker connection was removed from this computer');
    reads.mockImplementation(async () => { throw failure; });
    await refreshForgeAccounts(HOME_BACKEND, 'jira-1');
    expect(forgeAccountsOf(HOME_BACKEND, 'jira-1')).toMatchObject({ error: failure, reading: false });
    expect(logins(), 'a failed read keeps the accounts it had').toEqual(['ghost', 'ci']);

    first();
    second();
    expect(forgeAccountsOf(HOME_BACKEND, 'jira-1').view).toBeNull();
    await refreshForgeAccounts(HOME_BACKEND, 'jira-1');
    __setBackendStatusForTest(HOME_BACKEND, { status: 'disconnected', nextAttemptAt: null });
    __setBackendStatusForTest(HOME_BACKEND, { status: 'connected', nextAttemptAt: null });
    await flush();
    expect(reads, 'nothing is read while no surface holds them').toHaveBeenCalledTimes(4);
    await expect(sayForgeAccount(HOME_BACKEND, 'jira-1', word('ghost'))).rejects.toThrow('no longer shown');
  });

  it('shows what the computer answered a word, sends the words of a connection one at a time, and reads after them a read asked for meanwhile', async () => {
    const reads = setBindingMock('TrackerForgeAccounts', async () => accounts('read'));
    const release = acquireForgeAccounts('p1', HOME_BACKEND, 'jira-1');
    await flush();
    const sent = held<ForgeAccountsView>('SetTrackerForgeAccount');
    const saving = () => [...forgeAccountsOf(HOME_BACKEND, 'jira-1').saving];

    const one = sayForgeAccount(HOME_BACKEND, 'jira-1', word('ghost'));
    const two = sayForgeAccount(HOME_BACKEND, 'jira-1', word('ci'));
    await expect(sayForgeAccount(HOME_BACKEND, 'jira-1', word('ghost'))).rejects.toThrow('still saving');
    await flush();
    expect(sent.map((c) => c.args), 'the second word waits for the first').toEqual([['jira-1', word('ghost')]]);
    expect(saving()).toEqual([forgeAccountKey('github.com', 'ghost'), forgeAccountKey('github.com', 'ci')]);

    // Read again while the words are out: no read is sent until they settle.
    const again = refreshForgeAccounts(HOME_BACKEND, 'jira-1');
    await flush();
    expect(reads).toHaveBeenCalledTimes(1);

    sent[0].resolve(accounts('after-one'));
    await one;
    expect(logins()).toEqual(['after-one']);
    await flush();
    expect(sent.map((c) => c.args)).toEqual([['jira-1', word('ghost')], ['jira-1', word('ci')]]);
    expect(saving()).toEqual([forgeAccountKey('github.com', 'ci')]);
    sent[1].resolve(accounts('after-two'));
    await two;
    expect(logins()).toEqual(['after-two']);
    await again;
    await flush();
    expect(reads, 'the read asked for meanwhile runs once').toHaveBeenCalledTimes(2);
    expect(logins()).toEqual(['read']);
    expect(saving()).toEqual([]);
    release();
  });

  it('drops the answer of a read that was out when a word was sent', async () => {
    const reads = held<ForgeAccountsView>('TrackerForgeAccounts');
    const release = acquireForgeAccounts('p1', HOME_BACKEND, 'jira-1');
    await flush();
    setBindingMock('SetTrackerForgeAccount', async () => accounts('answered'));
    await sayForgeAccount(HOME_BACKEND, 'jira-1', word('ghost'));
    expect(logins()).toEqual(['answered']);
    reads[0].resolve(accounts('older'));
    await flush();
    expect(logins(), 'the read began before the word').toEqual(['answered']);
    release();
  });

  it('rejects a word that failed with its failure and reads what the computer holds', async () => {
    const reads = setBindingMock('TrackerForgeAccounts', async () => accounts('ghost'));
    const release = acquireForgeAccounts('p1', HOME_BACKEND, 'jira-1');
    await flush();
    const refused = new TransportError('tracker_request_invalid', 'nothing was sent: a forge account is named by its host and login');
    setBindingMock('SetTrackerForgeAccount', async () => { throw refused; });
    reads.mockImplementation(async () => accounts('ghost', 'held'));
    await expect(sayForgeAccount(HOME_BACKEND, 'jira-1', word('ghost'))).rejects.toBe(refused);
    await flush();
    expect(reads).toHaveBeenCalledTimes(2);
    expect(logins()).toEqual(['ghost', 'held']);
    expect([...forgeAccountsOf(HOME_BACKEND, 'jira-1').saving]).toEqual([]);
    release();
  });

  it("searches the tracker's people on the connection's computer", async () => {
    const pins: (string | null)[] = [];
    const searches = setBindingMock('TrackerPeople', async () => {
      pins.push(getPinnedBackend());
      return { users: [{ id: 'acc-user2', name: 'User 2' }], more: true };
    });
    await expect(trackerPeople(HOME_BACKEND, 'jira-1', ['10000'], 'use')).resolves.toEqual({ users: [{ id: 'acc-user2', name: 'User 2' }], more: true });
    expect(searches.mock.calls).toEqual([['jira-1', ['10000'], 'use']]);
    expect(pins).toEqual([HOME_BACKEND]);
  });
});
