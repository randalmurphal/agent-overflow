// Settings > Issues > Fields: the pinned fields and sections of each Jira
// site of the project's boards and pins at once, in pin order; a field
// or a section not pinned found by name, the exact name first, at most 20
// drawn, two of one name told apart by ID; a pin or an unpin saved as the
// ticket's pin saves it (withTicketPin), with no read of the catalog; the
// sections a ticket opens while none are pinned shown as pinned; each
// site's catalog its own read, one that failed said with Try again and
// its pins by ID to take off; the sites following the setup and the
// connections while the page is open; and a Linear-only project, or one
// with no boards, said.

import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import type { PinField, Setup, SetupView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import TrackerFieldsSettings from './TrackerFieldsSettings.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { readSidebarCatalogs } from '../../../test/helpers/sidebarCatalogs';
import { trackerBoardChoice, trackerConnection } from '../../../test/helpers/trackerConnections';
import { noteProject } from '../../transport/entityIndex';
import { addProjectLocal } from '../../stores/projects.svelte';
import { withTicketPin } from '../../stores/trackerTicketModel';
import { trackerSetupUpdated } from '../../stores/trackerBoardEvents';
import { refreshTrackerConnections } from '../../stores/trackerConnections.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';

const SITE = 'https://example.atlassian.net';

/** A site's catalog of `n` fields, Field 1 to Field n, with two long
 * text fields. */
function catalog(n: number): PinField[] {
  const out: PinField[] = [];
  for (let i = 1; i <= n; i++) out.push({ id: `customfield_${10000 + i}`, name: `Field ${i}`, kind: 'text' });
  out.push({ id: 'customfield_20001', name: 'Acceptance criteria', kind: 'rich_text' });
  out.push({ id: 'customfield_20002', name: 'Test plan', kind: 'rich_text' });
  return out;
}

let stored: SetupView;
/** Each site's catalog as its read answers it, or the failure it throws. */
let catalogs: Record<string, PinField[] | Error>;
let reads: string[];
let saves: Setup[];

beforeEach(async () => {
  setBindingMock('ListProjects', async () => []);
  await readSidebarCatalogs();
  noteProject('p1', '');
  addProjectLocal({ id: 'p1', name: 'web', path: '/repos/web', createdAt: 1, updatedAt: 1, sortPosition: 0, archived: false });
  setBindingMock('TrackerConnections', async () => [trackerConnection()]);
  setBindingMock('ProjectTrackerBoards', async () => [trackerBoardChoice()]);
  catalogs = { [SITE]: catalog(400) };
  reads = [];
  setBindingMock('ProjectTrackerSiteFields', async (_project: string, tracker: string, site: string) => {
    reads.push(`${tracker} ${site}`);
    const answer = catalogs[site];
    if (answer instanceof Error) throw answer;
    if (!answer) throw new Error(`test: no catalog for ${site}`);
    return answer;
  });
  stored = { revision: 'r0', setup: { status_groups: [], ticket_pins: [{ tracker: 'jira', site: SITE, fields: ['customfield_10007', 'customfield_10003'] }], start_work: [] } } as SetupView;
  saves = [];
  setBindingMock('ProjectTrackerSetup', async () => stored);
  setBindingMock('SetProjectTrackerSetup', async (_project: string, _revision: string, setup: Setup) => {
    saves.push(setup);
    stored = { revision: `r${saves.length}`, setup } as SetupView;
    return { saved: true, stored };
  });
});

function list(kind: 'fields' | 'sections'): HTMLElement {
  return screen.getAllByTestId('tracker-fields-list').find((el) => el.dataset.kind === kind)!;
}

function names(within: HTMLElement, testId: string): string[] {
  return Array.from(within.querySelectorAll(`[data-testid="${testId}"]`)).map((el) => el.textContent ?? '');
}

describe('<TrackerFieldsSettings>', () => {
  it('shows the pinned at once and draws none of 400 fields before a name is typed', async () => {
    render(TrackerFieldsSettings);
    await screen.findAllByTestId('tracker-fields-find');
    expect(names(list('fields'), 'tracker-fields-pinned')).toEqual(['Field 7', 'Field 3']);
    // No section is pinned: the ticket's default ones show, as the ticket opens them.
    expect(names(list('sections'), 'tracker-fields-pinned')).toEqual(['Description', 'Linked tickets', 'Attachments']);
    expect(screen.queryAllByTestId('tracker-fields-match')).toHaveLength(0);
  });

  it('finds a field by name, at most 20 drawn, and pins it as the ticket\'s pin does', async () => {
    render(TrackerFieldsSettings);
    await screen.findAllByTestId('tracker-fields-find');
    const find = within(list('fields')).getByTestId('tracker-fields-find');
    await fireEvent.input(find, { target: { value: 'field 1' } });
    // Field 1, 10 to 19 and 100 to 199 match: 111, of which 20 draw.
    expect(within(list('fields')).getAllByTestId('tracker-fields-match')).toHaveLength(20);
    expect(within(list('fields')).getByTestId('tracker-fields-more').textContent).toBe('91 more match; type more of the name.');
    await fireEvent.input(find, { target: { value: 'field 12' } });
    const matches = names(list('fields'), 'tracker-fields-match');
    expect(matches.slice(0, 3)).toEqual(['Field 12', 'Field 120', 'Field 121']);
    const before = stored.setup;
    await fireEvent.click(within(list('fields')).getAllByTestId('tracker-fields-match')[0]);
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0]).toEqual(withTicketPin(before, 'jira', SITE, 'fields', 'customfield_10012', true, true));
    await waitFor(() => expect(names(list('fields'), 'tracker-fields-pinned')).toEqual(['Field 7', 'Field 3', 'Field 12']));
    expect(reads, 'a pin reads no catalog').toEqual([`jira ${SITE}`]);
  });

  it('unpins a field, and opens a section by name after the ticket\'s default ones', async () => {
    render(TrackerFieldsSettings);
    await screen.findAllByTestId('tracker-fields-find');
    await fireEvent.click(within(list('fields')).getAllByTestId('tracker-fields-pinned')[0]);
    await waitFor(() => expect(names(list('fields'), 'tracker-fields-pinned')).toEqual(['Field 3']));
    await fireEvent.input(within(list('sections')).getByTestId('tracker-fields-find'), { target: { value: 'plan' } });
    await fireEvent.click(within(list('sections')).getByTestId('tracker-fields-match'));
    await waitFor(() => expect(names(list('sections'), 'tracker-fields-pinned')).toEqual(['Description', 'Linked tickets', 'Attachments', 'Test plan']));
    expect(saves[1].ticket_pins).toEqual([{ tracker: 'jira', site: SITE, fields: ['customfield_10003'], sections: ['description', 'links', 'attachments', 'customfield_20002'] }]);
  });

  it('says why a site\'s fields could not be read, and still unpins its pins by ID', async () => {
    setBindingMock('ProjectTrackerBoards', async () => []);
    const other = 'https://other.atlassian.net';
    stored = { revision: 'r0', setup: { status_groups: [], ticket_pins: [
      { tracker: 'jira', site: SITE, fields: ['customfield_10007', 'customfield_10003'] }, { tracker: 'jira', site: other, fields: ['customfield_10001'] },
    ], start_work: [] } } as SetupView;
    catalogs = { [SITE]: new Error('no tracker connection of this computer reaches it'), [other]: catalog(3) };
    render(TrackerFieldsSettings);
    const failed = await screen.findByTestId('tracker-fields-site-error');
    expect(failed.textContent).toBe('Its fields could not be read: No tracker connection of this computer reaches it.');
    const site = screen.getAllByTestId('tracker-fields-site')[0];
    const fields = within(site).getAllByTestId('tracker-fields-list').find((el) => el.dataset.kind === 'fields')!;
    expect(names(fields, 'tracker-fields-pinned')).toEqual(['customfield_10007', 'customfield_10003']);
    expect(within(site).queryAllByTestId('tracker-fields-find'), 'nothing to find without the catalog').toHaveLength(0);
    expect(within(screen.getAllByTestId('tracker-fields-site')[1]).getAllByTestId('tracker-fields-find'), 'the other site').toHaveLength(2);
    await fireEvent.click(within(fields).getAllByTestId('tracker-fields-pinned')[0]);
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0].ticket_pins).toEqual([{ tracker: 'jira', site: other, fields: ['customfield_10001'] }, { tracker: 'jira', site: SITE, fields: ['customfield_10003'] }]);
  });

  it('finds the field named exactly as typed among more than 20 that hold the name', async () => {
    catalogs = { [SITE]: [...Array.from({ length: 25 }, (_, i) => ({ id: `customfield_${30000 + i}`, name: `Area owner ${i}`, kind: 'text' })), { id: 'customfield_39999', name: 'Owner', kind: 'text' }] };
    render(TrackerFieldsSettings);
    await screen.findAllByTestId('tracker-fields-find');
    await fireEvent.input(within(list('fields')).getByTestId('tracker-fields-find'), { target: { value: 'owner' } });
    expect(names(list('fields'), 'tracker-fields-match')[0]).toBe('Owner');
    expect(within(list('fields')).getByTestId('tracker-fields-more').textContent).toBe('6 more match; type more of the name.');
  });

  it('tells two fields of one name apart by their IDs', async () => {
    catalogs = { [SITE]: [{ id: 'customfield_1', name: 'Team', kind: 'option' }, { id: 'customfield_2', name: 'Team', kind: 'text' }, { id: 'customfield_3', name: 'Teams', kind: 'text' }] };
    stored = { revision: 'r0', setup: { status_groups: [], ticket_pins: [{ tracker: 'jira', site: SITE, fields: ['customfield_1'] }], start_work: [] } } as SetupView;
    render(TrackerFieldsSettings);
    await screen.findAllByTestId('tracker-fields-find');
    expect(names(list('fields'), 'tracker-fields-pinned')).toEqual(['Team(customfield_1)']);
    await fireEvent.input(within(list('fields')).getByTestId('tracker-fields-find'), { target: { value: 'team' } });
    expect(names(list('fields'), 'tracker-fields-match')).toEqual(['Team(customfield_2)', 'Teams']);
  });

  it('ranks every field named exactly as typed first, two of one name included', async () => {
    // Jira sorts its catalog by name, case first: TEAM 0 to TEAM 19 before Team.
    catalogs = { [SITE]: [
      ...Array.from({ length: 20 }, (_, i) => ({ id: `customfield_${30000 + i}`, name: `TEAM ${i}`, kind: 'text' })),
      { id: 'customfield_1', name: 'Team', kind: 'option' }, { id: 'customfield_2', name: 'Team', kind: 'text' },
    ] };
    stored = { revision: 'r0', setup: { status_groups: [], ticket_pins: [], start_work: [] } } as SetupView;
    render(TrackerFieldsSettings);
    await screen.findAllByTestId('tracker-fields-find');
    await fireEvent.input(within(list('fields')).getByTestId('tracker-fields-find'), { target: { value: 'team' } });
    expect(names(list('fields'), 'tracker-fields-match').slice(0, 2)).toEqual(['Team(customfield_1)', 'Team(customfield_2)']);
  });

  it('tries a site whose fields could not be read again on the page, and lists them once they are', async () => {
    catalogs = { [SITE]: new Error('Jira could not be reached; try again') };
    render(TrackerFieldsSettings);
    await screen.findByTestId('tracker-fields-site-error');
    catalogs = { [SITE]: catalog(3) };
    await fireEvent.click(screen.getByTestId('tracker-fields-site-retry'));
    await waitFor(() => expect(screen.queryByTestId('tracker-fields-site-error')).toBeNull());
    expect(within(list('fields')).getAllByTestId('tracker-fields-find')).toHaveLength(1);
  });

  it('lists a site whose first pin another window saved while the page is open', async () => {
    setBindingMock('ProjectTrackerBoards', async () => []);
    stored = { revision: 'r0', setup: { status_groups: [], ticket_pins: [], start_work: [] } } as SetupView;
    catalogs = { [SITE]: catalog(3) };
    render(TrackerFieldsSettings);
    await screen.findByTestId('settings-tracker-fields-none');
    stored = { revision: 'r1', setup: { status_groups: [], ticket_pins: [{ tracker: 'jira', site: SITE, fields: ['customfield_10003'] }], start_work: [] } } as SetupView;
    trackerSetupUpdated(HOME_BACKEND);
    await waitFor(() => expect(names(list('fields'), 'tracker-fields-pinned')).toEqual(['Field 3']));
    // Its last pin taken off elsewhere: the site goes.
    stored = { revision: 'r2', setup: { status_groups: [], ticket_pins: [], start_work: [] } } as SetupView;
    trackerSetupUpdated(HOME_BACKEND);
    await screen.findByTestId('settings-tracker-fields-none');
  });

  it('scans no field of the catalog for a pin made with nothing typed', async () => {
    let touched = 0;
    class Counted implements PinField {
      constructor(private readonly i: number) {}
      get id(): string { touched++; return `customfield_${10000 + this.i}`; }
      get name(): string { touched++; return `Field ${this.i}`; }
      get kind(): string { return 'text'; }
    }
    catalogs = { [SITE]: Array.from({ length: 400 }, (_, i) => new Counted(i + 1)) };
    render(TrackerFieldsSettings);
    await screen.findAllByTestId('tracker-fields-find');
    touched = 0;
    await fireEvent.click(within(list('fields')).getAllByTestId('tracker-fields-pinned')[0]);
    await waitFor(() => expect(names(list('fields'), 'tracker-fields-pinned')).toEqual(['Field 3']));
    expect(touched).toBe(0);
  });

  it('reads a site again when the connections that reach it change, and only then', async () => {
    render(TrackerFieldsSettings);
    await screen.findAllByTestId('tracker-fields-find');
    expect(reads).toEqual([`jira ${SITE}`]);
    trackerSetupUpdated(HOME_BACKEND);
    await screen.findAllByTestId('tracker-fields-find');
    catalogs = { [SITE]: new Error('this tracker connection is turned off; turn it on in the Issues settings, then try again') };
    setBindingMock('TrackerConnections', async () => [trackerConnection({ enabled: false })]);
    await refreshTrackerConnections(HOME_BACKEND);
    await waitFor(() => expect(screen.getByTestId('tracker-fields-site-error').textContent).toContain('This tracker connection is turned off'));
    expect(reads).toEqual([`jira ${SITE}`, `jira ${SITE}`]);
  });

  it('says a Linear-only project has no fields to pin, and a project with no boards that it has none', async () => {
    stored = { revision: 'r0', setup: { status_groups: [], ticket_pins: [], start_work: [] } } as SetupView;
    setBindingMock('TrackerConnections', async () => [trackerConnection({ id: 'lin-1', kind: 'linear', site: 'example' })]);
    setBindingMock('ProjectTrackerBoards', async () => [trackerBoardChoice({ connection: 'lin-1', connection_kind: 'linear', tracker_project: 'APP' })]);
    const { unmount } = render(TrackerFieldsSettings);
    expect((await screen.findByTestId('settings-tracker-fields-none')).textContent).toContain("A Linear ticket's fields are fixed");
    unmount();
    setBindingMock('ProjectTrackerBoards', async () => []);
    render(TrackerFieldsSettings);
    expect((await screen.findByTestId('settings-tracker-fields-none')).textContent).toContain('This project lists no boards');
  });
});
