// Settings > Issues > Time: the meetings and time-off tickets found by
// key or title on the project's Jira board connections and saved at once
// by site and key, cleared; each ticket set checked through one
// connection of its site, every ticket of a connection in one lookup,
// which names its title or says it is gone or could not be checked;
// Never log to an epic on until turned off; and a Linear-only project,
// one with no boards, and a site no connection reaches, said.

import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import type { CardView, LookupView, Setup, SetupView, TimeSettings } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import TrackerTimeSettings from './TrackerTimeSettings.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { readSidebarCatalogs } from '../../../test/helpers/sidebarCatalogs';
import { trackerBoardChoice, trackerConnection } from '../../../test/helpers/trackerConnections';
import { noteProject } from '../../transport/entityIndex';
import { getPinnedBackend } from '../../transport/backends';
import { addProjectLocal } from '../../stores/projects.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';

const SITE = 'https://example.atlassian.net';

/** The tickets each site's tracker has, by key. */
const TICKETS: Record<string, string> = { 'OPS-1': 'Meetings', 'OPS-2': 'Time off' };

let stored: SetupView;
let saves: Setup[];
/** The checks the page made: the lookups it asked with no finder open. */
let lookups: { connection: string; keys: string[]; pinned: string | null }[];
let lookupFails: Error | null;

function setupWith(time: TimeSettings | undefined): SetupView {
  return { revision: 'r0', setup: { status_groups: [], ticket_pins: [], start_work: [], time } } as SetupView;
}

beforeEach(async () => {
  setBindingMock('ListProjects', async () => []);
  await readSidebarCatalogs();
  noteProject('p1', '');
  addProjectLocal({ id: 'p1', name: 'web', path: '/repos/web', createdAt: 1, updatedAt: 1, sortPosition: 0, archived: false });
  setBindingMock('TrackerConnections', async () => [trackerConnection()]);
  setBindingMock('ProjectTrackerBoards', async () => [trackerBoardChoice()]);
  stored = setupWith(undefined);
  saves = [];
  lookups = [];
  lookupFails = null;
  setBindingMock('ProjectTrackerSetup', async () => stored);
  setBindingMock('SetProjectTrackerSetup', async (_project: string, _revision: string, setup: Setup) => {
    saves.push(setup);
    stored = { revision: `r${saves.length}`, setup } as SetupView;
    return { saved: true, stored };
  });
  setBindingMock('TrackerLookup', async (connection: string, keys: string[]): Promise<LookupView> => {
    if (!document.querySelector('[data-testid="ticket-picker-find"]')) lookups.push({ connection, keys, pinned: getPinnedBackend() });
    if (lookupFails) throw lookupFails;
    const found = keys.filter((k) => TICKETS[k]).map((k) => ({ key: k, card: { id: `id-${k}`, key: k, title: TICKETS[k] } as CardView }));
    return { found, unmatched: keys.filter((k) => !TICKETS[k]) } as LookupView;
  });
  setBindingMock('TrackerSearch', async (_connection: string, req: { text: string }) => ({
    cards: Object.entries(TICKETS).filter(([, title]) => title.toLowerCase().includes(req.text.toLowerCase())).map(([key, title]) => ({ id: `id-${key}`, key, title })),
  }));
});

function row(role: 'meetings' | 'time_off'): HTMLElement {
  return screen.getAllByTestId('tracker-time-ticket').find((el) => el.dataset.role === role)!;
}

const text = (el: Element | null) => el?.textContent?.replace(/\s+/g, ' ').trim();

/** Opens the row's finder, types `query` and picks the one ticket listed. */
async function choose(role: 'meetings' | 'time_off', query: string): Promise<void> {
  await fireEvent.click(within(row(role)).getByTestId('tracker-time-ticket-find'));
  await fireEvent.input(await screen.findByTestId('ticket-picker-find'), { target: { value: query } });
  const item = await screen.findByTestId('ticket-picker-item');
  await fireEvent.click(item);
  await waitFor(() => expect(screen.queryByTestId('ticket-picker-find')).toBeNull());
}

const epics = () => screen.getByRole('switch', { name: 'Never log to an epic' });

