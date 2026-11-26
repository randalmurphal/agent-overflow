// Settings > Issues > Status groups with the keyboard, in a real browser:
// a status's group changed from its focused select keeps the focus there
// after the save, so the next one is changed in turn, and a refused change
// shows the stored group in the select that keeps it.

import { afterEach, beforeEach, expect, it } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/svelte';
import { userEvent } from 'vitest/browser';
import type { Setup, SetupView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import TrackerStatusesSettings from './TrackerStatusesSettings.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { readSidebarCatalogs } from '../../../test/helpers/sidebarCatalogs';
import { raf } from '../../../test/helpers/browserFrames';
import { trackerConnection } from '../../../test/helpers/trackerConnections';
import { noteProject } from '../../transport/entityIndex';
import { addProjectLocal } from '../../stores/projects.svelte';

const SITE = 'https://example.atlassian.net';
const REFUSED = "this change was not saved: the project's tracker setup changed after it was read; it is shown as it is now";

let stored: SetupView;
let saves: { revision: string; setup: Setup }[];

beforeEach(async () => {
  setBindingMock('ListProjects', async () => []);
  await readSidebarCatalogs();
  noteProject('p1', '');
  addProjectLocal({ id: 'p1', name: 'web', path: '/repos/web', createdAt: 1, updatedAt: 1, sortPosition: 0, archived: false });
  setBindingMock('TrackerConnections', async () => [trackerConnection()]);
  stored = { revision: '', setup: { status_groups: [], ticket_pins: [], start_work: [] } } as SetupView;
  saves = [];
  setBindingMock('ProjectTrackerSetup', async () => stored);
  setBindingMock('ProjectTrackerStatuses', async () => [{
    tracker: 'jira', site: SITE,
    statuses: [{ id: '10000', name: 'To Do', group: 'todo' }, { id: '10001', name: 'In Progress', group: 'in_progress' }, { id: '10002', name: 'Done', group: 'done' }],
  }]);
  setBindingMock('SetProjectTrackerSetup', async (_project: string, revision: string, setup: Setup) => {
    saves.push({ revision, setup });
    if (revision !== stored.revision) return { saved: false, refused: REFUSED, stored };
    stored = { revision: `r${saves.length}`, setup } as SetupView;
    return { saved: true, stored };
  });
});

afterEach(() => cleanup());

function select(status: string): HTMLSelectElement {
  const row = screen.getAllByTestId('tracker-status-row').find((el) => el.dataset.status === status)!;
  return within(row).getByTestId('tracker-status-group') as HTMLSelectElement;
}

async function shown(): Promise<void> {
  render(TrackerStatusesSettings);
  await waitFor(() => expect(screen.getAllByTestId('tracker-status-row')).toHaveLength(3));
  await waitFor(() => expect(select('10000').disabled).toBe(false));
}

it('keeps the focus on a select while its change saves and after, and changes the next from there', async () => {
  await shown();
  // The first save is held, so the page shows it saving.
  let release!: () => void;
  const held = new Promise<void>((resolve) => { release = resolve; });
  const answer = setBindingMock('SetProjectTrackerSetup', async (_project: string, revision: string, setup: Setup) => {
    saves.push({ revision, setup });
    if (saves.length === 1) await held;
    stored = { revision: `r${saves.length}`, setup } as SetupView;
    return { saved: true, stored };
  });
  select('10000').focus();
  await userEvent.keyboard('{ArrowDown}');
  await waitFor(() => expect(saves).toHaveLength(1));
  await raf();
  expect(document.activeElement, 'the focus stays while it saves').toBe(select('10000'));
  release();
  await waitFor(() => expect(answer).toHaveBeenCalledTimes(1));
  await waitFor(() => expect(select('10000').value).toBe('backlog'));
  expect(document.activeElement, 'the focus stays on the select that saved').toBe(select('10000'));

  await userEvent.keyboard('{Tab}');
  expect(document.activeElement).toBe(select('10001'));
  await userEvent.keyboard('{ArrowDown}');
  await waitFor(() => expect(saves).toHaveLength(2));
  expect(saves[1].revision).toBe('r1');
  expect(saves[1].setup.status_groups).toEqual([{ tracker: 'jira', site: SITE, statuses: { 10000: 'backlog', 10001: 'backlog' } }]);
  await waitFor(() => expect(select('10001').value).toBe('backlog'));
  expect(document.activeElement).toBe(select('10001'));
});

it('shows the stored group in a select whose change was refused, keeping its focus', async () => {
  await shown();
  // Another window saved since, and this window has not heard it yet.
  stored = { revision: 'other', setup: { status_groups: [{ tracker: 'jira', site: SITE, statuses: { 10002: 'canceled' } }], ticket_pins: [], start_work: [] } } as SetupView;
  select('10000').focus();
  await userEvent.keyboard('{ArrowDown}');
  await screen.findByTestId('settings-tracker-statuses-save-note');
  await waitFor(() => expect(select('10000').disabled).toBe(false));
  expect(select('10000').value, 'the pick is not shown as made').toBe('');
  expect(select('10002').value).toBe('canceled');
  expect(document.activeElement).toBe(select('10000'));
});
