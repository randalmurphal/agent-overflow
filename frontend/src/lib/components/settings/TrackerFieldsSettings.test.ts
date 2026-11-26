// Settings > Issues > Fields: the pinned fields and sections of the
// project's Jira site at once, in pin order; a field or a section not
// pinned found by name, the exact name first, at most 20 drawn, two of one
// name told apart by ID; a pin or an unpin saved as the ticket's pin
// saves it (withTicketPin); the sections a ticket opens while none are
// pinned shown as pinned; a site whose fields could not be read said,
// with its pins by ID to take off; and a Linear-only project, or one with
// no boards, said.

import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import type { PinField, Setup, SetupView, SiteFields } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import TrackerFieldsSettings from './TrackerFieldsSettings.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { readSidebarCatalogs } from '../../../test/helpers/sidebarCatalogs';
import { trackerBoardChoice, trackerConnection } from '../../../test/helpers/trackerConnections';
import { noteProject } from '../../transport/entityIndex';
import { addProjectLocal } from '../../stores/projects.svelte';
import { withTicketPin } from '../../stores/trackerTicketModel';

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
let sites: SiteFields[];
let saves: Setup[];

beforeEach(async () => {
  setBindingMock('ListProjects', async () => []);
  await readSidebarCatalogs();
  noteProject('p1', '');
  addProjectLocal({ id: 'p1', name: 'web', path: '/repos/web', createdAt: 1, updatedAt: 1, sortPosition: 0, archived: false });
  setBindingMock('TrackerConnections', async () => [trackerConnection()]);
  setBindingMock('ProjectTrackerBoards', async () => [trackerBoardChoice()]);
  sites = [{ tracker: 'jira', site: SITE, fields: catalog(400) }];
  setBindingMock('ProjectTrackerFields', async () => sites);
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
    await screen.findAllByTestId('tracker-fields-pinned');
    expect(names(list('fields'), 'tracker-fields-pinned')).toEqual(['Field 7', 'Field 3']);
    // No section is pinned: the ticket's default ones show, as the ticket opens them.
    expect(names(list('sections'), 'tracker-fields-pinned')).toEqual(['Description', 'Linked tickets', 'Attachments']);
    expect(screen.queryAllByTestId('tracker-fields-match')).toHaveLength(0);
  });

  it('finds a field by name, at most 20 drawn, and pins it as the ticket\'s pin does', async () => {
    render(TrackerFieldsSettings);
    await screen.findAllByTestId('tracker-fields-pinned');
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
  });

  it('unpins a field, and opens a section by name after the ticket\'s default ones', async () => {
    render(TrackerFieldsSettings);
    await screen.findAllByTestId('tracker-fields-pinned');
    await fireEvent.click(within(list('fields')).getAllByTestId('tracker-fields-pinned')[0]);
    await waitFor(() => expect(names(list('fields'), 'tracker-fields-pinned')).toEqual(['Field 3']));
    await fireEvent.input(within(list('sections')).getByTestId('tracker-fields-find'), { target: { value: 'plan' } });
    await fireEvent.click(within(list('sections')).getByTestId('tracker-fields-match'));
    await waitFor(() => expect(names(list('sections'), 'tracker-fields-pinned')).toEqual(['Description', 'Linked tickets', 'Attachments', 'Test plan']));
    expect(saves[1].ticket_pins).toEqual([{ tracker: 'jira', site: SITE, fields: ['customfield_10003'], sections: ['description', 'links', 'attachments', 'customfield_20002'] }]);
  });

  it('says why a site\'s fields could not be read, and still unpins its pins by ID', async () => {
    setBindingMock('ProjectTrackerBoards', async () => []);
    sites = [
      { tracker: 'jira', site: SITE, fields: [], error: 'no tracker connection of this computer reaches it' },
      { tracker: 'jira', site: 'https://other.atlassian.net', fields: catalog(3) },
    ];
    render(TrackerFieldsSettings);
    const failed = await screen.findByTestId('tracker-fields-site-error');
    expect(failed.textContent).toBe('Its fields could not be read: no tracker connection of this computer reaches it');
    const site = screen.getAllByTestId('tracker-fields-site')[0];
    const fields = within(site).getAllByTestId('tracker-fields-list').find((el) => el.dataset.kind === 'fields')!;
    expect(names(fields, 'tracker-fields-pinned')).toEqual(['customfield_10007', 'customfield_10003']);
    expect(within(site).queryAllByTestId('tracker-fields-find'), 'nothing to find without the catalog').toHaveLength(0);
    expect(within(screen.getAllByTestId('tracker-fields-site')[1]).getAllByTestId('tracker-fields-find'), 'the other site').toHaveLength(2);
    await fireEvent.click(within(fields).getAllByTestId('tracker-fields-pinned')[0]);
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0].ticket_pins).toEqual([{ tracker: 'jira', site: SITE, fields: ['customfield_10003'] }]);
  });

  it('finds the field named exactly as typed among more than 20 that hold the name', async () => {
    sites = [{ tracker: 'jira', site: SITE, fields: [...Array.from({ length: 25 }, (_, i) => ({ id: `customfield_${30000 + i}`, name: `Area owner ${i}`, kind: 'text' })), { id: 'customfield_39999', name: 'Owner', kind: 'text' }] }];
    render(TrackerFieldsSettings);
    await screen.findAllByTestId('tracker-fields-find');
    await fireEvent.input(within(list('fields')).getByTestId('tracker-fields-find'), { target: { value: 'owner' } });
    expect(names(list('fields'), 'tracker-fields-match')[0]).toBe('Owner');
    expect(within(list('fields')).getByTestId('tracker-fields-more').textContent).toBe('6 more match; type more of the name.');
  });

  it('tells two fields of one name apart by their IDs', async () => {
    sites = [{ tracker: 'jira', site: SITE, fields: [{ id: 'customfield_1', name: 'Team', kind: 'option' }, { id: 'customfield_2', name: 'Team', kind: 'text' }, { id: 'customfield_3', name: 'Teams', kind: 'text' }] }];
    stored = { revision: 'r0', setup: { status_groups: [], ticket_pins: [{ tracker: 'jira', site: SITE, fields: ['customfield_1'] }], start_work: [] } } as SetupView;
    render(TrackerFieldsSettings);
    await screen.findAllByTestId('tracker-fields-pinned');
    expect(names(list('fields'), 'tracker-fields-pinned')).toEqual(['Team (customfield_1)']);
    await fireEvent.input(within(list('fields')).getByTestId('tracker-fields-find'), { target: { value: 'team' } });
    expect(names(list('fields'), 'tracker-fields-match')).toEqual(['Team (customfield_2)', 'Teams']);
  });

  it('says a Linear-only project has no fields to pin, and a project with no boards that it has none', async () => {
    sites = [];
    setBindingMock('ProjectTrackerBoards', async () => [trackerBoardChoice({ connection_kind: 'linear', tracker_project: 'APP' })]);
    const { unmount } = render(TrackerFieldsSettings);
    expect((await screen.findByTestId('settings-tracker-fields-none')).textContent).toContain("A Linear ticket's fields are fixed");
    unmount();
    setBindingMock('ProjectTrackerBoards', async () => []);
    render(TrackerFieldsSettings);
    expect((await screen.findByTestId('settings-tracker-fields-none')).textContent).toContain('This project lists no boards');
  });
});
