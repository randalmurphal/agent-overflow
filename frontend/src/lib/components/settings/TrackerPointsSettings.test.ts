// Settings > Issues > Points and credit: What a ticket is worth picks the
// boards' estimates or counting tickets and saves at once; Usual pace says
// each sprint-planning Jira board's pace in the unit the project counts
// it in, read again when the setup changes; Planning poker turns off and
// on; a Linear-only project, and one with no boards, say they have none.
// One read (ProjectTrackerPace) lists the boards with their pace. The
// Credit group turns credit on and off, keeping its rules; changes a rule
// from this sprint or for every sprint; words its rules in the unit the
// project counts; picks which tickets are support; and says a credit
// section a newer version saved.

import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import type { BoardPace, Credit, CreditRules, Setup, SetupView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import TrackerPointsSettings from './TrackerPointsSettings.svelte';
import { formatPace } from '../issues/boardFormat';
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
let boardReads: number;

const ALL_ACTIONS = ['commented', 'approved', 'logged_time'];

function rules(extra: Partial<CreditRules> = {}): CreditRules {
  return { from: 0, review_share: 10, review_need: 2, review_actions: ALL_ACTIONS, hours_per_point: 4, ...extra };
}

/** The computer's credit with no section: off, with the default rules. */
const NO_CREDIT: Credit = { on: false, rules: [rules()], support: [] };

function setupWith(estimates: Setup['estimates'], credit: Credit = NO_CREDIT): SetupView {
  return { revision: 'r0', setup: { status_groups: [], ticket_pins: [], start_work: [], estimates, credit } } as SetupView;
}

/** When the computer stamps a save's newest snapshot, Unix ms. */
const SAVED_AT = 1_900_000_000_000;

/** The credit a save stores, as the computer stamps it: a newest snapshot
 * with no time takes effect from the save (the first from the start), and
 * one with the values of the snapshot before it is dropped. */
function stamped(credit: Credit | null | undefined): Credit | null | undefined {
  if (!credit) return credit;
  const out = credit.rules.slice();
  const newest = out[out.length - 1];
  if (newest.from == null) {
    const before = out[out.length - 2];
    const same = before && before.review_share === newest.review_share && before.review_need === newest.review_need &&
      before.hours_per_point === newest.hours_per_point && [...before.review_actions].sort().join() === [...newest.review_actions].sort().join();
    if (same) out.pop();
    else out[out.length - 1] = { ...newest, from: out.length === 1 ? 0 : SAVED_AT };
  }
  return { ...credit, rules: out };
}

/** The fixture's board as the computer reads its pace: 44 points and 9
 * tickets a sprint over 5 sprints, in the unit the stored setup counts. */
function paceOf(extra: Partial<BoardPace> = {}): BoardPace {
  const tickets = stored.setup.estimates?.count === 'tickets';
  return {
    connection: 'jira-1', connection_kind: 'jira', site: 'https://example.atlassian.net', board: '1', name: 'WEB board', read: true, estimate: 'points', counts: tickets ? 'none' : 'points', plans_sprints: true,
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
  boardReads = 0;
  setBindingMock('ProjectTrackerBoards', async () => {
    boardReads++;
    return [trackerBoardChoice()];
  });
  stored = setupWith(undefined);
  saves = [];
  paceReads = 0;
  paces = () => [paceOf()];
  setBindingMock('ProjectTrackerSetup', async () => stored);
  setBindingMock('SetProjectTrackerSetup', async (_project: string, _revision: string, setup: Setup) => {
    saves.push(setup);
    // The computer stores the built-in choices as none.
    const estimates = { ...(setup.estimates?.count === 'tickets' ? { count: 'tickets' } : {}), ...(setup.estimates?.poker === false ? { poker: false } : {}) };
    stored = { revision: `r${saves.length}`, setup: { ...setup, estimates: Object.keys(estimates).length > 0 ? estimates : undefined, credit: stamped(setup.credit) } } as SetupView;
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
    expect([paceReads, boardReads], 'one read of the boards, the pace\'s').toEqual([1, 0]);

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
      { connection: 'lin-1', connection_kind: 'linear', board: 'team-1', name: 'Linear team', read: false, estimate: '', counts: '', plans_sprints: false },
    ];
    render(TrackerPointsSettings);
    await screen.findByTestId('tracker-points-count');
    expect(paceLines()).toEqual([
      'WEB board: 44 points a sprint, from the last 5 sprints.',
      'Read when OPS board is opened in Issues.',
      'No sprint of New board has finished yet.',
    ]);
  });

  it('says a pace to a tenth as sprint planning does', async () => {
    paces = () => [paceOf({ pace: { sprints: 1, rows: 1, estimate: 1.15 } })];
    render(TrackerPointsSettings);
    await screen.findByTestId('tracker-points-count');
    expect(paceLines()).toEqual([`${formatPace(1.15, 'points')} a sprint, from the last sprint.`]);
    expect(paceLines()).toEqual(['1.2 points a sprint, from the last sprint.']);
  });

  it('names the pick the board\'s estimates when a board estimates in time', async () => {
    paces = () => [paceOf({ estimate: 'time', counts: 'time', pace: { sprints: 1, rows: 3, estimate: 36_000 } })];
    render(TrackerPointsSettings);
    await screen.findByTestId('tracker-points-count');
    expect([...count().options].map((o) => o.text)).toEqual(["The board's estimates", 'Count tickets']);
    expect(paceLines()).toEqual(['10h a sprint, from the last sprint.']);
  });

  it('says a Linear-only project has no points settings, from its one read of the boards', async () => {
    setBindingMock('TrackerConnections', async () => [trackerConnection({ id: 'lin-1', kind: 'linear', site: 'acme' })]);
    paces = () => [{ connection: 'lin-1', connection_kind: 'linear', board: 'team-1', name: 'Linear team', read: false, estimate: '', counts: '', plans_sprints: false }];
    render(TrackerPointsSettings);
    expect((await screen.findByTestId('settings-tracker-points-none')).textContent?.trim()).toBe('A Linear project has no points settings: they are for Jira boards.');
    expect(screen.queryByTestId('tracker-points-count')).toBeNull();
    expect([paceReads, boardReads]).toEqual([1, 0]);
  });

  it('says a project with no boards to add one', async () => {
    paces = () => [];
    render(TrackerPointsSettings);
    expect((await screen.findByTestId('settings-tracker-points-none')).textContent?.trim()).toBe('This project lists no boards. Add a Jira board in Boards to set how its work is counted.');
  });

  it('shows no control for an estimates section a newer version saved', async () => {
    stored = { ...setupWith(undefined), newer: ['estimates'] } as SetupView;
    render(TrackerPointsSettings);
    expect((await screen.findByTestId('settings-tracker-points-newer')).textContent).toContain("saved this project's points and credit settings");
    expect(screen.queryByTestId('tracker-points-count')).toBeNull();
    expect(paceReads).toBe(1);
  });
});