describe('<TrackerTimeSettings>', () => {
  it('saves a ticket found by key and one found by title, and checks both in one lookup', async () => {
    render(TrackerTimeSettings);
    await screen.findAllByTestId('tracker-time-ticket');
    expect(text(within(row('meetings')).getByTestId('tracker-time-ticket-find'))).toBe('Choose a ticket');
    expect(epics().getAttribute('aria-checked')).toBe('true');
    expect(lookups, 'nothing is set, so nothing is checked').toEqual([]);

    await choose('meetings', 'ops-1');
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0].time).toEqual({ meetings: { tracker: 'jira', site: SITE, key: 'OPS-1' } });
    await waitFor(() => expect(text(within(row('meetings')).getByTestId('tracker-time-ticket-value'))).toBe('OPS-1 Meetings'));

    await choose('time_off', 'time off');
    await waitFor(() => expect(saves).toHaveLength(2));
    expect(saves[1].time).toEqual({ meetings: { tracker: 'jira', site: SITE, key: 'OPS-1' }, time_off: { tracker: 'jira', site: SITE, key: 'OPS-2' } });
    await waitFor(() => expect(text(within(row('time_off')).getByTestId('tracker-time-ticket-value'))).toBe('OPS-2 Time off'));
    expect(lookups).toEqual([
      { connection: 'jira-1', keys: ['OPS-1'], pinned: HOME_BACKEND },
      { connection: 'jira-1', keys: ['OPS-1', 'OPS-2'], pinned: HOME_BACKEND },
    ]);
  });

  it('clears a ticket, and asks nothing more for the one left', async () => {
    stored = setupWith({ meetings: { tracker: 'jira', site: SITE, key: 'OPS-1' }, time_off: { tracker: 'jira', site: SITE, key: 'OPS-2' } });
    render(TrackerTimeSettings);
    await waitFor(() => expect(text(within(row('time_off')).getByTestId('tracker-time-ticket-value'))).toBe('OPS-2 Time off'));
    await fireEvent.click(within(row('meetings')).getByTestId('tracker-time-ticket-clear'));
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0].time).toEqual({ meetings: null, time_off: { tracker: 'jira', site: SITE, key: 'OPS-2' } });
    await waitFor(() => expect(within(row('meetings')).queryByTestId('tracker-time-ticket-value')).toBeNull());
    expect(within(row('meetings')).queryByTestId('tracker-time-ticket-clear')).toBeNull();
    await waitFor(() => expect(lookups.map((l) => l.keys)).toEqual([['OPS-1', 'OPS-2'], ['OPS-2']]));
  });

  it('says a ticket that is gone or that the connection cannot see', async () => {
    stored = setupWith({ meetings: { tracker: 'jira', site: SITE, key: 'OPS-404' } });
    render(TrackerTimeSettings);
    const note = await screen.findByTestId('tracker-time-ticket-note');
    expect(note.textContent).toBe('OPS-404 cannot be found on example.atlassian.net, or this connection cannot see it. Choose another ticket.');
    expect(text(within(row('meetings')).getByTestId('tracker-time-ticket-value'))).toBe('OPS-404');
  });

  it('says a check that failed, and checks again on Try again', async () => {
    stored = setupWith({ meetings: { tracker: 'jira', site: SITE, key: 'OPS-1' } });
    lookupFails = new Error('Jira did not answer.');
    render(TrackerTimeSettings);
    const note = await screen.findByTestId('tracker-time-ticket-note');
    expect(note.textContent).toContain('OPS-1 could not be checked: Jira did not answer.');
    lookupFails = null;
    await fireEvent.click(screen.getByTestId('tracker-time-ticket-retry'));
    await waitFor(() => expect(text(within(row('meetings')).getByTestId('tracker-time-ticket-value'))).toBe('OPS-1 Meetings'));
    expect(screen.queryByTestId('tracker-time-ticket-note')).toBeNull();
    expect(lookups).toHaveLength(2);
  });

  it('says a ticket whose site no connection turned on reaches, asking nothing', async () => {
    setBindingMock('TrackerConnections', async () => [trackerConnection({ enabled: false })]);
    stored = setupWith({ meetings: { tracker: 'jira', site: SITE, key: 'OPS-1' } });
    render(TrackerTimeSettings);
    const note = await screen.findByTestId('tracker-time-ticket-note');
    expect(note.textContent).toBe('No Jira connection turned on reaches example.atlassian.net. Turn one on in Connections, or choose another ticket.');
    expect(within(row('time_off')).getByTestId('tracker-time-ticket-find')).toBeDisabled();
    expect(lookups).toEqual([]);
  });

  it('turns Never log to an epic off and back on at once', async () => {
    render(TrackerTimeSettings);
    await screen.findAllByTestId('tracker-time-ticket');
    await fireEvent.click(epics());
    await waitFor(() => expect(epics().getAttribute('aria-checked')).toBe('false'));
    expect(saves[0].time).toEqual({ never_epics: false });
    await fireEvent.click(epics());
    await waitFor(() => expect(epics().getAttribute('aria-checked')).toBe('true'));
    expect(saves[1].time).toEqual({ never_epics: true });
  });

  it('says a Linear-only project has no time settings, and a project with no boards that it has none', async () => {
    setBindingMock('TrackerConnections', async () => [trackerConnection({ id: 'lin-1', kind: 'linear', site: 'acme' })]);
    setBindingMock('ProjectTrackerBoards', async () => [trackerBoardChoice({ connection: 'lin-1', connection_kind: 'linear' })]);
    const view = render(TrackerTimeSettings);
    expect((await screen.findByTestId('settings-tracker-time-none')).textContent?.trim()).toBe('A Linear project has no time settings: time is logged on Jira.');
    expect(screen.queryByRole('switch', { name: 'Never log to an epic' })).toBeNull();
    view.unmount();
    setBindingMock('ProjectTrackerBoards', async () => []);
    render(TrackerTimeSettings);
    expect((await screen.findByTestId('settings-tracker-time-none')).textContent?.trim()).toBe('This project lists no boards. Add a Jira board in Boards to choose where its time goes.');
  });

  it('shows no control for a time section a newer version saved', async () => {
    stored = { ...setupWith(undefined), newer: ['time'] } as SetupView;
    render(TrackerTimeSettings);
    expect((await screen.findByTestId('settings-tracker-time-newer')).textContent).toContain("saved this project's time settings");
    expect(screen.queryByTestId('tracker-time-ticket')).toBeNull();
  });
});
