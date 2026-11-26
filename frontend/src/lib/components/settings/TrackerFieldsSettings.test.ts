// Settings > Issues > Fields: the pinned fields and sections of the
// project's Jira site at once, in pin order; a field or a section not
// pinned found by name, at most 20 drawn; a pin or an unpin saved as the
// ticket's pin saves it (withTicketPin); the sections a ticket opens
// while none are pinned shown as pinned; and a Linear-only project, or one
// with no boards, said.

import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import type { FieldView, Setup, SetupView, SiteFields } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
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
function catalog(n: number): FieldView[] {
  const out: FieldView[] = [];
  for (let i = 1; i <= n; i++) out.push({ id: `customfield_${10000 + i}`, name: `Field ${i}`, kind: 'text', value: {} });
  out.push({ id: 'customfield_20001', name: 'Acceptance criteria', kind: 'rich_text', value: {} });
  out.push({ id: 'customfield_20002', name: 'Test plan', kind: 'rich_text', value: {} });
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