describe('the Credit group', () => {
  const on = () => screen.getByRole('switch', { name: 'Count credit' });
  const line = (id: string) => screen.getByTestId(id).textContent?.replace(/\s+/g, ' ').trim();
  const change = async (testId: string, value: number) => {
    const input = screen.getByTestId(testId) as HTMLInputElement;
    input.value = String(value);
    await fireEvent.change(input);
  };

  function answerTypes(): void {
    setBindingMock('TrackerBoardProjects', async () => [{ id: '10000', key: 'WEB', name: 'Web' }]);
    const types = [{ id: '10001', name: 'Story', level: 0 }, { id: '10004', name: 'Support', level: 0 }];
    setBindingMock('TrackerProjectIssueTypes', async () => types);
    // The types the account may create: the same here, and never what the
    // group lists.
    setBindingMock('TrackerIssueTypes', async () => types);
  }

  it('turns credit on, saying its rules, and off keeping them', async () => {
    answerTypes();
    render(TrackerPointsSettings);
    await screen.findByTestId('tracker-credit');
    expect(on().getAttribute('aria-checked')).toBe('false');
    expect(screen.queryByTestId('tracker-credit-review'), 'off shows only the switch').toBeNull();

    await fireEvent.click(on());
    await waitFor(() => expect(on().getAttribute('aria-checked')).toBe('true'));
    expect(saves[0].credit).toEqual({ ...NO_CREDIT, on: true });
    expect(line('tracker-credit-review-line')).toBe("10% of a finished ticket's points, to each reviewer who did at least 2 of: commented, approved, logged time. On the ticket or its pull request; the assignee never counts.");
    expect(line('tracker-credit-support-line')).toBe('1 point for every 4 hours logged on support tickets, which carry no points.');

    await change('tracker-credit-hours', 6);
    await waitFor(() => expect(saves).toHaveLength(2));
    await fireEvent.click(on());
    await waitFor(() => expect(saves).toHaveLength(3));
    expect(saves[2].credit).toEqual({ on: false, rules: [rules(), rules({ from: SAVED_AT, hours_per_point: 6 })], support: [] });
    await fireEvent.click(on());
    await waitFor(() => expect(line('tracker-credit-support-line')).toBe('1 point for every 6 hours logged on support tickets, which carry no points.'));
  });

  it('changes a rule from this sprint on, or in every sprint', async () => {
    answerTypes();
    stored = setupWith(undefined, { on: true, rules: [rules(), rules({ from: 1_800_000_000_000 })], support: [] });
    render(TrackerPointsSettings);
    await screen.findByTestId('tracker-credit-share');
    await change('tracker-credit-share', 15);
    await waitFor(() => expect(saves).toHaveLength(1));
    // A new newest snapshot with no time, which the computer stamps.
    expect(saves[0].credit!.rules).toEqual([rules(), rules({ from: 1_800_000_000_000 }), rules({ from: undefined, review_share: 15 })]);

    await fireEvent.click(screen.getByRole('radio', { name: 'Every sprint' }));
    await change('tracker-credit-hours', 8);
    await waitFor(() => expect(saves).toHaveLength(2));
    expect(saves[1].credit!.rules).toEqual([rules({ hours_per_point: 8 }), rules({ from: 1_800_000_000_000, hours_per_point: 8 }),
      rules({ from: SAVED_AT, review_share: 15, hours_per_point: 8 })]);
  });

  it('sets an action as chosen in every sprint, whatever each snapshot counted', async () => {
    answerTypes();
    // The older rule counts comments only; the newest all three.
    const older = rules({ review_actions: ['commented'], review_need: 1 });
    stored = setupWith(undefined, { on: true, rules: [older, rules({ from: 1_800_000_000_000 })], support: [] });
    render(TrackerPointsSettings);
    await screen.findByTestId('tracker-credit-review');
    const action = (id: string) => screen.getAllByTestId('tracker-credit-action').find((b) => b.dataset.action === id)!;
    await fireEvent.click(screen.getByRole('radio', { name: 'Every sprint' }));
    await fireEvent.click(action('logged_time'));
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0].credit!.rules.map((r) => r.review_actions)).toEqual([['commented'], ['commented', 'approved']]);
    // Turned on again, it is on in both.
    await waitFor(() => expect(action('logged_time').getAttribute('aria-pressed')).toBe('false'));
    await fireEvent.click(action('logged_time'));
    await waitFor(() => expect(saves).toHaveLength(2));
    expect(saves[1].credit!.rules.map((r) => r.review_actions)).toEqual([['commented', 'logged_time'], ['commented', 'approved', 'logged_time']]);
  });

  it('lowers the actions needed with the actions counted, and keeps one', async () => {
    answerTypes();
    stored = setupWith(undefined, { on: true, rules: [rules()], support: [] });
    render(TrackerPointsSettings);
    await screen.findByTestId('tracker-credit-review');
    const action = (id: string) => screen.getAllByTestId('tracker-credit-action').find((b) => b.dataset.action === id)!;
    await fireEvent.click(action('approved'));
    await waitFor(() => expect(saves).toHaveLength(1));
    await fireEvent.click(action('logged_time'));
    await waitFor(() => expect(saves).toHaveLength(2));
    expect(saves[1].credit!.rules.at(-1)).toMatchObject({ review_actions: ['commented'], review_need: 1 });
    await waitFor(() => expect(line('tracker-credit-review-line')).toContain('at least 1 of: commented.'));
    expect(action('commented')).toBeDisabled();
  });

  it('words its rules in tickets for a project that counts tickets, and says a board estimated in time counts hours', async () => {
    answerTypes();
    stored = setupWith({ count: 'tickets' }, { on: true, rules: [rules()], support: [] });
    render(TrackerPointsSettings);
    await screen.findByTestId('tracker-credit-review');
    expect(line('tracker-credit-review-line')).toContain('10% of each finished ticket, to each reviewer');
    expect(line('tracker-credit-support-line')).toBe('1 ticket for every 4 hours logged on support tickets, which carry no estimate.');
  });

  it('words its rules in tickets for a board with no estimates, which counts tickets by default', async () => {
    answerTypes();
    stored = setupWith(undefined, { on: true, rules: [rules()], support: [] });
    paces = () => [paceOf({ estimate: 'none', counts: 'none' })];
    render(TrackerPointsSettings);
    await screen.findByTestId('tracker-credit-review');
    expect(line('tracker-credit-review-line')).toContain('10% of each finished ticket, to each reviewer');
    expect(line('tracker-credit-support-line')).toBe('1 ticket for every 4 hours logged on support tickets, which carry no estimate.');
  });

  it('says which boards count tickets beside boards that count points', async () => {
    answerTypes();
    stored = setupWith(undefined, { on: true, rules: [rules()], support: [] });
    paces = () => [paceOf(), paceOf({ board: '2', name: 'OPS board', estimate: 'none', counts: 'none' })];
    render(TrackerPointsSettings);
    await screen.findByTestId('tracker-credit-review');
    expect(line('tracker-credit-review-line')).toContain("of a finished ticket's points");
    expect(line('tracker-credit-support-line')).toBe('1 point for every 4 hours logged on support tickets, which carry no points. On OPS board, which counts tickets, a point is a ticket.');
  });

  it('says hours count as they are on a board estimated in time', async () => {
    answerTypes();
    stored = setupWith(undefined, { on: true, rules: [rules()], support: [] });
    paces = () => [paceOf({ estimate: 'time', counts: 'time' })];
    render(TrackerPointsSettings);
    await screen.findByTestId('tracker-credit-review');
    expect(line('tracker-credit-review-line')).toContain("of a finished ticket's estimate");
    expect(line('tracker-credit-support-line')).toContain('On a board estimated in time, the hours count as they are.');
  });

  it('picks the support tickets of the site by type, label and board', async () => {
    answerTypes();
    stored = setupWith(undefined, { on: true, rules: [rules()], support: [] });
    paces = () => [paceOf(), paceOf({ board: '2', name: 'Support board' })];
    render(TrackerPointsSettings);
    const type = async (id: string) => (await screen.findAllByTestId('tracker-credit-type')).find((b) => b.dataset.type === id)!;
    await fireEvent.click(await type('10004'));
    await waitFor(() => expect(saves).toHaveLength(1));
    const site = 'https://example.atlassian.net';
    expect(saves[0].credit!.support).toEqual([{ tracker: 'jira', site, types: ['10004'], labels: [], boards: [] }]);

    const input = screen.getByTestId('tracker-credit-label-input') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: ' support ' } });
    await fireEvent.keyDown(input, { key: 'Enter' });
    await waitFor(() => expect(saves).toHaveLength(2));
    expect(saves[1].credit!.support[0].labels).toEqual(['support']);
    await fireEvent.click(screen.getAllByTestId('tracker-credit-board').find((b) => b.dataset.board === '2')!);
    await waitFor(() => expect(saves).toHaveLength(3));
    expect(saves[2].credit!.support).toEqual([{ tracker: 'jira', site, types: ['10004'], labels: ['support'], boards: ['2'] }]);
    await waitFor(() => expect((screen.getAllByTestId('tracker-credit-label'))[0].textContent).toContain('support'));
    await fireEvent.click(screen.getAllByTestId('tracker-credit-label')[0]);
    await waitFor(() => expect(saves).toHaveLength(4));
    expect(saves[3].credit!.support[0].labels).toEqual([]);
    expect((await type('10004')).getAttribute('aria-pressed')).toBe('true');
  });

  it('says a value the computer refused, and what it stores shows', async () => {
    answerTypes();
    stored = setupWith(undefined, { on: true, rules: [rules()], support: [] });
    setBindingMock('SetProjectTrackerSetup', async () => {
      throw new Error("the project's tracker setup is not valid: rule 1 of credit counts 101% of a ticket's points; the share is 0 to 100");
    });
    render(TrackerPointsSettings);
    await screen.findByTestId('tracker-credit-share');
    await change('tracker-credit-share', 101);
    expect((await screen.findByTestId('settings-tracker-points-save-note')).textContent).toContain('the share is 0 to 100');
    await waitFor(() => expect((screen.getByTestId('tracker-credit-share') as HTMLInputElement).value).toBe('10'));
  });

  it('says a number field takes a whole number, saves nothing, and shows what it stores', async () => {
    answerTypes();
    stored = setupWith(undefined, { on: true, rules: [rules()], support: [] });
    render(TrackerPointsSettings);
    const hours = (await screen.findByTestId('tracker-credit-hours')) as HTMLInputElement;
    hours.value = '2.5';
    await fireEvent.change(hours);
    expect((await screen.findByTestId('tracker-credit-whole')).textContent).toBe('The hours of support for 1 point takes a whole number.');
    expect(hours.value).toBe('4');
    const share = screen.getByTestId('tracker-credit-share') as HTMLInputElement;
    share.value = '';
    await fireEvent.change(share);
    await waitFor(() => expect(screen.getByTestId('tracker-credit-whole').textContent).toBe('The review share takes a whole number.'));
    expect(share.value).toBe('10');
    expect(saves).toHaveLength(0);
    // A whole number saves, and the note goes.
    await change('tracker-credit-hours', 5);
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(screen.queryByTestId('tracker-credit-whole')).toBeNull();
  });

  it('lists the types a project has though the account may create none of them, and retries a read that failed', async () => {
    stored = setupWith(undefined, { on: true, rules: [rules()], support: [] });
    setBindingMock('TrackerBoardProjects', async () => [{ id: '10000', key: 'WEB', name: 'Web' }]);
    setBindingMock('TrackerIssueTypes', async () => []);
    let reads = 0;
    setBindingMock('TrackerProjectIssueTypes', async () => {
      if (++reads === 1) throw new Error('the tracker answered 503');
      return [{ id: '10004', name: 'Support', level: 0 }];
    });
    render(TrackerPointsSettings);
    const failed = await screen.findByTestId('tracker-credit-types-failed');
    expect(failed.textContent).toContain('The issue types of WEB could not be read: The tracker answered 503.');
    await fireEvent.click(screen.getByTestId('tracker-credit-types-retry'));
    expect((await screen.findAllByTestId('tracker-credit-type')).map((b) => b.textContent)).toEqual(['Support']);
    expect(screen.queryByTestId('tracker-credit-types-failed')).toBeNull();
  });

  it('says a credit section a newer version saved, and still changes the estimates', async () => {
    stored = { ...setupWith(undefined), newer: ['credit'] } as SetupView;
    delete stored.setup.credit;
    render(TrackerPointsSettings);
    expect((await screen.findByTestId('tracker-credit-newer')).textContent).toContain("saved this project's credit rules");
    expect(screen.queryByRole('switch', { name: 'Count credit' })).toBeNull();
    expect(screen.getByTestId('tracker-points-count')).toBeInTheDocument();
  });

  it('shows no Credit group for a Linear-only project', async () => {
    setBindingMock('TrackerConnections', async () => [trackerConnection({ id: 'lin-1', kind: 'linear', site: 'acme' })]);
    paces = () => [{ connection: 'lin-1', connection_kind: 'linear', board: 'team-1', name: 'Linear team', read: false, estimate: '', counts: '', plans_sprints: false }];
    render(TrackerPointsSettings);
    await screen.findByTestId('settings-tracker-points-none');
    expect(screen.queryByTestId('tracker-credit')).toBeNull();
  });
});
