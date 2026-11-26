// Settings > Issues > Status groups: each status of a project's boards
// with the tracker's own group, a pick saved at once under the revision
// read, a status put back by picking the tracker's group, the computer's
// refusal and failure said in its words with what is stored shown, a
// setup that cannot be read and one a newer build saved said where the
// groups would be, boards not read yet named, and the page following
// another window's save.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import type { Setup, SetupView, SiteStatuses } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import TrackerStatusesSettings from './TrackerStatusesSettings.svelte';
import ComputerSettingsPage from './ComputerSettingsPage.svelte';
import { SCOPES_AT_V0_0_19, answerAsV0019 } from '../../../test/helpers/olderComputer';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { readSidebarCatalogs } from '../../../test/helpers/sidebarCatalogs';
import { trackerConnection } from '../../../test/helpers/trackerConnections';
import { noteProject } from '../../transport/entityIndex';
import { HOME_BACKEND } from '../../transport/backendKey';
import { TransportError } from '../../transport/wsClient';
import { setCarriedSessionScopes } from '../../transport/scopes';
import { addProjectLocal } from '../../stores/projects.svelte';
import { trackerSetupUpdated } from '../../stores/trackerBoardEvents';

const SITE = 'https://example.atlassian.net';

let stored: SetupView;
let sites: SiteStatuses[];
let saves: { revision: string; setup: Setup }[];

function setupWith(groups: Record<string, string>, revision: string): SetupView {
  return {
    revision,
    setup: { status_groups: Object.keys(groups).length ? [{ tracker: 'jira', site: SITE, statuses: groups }] : [], ticket_pins: [], start_work: [] },
  } as SetupView;
}

beforeEach(async () => {
  setBindingMock('ListProjects', async () => []);
  await readSidebarCatalogs();
  noteProject('p1', '');
  addProjectLocal({ id: 'p1', name: 'web', path: '/repos/web', createdAt: 1, updatedAt: 1, sortPosition: 0, archived: false });
  setBindingMock('TrackerConnections', async () => [trackerConnection()]);
  stored = setupWith({}, '');
  sites = [{
    tracker: 'jira', site: SITE,
    statuses: [{ id: '10000', name: 'To Do', group: 'todo' }, { id: '10001', name: 'In Progress', group: 'in_progress' }, { id: '10002', name: 'Done', group: 'done' }],
  }];
  saves = [];
  setBindingMock('ProjectTrackerSetup', async () => stored);
  setBindingMock('ProjectTrackerStatuses', async () => ({ sites, places: [] }));
  setBindingMock('SetProjectTrackerSetup', async (_project: string, revision: string, setup: Setup) => {
    saves.push({ revision, setup });
    if (revision !== stored.revision) return { saved: false, refused: "this change was not saved: the project's tracker setup changed after it was read; it is shown as it is now", stored };
    stored = { revision: `r${saves.length}`, setup } as SetupView;
    return { saved: true, stored };
  });
});

function row(status: string): HTMLElement {
  return screen.getAllByTestId('tracker-status-row').find((el) => el.dataset.status === status)!;
}

function groupOf(status: string): HTMLSelectElement {
  return within(row(status)).getByTestId('tracker-status-group') as HTMLSelectElement;
}

async function shown(): Promise<void> {
  render(TrackerStatusesSettings);
  await screen.findAllByTestId('tracker-status-row');
}

