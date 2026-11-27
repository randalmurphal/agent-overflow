// Settings > Issues > Points and credit: What a ticket is worth picks the
// boards' estimates or counting tickets and saves at once; Usual pace says
// each sprint-planning Jira board's pace in the unit the project counts
// it in, read again when the setup changes; Planning poker turns off and
// on; a Linear-only project, and one with no boards, say they have none.

import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import type { BoardPace, Setup, SetupView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import TrackerPointsSettings from './TrackerPointsSettings.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { readSidebarCatalogs } from '../../../test/helpers/sidebarCatalogs';
import { trackerBoardChoice, trackerConnection } from '../../../test/helpers/trackerConnections';
import { noteProject } from '../../transport/entityIndex';
import { addProjectLocal } from '../../stores/projects.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';
import { trackerSetupUpdated } from '../../stores/trackerBoardEvents';

let stored: SetupView;
let saves: Setup[];
let paceReads: number;

function setupWith(estimates: Setup['estimates']): SetupView {
  return { revision: 'r0', setup: { status_groups: [], ticket_pins: [], start_work: [], estimates } } as SetupView;
}

/** The fixture's board as the computer reads its pace: 44 points and 9
 * tickets a sprint over 5 sprints, in the unit the stored setup counts. */
function paceOf(extra: Partial<BoardPace> = {}): BoardPace {
  const tickets = stored.setup.estimates?.count === 'tickets';
  return {
    connection: 'jira-1', board: '1', name: 'WEB board', read: true, estimate: 'points', counts: tickets ? 'none' : 'points', plans_sprints: true,
    pace: { sprints: 5, rows: 9, estimate: 44 }, ...extra,
  };
}

let paces: () => BoardPace[];

beforeEach(async () => {
  setBindingMock('ListProjects', async () => []);
  await readSidebarCatalogs();
  noteProject('p1', '');
  addProjectLocal({ id: 'p1', name: 'web', path: '/repos/web', createdAt: 1, updatedAt: 1, sortPosition: 0, archived: false });
  setBindingMock('TrackerConnections', async () => [trackerConnection()]);
  setBindingMock('ProjectTrackerBoards', async () => [trackerBoardChoice()]);
  stored = setupWith(undefined);
  saves = [];
  paceReads = 0;
  paces = () => [paceOf()];
  setBindingMock('ProjectTrackerSetup', async () => stored);
  setBindingMock('SetProjectTrackerSetup', async (_project: string, _revision: string, setup: Setup) => {
    saves.push(setup);
    // The computer stores the built-in choices as none.
    const estimates = { ...(setup.estimates?.count === 'tickets' ? { count: 'tickets' } : {}), ...(setup.estimates?.poker === false ? { poker: false } : {}) };
    stored = { revision: `r${saves.length}`, setup: { ...setup, estimates: Object.keys(estimates).length > 0 ? estimates : undefined } } as SetupView;
    return { saved: true, stored };
  });
  setBindingMock('ProjectTrackerPace', async () => {
    paceReads++;
    return paces();
  });
});

const count = () => screen.getByTestId('tracker-points-count') as HTMLSelectElement;
const paceLines = () => screen.getAllByTestId('tracker-points-pace-line').map((el) => el.textContent?.trim());
const poker = () => screen.getByRole('switch', { name: 'Planning poker' });

describe('<TrackerPointsSettings>', () => {
  it('counts tickets in place of story points and back, the pace following the pick once the setup changes', async () => {
    render(TrackerPointsSettings);
    await screen.findByTestId('tracker-points-count');
    expect(count().value).toBe('estimates');
    expect([...count().options].map((o) => o.text)).toEqual(['Story points', 'Count tickets']);
    expect(paceLines()).toEqual(['44 points a sprint, from the last 5 sprints.']);

    count().value = 'tickets';
    await fireEvent.change(count());
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0].estimates).toEqual({ count: 'tickets' });
    trackerSetupUpdated(HOME_BACKEND);
    await waitFor(() => expect(paceLines()).toEqual(['9 tickets a sprint, from the last 5 sprints.']));
    expect(count().value).toBe('tickets');

    count().value = 'estimates';
    await fireEvent.change(count());
    await waitFor(() => expect(saves).toHaveLength(2));
    expect(saves[1].estimates).toEqual({ count: 'estimates' });
    trackerSetupUpdated(HOME_BACKEND);
    await waitFor(() => expect(paceLines()).toEqual(['44 points a sprint, from the last 5 sprints.']));
  });

  it('turns planning poker off and back on at once', async () => {
    render(TrackerPointsSettings);
    await screen.findByTestId('tracker-points-poker');
    expect(poker().getAttribute('aria-checked')).toBe('true');
    await fireEvent.click(poker());
    await waitFor(() => expect(poker().getAttribute('aria-checked')).toBe('false'));
    expect(saves[0].estimates).toEqual({ poker: false });
    await fireEvent.click(poker());
    await waitFor(() => expect(poker().getAttribute('aria-checked')).toBe('true'));
    expect(saves[1].estimates).toEqual({ poker: true });
  });

  it('names each board when several plan sprints, says a board not read and one that finished no sprint, and skips one that plans none', async () => {
    paces = () => [
      paceOf(),
      paceOf({ board: '2', name: 'OPS board', read: false, estimate: '', counts: '', plans_sprints: false, pace: undefined }),
      paceOf({ board: '3', name: 'New board', pace: undefined }),
      paceOf({ board: '4', name: 'Kanban', plans_sprints: false, pace: undefined }),
    ];
    render(TrackerPointsSettings);
    await screen.findByTestId('tracker-points-count');
    expect(paceLines()).toEqual([
      'WEB board: 44 points a sprint, from the last 5 sprints.',
      'Read when OPS board is opened in Issues.',
      'No sprint of New board has finished yet.',
    ]);
  });

  it('names the pick the board\'s estimates when a board estimates in time', async () => {
    paces = () => [paceOf({ estimate: 'time', counts: 'time', pace: { sprints: 1, rows: 3, estimate: 36_000 } })];
    render(TrackerPointsSettings);
    await screen.findByTestId('tracker-points-count');
    expect([...count().options].map((o) => o.text)).toEqual(["The board's estimates", 'Count tickets']);
    expect(paceLines()).toEqual(['10h a sprint, from the last sprint.']);
  });

  it('says a Linear-only project has no points settings, reading no pace', async () => {
    setBindingMock('TrackerConnections', async () => [trackerConnection({ id: 'lin-1', kind: 'linear', site: 'acme' })]);
    setBindingMock('ProjectTrackerBoards', async () => [trackerBoardChoice({ connection: 'lin-1', connection_kind: 'linear' })]);
    paces = () => [];
    render(TrackerPointsSettings);
    expect((await screen.findByTestId('settings-tracker-points-none')).textContent?.trim()).toBe('A Linear project has no points settings: they are for Jira boards.');
    expect(screen.queryByTestId('tracker-points-count')).toBeNull();
  });

  it('shows no control for an estimates section a newer version saved', async () => {
    stored = { ...setupWith(undefined), newer: ['estimates'] } as SetupView;
    render(TrackerPointsSettings);
    expect((await screen.findByTestId('settings-tracker-points-newer')).textContent).toContain("saved this project's points and credit settings");
    expect(screen.queryByTestId('tracker-points-count')).toBeNull();
    expect(paceReads).toBe(1);
  });
});
