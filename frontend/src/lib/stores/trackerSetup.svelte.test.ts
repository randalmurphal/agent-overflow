// A project's tracker setup as this window holds it: one read for every
// surface holding it, read again on tracker:setup_updated only while held,
// a save sent with the held revision and the edit of the held setup, a
// refusal that shows what is stored, a failure that never reports saved
// and reads again, and one save at a time.

import { describe, expect, it } from 'vitest';
import type { SetupView, SiteStatuses } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { trackerSetupUpdated } from './trackerBoardEvents';
import {
  acquireTrackerSetup,
  acquireTrackerStatuses,
  editTrackerSetup,
  trackerSetupOf,
  trackerStatusesOf,
} from './trackerSetup.svelte';
import { __setBackendStatusForTest } from './transportStatus.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
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
    const statusReads = setBindingMock('ProjectTrackerStatuses', async () => []);
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
    const reads = setBindingMock('ProjectTrackerStatuses', async () => sites);
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
});