describe('<TrackerStatusesSettings>', () => {
  it('lists each status with the tracker\'s group, saves a pick, and puts it back with the tracker\'s group', async () => {
    await shown();
    expect(screen.getAllByTestId('tracker-status-name').map((el) => el.textContent)).toEqual(['To Do', 'In Progress', 'Done']);
    expect(within(row('10001')).getByTestId('tracker-status-own').textContent).toBe('Jira: In progress');
    const options = Array.from(groupOf('10001').options).map((o) => o.textContent);
    expect(options).toEqual(['In progress (Jira\'s)', 'Backlog', 'To do', 'Done', 'Canceled', 'Left out of counts']);

    await fireEvent.change(groupOf('10001'), { target: { value: 'left_out' } });
    await waitFor(() => expect(within(row('10001')).queryByTestId('tracker-status-changed')).not.toBeNull());
    expect(saves).toEqual([{ revision: '', setup: { status_groups: [{ tracker: 'jira', site: SITE, statuses: { 10001: 'left_out' } }], ticket_pins: [], start_work: [] } }]);
    expect(groupOf('10001').value).toBe('left_out');

    await fireEvent.change(groupOf('10001'), { target: { value: '' } });
    await waitFor(() => expect(within(row('10001')).queryByTestId('tracker-status-changed')).toBeNull());
    expect(saves[1]).toEqual({ revision: 'r1', setup: { status_groups: [], ticket_pins: [], start_work: [] } });
    expect(groupOf('10001').value).toBe('');
  });

  it('keeps the other sections and sites a save does not change', async () => {
    stored = {
      revision: 'r0',
      setup: {
        status_groups: [{ tracker: 'linear', site: 'example', statuses: { s1: 'done' } }],
        ticket_pins: [{ tracker: 'jira', site: SITE, fields: ['customfield_1'] }],
        start_work: [{ tracker: 'jira', site: SITE, project: 'WEB', assign: false }],
      },
    } as SetupView;
    await shown();
    await fireEvent.change(groupOf('10000'), { target: { value: 'backlog' } });
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0].setup).toEqual({
      status_groups: [{ tracker: 'linear', site: 'example', statuses: { s1: 'done' } }, { tracker: 'jira', site: SITE, statuses: { 10000: 'backlog' } }],
      ticket_pins: [{ tracker: 'jira', site: SITE, fields: ['customfield_1'] }],
      start_work: [{ tracker: 'jira', site: SITE, project: 'WEB', assign: false }],
    });
  });

  it('says a save that lost to another was not saved, and shows the groups stored', async () => {
    await shown();
    // Another window saved since, and this window has not heard it yet.
    stored = setupWith({ 10002: 'canceled' }, 'other');
    await fireEvent.change(groupOf('10001'), { target: { value: 'done' } });
    const note = await screen.findByTestId('settings-tracker-statuses-save-note');
    expect(note.textContent?.trim()).toBe("This change was not saved: the project's tracker setup changed after it was read; it is shown as it is now. Make the change again.");
    await waitFor(() => expect(groupOf('10001').value, 'the pick is not shown as made').toBe(''));
    expect(groupOf('10002').value).toBe('canceled');
    expect(within(row('10002')).queryByTestId('tracker-status-changed')).not.toBeNull();
  });

  it('says a save whose outcome is unknown in the computer\'s words, and reads the setup again', async () => {
    await shown();
    const sentence = "this change may or may not be stored: the computer that holds the project could not finish updating /x/tracker.json (sync failed); read the project's tracker setup again before you change it again";
    setBindingMock('SetProjectTrackerSetup', async () => {
      stored = setupWith({ 10001: 'done' }, 'r9');
      throw new TransportError('tracker_setup_unsure', sentence, { detail: { ref: 'r1', method: 'SetProjectTrackerSetup', at: 1, chain: [sentence] } });
    });
    await fireEvent.change(groupOf('10001'), { target: { value: 'done' } });
    const note = await screen.findByTestId('settings-tracker-statuses-save-note');
    expect(note.textContent?.trim()).toBe(`T${sentence.slice(1)}.`);
    await waitFor(() => expect(groupOf('10001').value, 'what the file holds after it').toBe('done'));
  });

  it('follows another window\'s save on tracker:setup_updated', async () => {
    await shown();
    stored = setupWith({ 10000: 'left_out' }, 'r5');
    trackerSetupUpdated(HOME_BACKEND);
    await waitFor(() => expect(groupOf('10000').value).toBe('left_out'));
  });

  it('says a setup file that cannot be read, offers Try again and no select', async () => {
    const unreadable = new TransportError('tracker_setup_unreadable', "this project's tracker setup cannot be read from /x/tracker.json on the computer that holds the project (unexpected end of JSON input); fix the file or remove it, and it is read again at its next use");
    setBindingMock('ProjectTrackerSetup', async () => { throw unreadable; });
    render(TrackerStatusesSettings);
    const error = await screen.findByTestId('settings-tracker-statuses-error');
    expect(error.textContent).toContain('/x/tracker.json');
    expect(screen.queryAllByTestId('tracker-status-group')).toHaveLength(0);
    setBindingMock('ProjectTrackerSetup', async () => stored);
    await fireEvent.click(screen.getByTestId('settings-tracker-statuses-retry'));
    await screen.findAllByTestId('tracker-status-group');
    expect(screen.queryByTestId('settings-tracker-statuses-error')).toBeNull();
  });

  it('says a newer build saved the status groups, and offers no select', async () => {
    stored = { revision: 'r1', setup: { status_groups: [], ticket_pins: [], start_work: [] }, newer: ['status_groups'] } as SetupView;
    render(TrackerStatusesSettings);
    expect((await screen.findByTestId('settings-tracker-statuses-newer')).textContent).toContain('update the app on this computer');
    expect(screen.queryAllByTestId('tracker-status-group')).toHaveLength(0);
  });

  it('names a board not read yet, and lists its statuses once it has been', async () => {
    sites = [{ tracker: 'jira', site: SITE, statuses: [], unread: [{ board: '7', name: 'WEB board' }] }];
    render(TrackerStatusesSettings);
    expect((await screen.findByTestId('tracker-status-unread')).textContent).toContain('WEB board has not been read on this computer yet. Open it once in the Issues pane');
    sites = [{ tracker: 'jira', site: SITE, statuses: [{ id: '10000', name: 'To Do', group: 'todo' }] }];
    await fireEvent.click(screen.getByTestId('tracker-status-unread-retry'));
    await screen.findAllByTestId('tracker-status-row');
    expect(screen.queryByTestId('tracker-status-unread')).toBeNull();
  });

  it('puts back a status regrouped that no board shows now', async () => {
    stored = setupWith({ 10009: 'done' }, 'r1');
    sites = [{ tracker: 'jira', site: SITE, statuses: [], elsewhere: [{ id: '10009' }] }];
    render(TrackerStatusesSettings);
    const elsewhere = await screen.findByTestId('tracker-status-elsewhere');
    expect(elsewhere.textContent).toContain('10009');
    expect(elsewhere.textContent).toContain('Done');
    await fireEvent.click(within(elsewhere).getByTestId('tracker-status-put-back'));
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0]).toEqual({ revision: 'r1', setup: { status_groups: [], ticket_pins: [], start_work: [] } });
  });

  it('says a project lists no boards', async () => {
    sites = [];
    render(TrackerStatusesSettings);
    await screen.findByTestId('settings-tracker-statuses-empty');
  });

  describe('on an attached computer', () => {
    beforeEach(() => {
      stageBackend();
      noteProject('p1', 'laptop');
    });
    afterEach(() => {
      resetStagedBackends();
    });

    it('offers no change to a device that may not operate its trackers', async () => {
      setCarriedSessionScopes('laptop', ['tracker:read']);
      render(ComputerSettingsPage, { props: { backend: 'laptop', Page: TrackerStatusesSettings, needsComputer: false } });
      await screen.findAllByTestId('tracker-status-row');
      expect(groupOf('10000').disabled).toBe(true);
      expect(groupOf('10000').title).toBe("This device may not change a project's status groups on this computer.");
    });

    it('says a computer on v0.0.19 has no issue trackers', async () => {
      setCarriedSessionScopes('laptop', SCOPES_AT_V0_0_19);
      answerAsV0019();
      render(ComputerSettingsPage, { props: { backend: 'laptop', Page: TrackerStatusesSettings, needsComputer: false } });
      expect((await screen.findByTestId('settings-older-computer')).textContent).toBe('Laptop runs an older version: it has no issue trackers.');
    });
  });
});
