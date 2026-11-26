// Settings > Issues > Starting work: one block per tracker project the
// project's boards name, and per one the setup holds defaults for; each
// row shows the defaults Start work reads (no move, assigned, the branch
// named {key}-{title}, the thread in the project) until set, and each
// change saves at once through the rule Start work's "Save as defaults"
// writes by: the move turned on at the first status counted in progress,
// the repository of the project itself stored as none, and naming the
// branch turned back on stored as none.

import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import type { Setup, SetupView, SiteStatuses } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import TrackerStartWorkSettings from './TrackerStartWorkSettings.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { readSidebarCatalogs } from '../../../test/helpers/sidebarCatalogs';
import { trackerBoardChoice, trackerConnection } from '../../../test/helpers/trackerConnections';
import { noteProject } from '../../transport/entityIndex';
import { addProjectLocal } from '../../stores/projects.svelte';

const SITE = 'https://example.atlassian.net';

let stored: SetupView;
let saves: Setup[];
let statuses: SiteStatuses[];

beforeEach(async () => {
  setBindingMock('ListProjects', async () => []);
  await readSidebarCatalogs();
  for (const [id, name] of [['p1', 'web'], ['p2', 'api']] as const) {
    noteProject(id, '');
    addProjectLocal({ id, name, path: `/repos/${name}`, createdAt: 1, updatedAt: 1, sortPosition: 0, archived: false });
  }
  setBindingMock('TrackerConnections', async () => [trackerConnection()]);
  setBindingMock('ProjectTrackerBoards', async () => [trackerBoardChoice()]);
  statuses = [{
    tracker: 'jira', site: SITE,
    statuses: [{ id: '10000', name: 'To Do', group: 'todo' }, { id: '10001', name: 'In Progress', group: 'in_progress' }, { id: '10003', name: 'In Review', group: 'in_progress' }],
  }];
  setBindingMock('ProjectTrackerStatuses', async () => statuses);
  stored = { revision: 'r0', setup: { status_groups: [], ticket_pins: [], start_work: [{ tracker: 'jira', site: SITE, project: 'OPS', branch: 'ops/{key}' }] } } as SetupView;
  saves = [];
  setBindingMock('ProjectTrackerSetup', async () => stored);
  setBindingMock('SetProjectTrackerSetup', async (_project: string, _revision: string, setup: Setup) => {
    saves.push(setup);
    stored = { revision: `r${saves.length}`, setup } as SetupView;
    return { saved: true, stored };
  });
});

/** Shows the page for project p1, beside p2 the repository picker offers. */
async function shown(): Promise<void> {
  render(TrackerStartWorkSettings);
  const select = await screen.findByTestId('settings-tracker-start-work-project');
  await fireEvent.change(select, { target: { value: 'p1' } });
  await screen.findAllByTestId('tracker-start-work-place');
}

function place(project: string): HTMLElement {
  return screen.getAllByTestId('tracker-start-work-place').find((el) => el.dataset.project === project)!;
}

function web(setup: Setup) {
  return setup.start_work?.find((d) => d.project === 'WEB');
}

describe('<TrackerStartWorkSettings>', () => {
  it('lists the boards\' tracker project and those with saved defaults, with Start work\'s defaults until set', async () => {
    await shown();
    expect(screen.getAllByTestId('tracker-start-work-place').map((el) => el.dataset.project)).toEqual(['WEB', 'OPS']);
    const w = within(place('WEB'));
    expect(w.getByRole('switch', { name: 'Move the ticket' }).getAttribute('aria-checked')).toBe('false');
    expect(w.queryByTestId('tracker-start-work-status')).toBeNull();
    expect(w.getByRole('switch', { name: 'Assign it to me' }).getAttribute('aria-checked')).toBe('true');
    expect(w.getByRole('switch', { name: 'Name the branch after the ticket' }).getAttribute('aria-checked')).toBe('true');
    expect(w.getByTestId('tracker-start-work-branch-example').textContent?.trim()).toBe('WEB-123-short-title');
    expect(within(place('OPS')).getByTestId('tracker-start-work-branch-example').textContent?.trim()).toBe('ops/OPS-123');
    expect((w.getByTestId('tracker-start-work-repository') as HTMLSelectElement).value).toBe('p1');
  });

  it('turns the move on at the first status counted in progress, and moves it to the one picked', async () => {
    stored = { ...stored, setup: { ...stored.setup, status_groups: [{ tracker: 'jira', site: SITE, statuses: { 10001: 'todo' } }] } };
    await shown();
    await fireEvent.click(within(place('WEB')).getByRole('switch', { name: 'Move the ticket' }));
    await waitFor(() => expect(saves).toHaveLength(1));
    // The project counts In Progress as to do, so In Review is the first in progress.
    expect(web(saves[0])).toEqual({ tracker: 'jira', site: SITE, project: 'WEB', status: '10003' });
    const picker = await within(place('WEB')).findByTestId('tracker-start-work-status') as HTMLSelectElement;
    expect(picker.value).toBe('10003');
    await fireEvent.change(picker, { target: { value: '10000' } });
    await waitFor(() => expect(saves).toHaveLength(2));
    expect(web(saves[1])?.status).toBe('10000');
    await fireEvent.click(within(place('WEB')).getByRole('switch', { name: 'Move the ticket' }));
    await waitFor(() => expect(saves).toHaveLength(3));
    expect(web(saves[2])?.status).toBeUndefined();
    expect(saves[2].start_work?.find((d) => d.project === 'OPS'), 'the other entry is kept').toEqual({ tracker: 'jira', site: SITE, project: 'OPS', branch: 'ops/{key}' });
  });

  it('saves assigning off, naming the branch off and on, and the repository, the project itself stored as none', async () => {
    await shown();
    const w = () => within(place('WEB'));
    await fireEvent.click(w().getByRole('switch', { name: 'Assign it to me' }));
    await waitFor(() => expect(web(stored.setup)?.assign).toBe(false));
    await fireEvent.click(w().getByRole('switch', { name: 'Name the branch after the ticket' }));
    await waitFor(() => expect(web(stored.setup)?.name_branch).toBe(false));
    expect(w().getByTestId('tracker-start-work-branch-example').textContent?.trim()).toBe('Start work opens with the branch box empty, for you to name it.');
    await fireEvent.click(w().getByRole('switch', { name: 'Name the branch after the ticket' }));
    await waitFor(() => expect(saves).toHaveLength(3));
    expect(JSON.parse(JSON.stringify(web(saves[2])))).toEqual({ tracker: 'jira', site: SITE, project: 'WEB', assign: false });
    await fireEvent.change(w().getByTestId('tracker-start-work-repository'), { target: { value: 'p2' } });
    await waitFor(() => expect(web(stored.setup)?.repository).toBe('p2'));
    await fireEvent.change(w().getByTestId('tracker-start-work-repository'), { target: { value: 'p1' } });
    await waitFor(() => expect(saves).toHaveLength(5));
    expect(web(saves[4])?.repository).toBeUndefined();
  });

  it('offers no move while no status of the tracker is listed', async () => {
    statuses = [];
    await shown();
    const move = within(place('WEB')).getByRole('switch', { name: 'Move the ticket' });
    expect(move).toBeDisabled();
    expect(move.title).toContain('No status of this tracker is listed yet');
  });

  it('says a project with no boards and no defaults has nothing to set', async () => {
    setBindingMock('ProjectTrackerBoards', async () => []);
    stored = { revision: '', setup: { status_groups: [], ticket_pins: [], start_work: [] } } as SetupView;
    render(TrackerStartWorkSettings);
    await screen.findByTestId('settings-tracker-start-work-empty');
  });
});
