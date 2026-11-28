// Settings > Issues > Points and credit: What a ticket is worth picks the
// boards' estimates or counting tickets and saves at once; Usual pace says
// each sprint-planning Jira board's pace in the unit the project counts
// it in, read again when the setup changes; Planning poker turns off and
// on; a Linear-only project, and one with no boards, say they have none.
// One read (ProjectTrackerPace) lists the boards with their pace. The
// Credit group turns credit on and off, keeping its rules; changes a rule
// from this sprint or for every sprint; words its rules in the unit the
// project counts; picks which tickets are support; and says a credit
// section a newer version saved. The Forge accounts item lists who each
// forge account is while a rule counts comments or approvals, and sends a
// person's word on one.

import { tick } from 'svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import type { BoardPace, Credit, CreditRules, ForgeAccountInput, ForgeAccountView, ForgeAccountsView, Setup, SetupView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import TrackerPointsSettings from './TrackerPointsSettings.svelte';
import ComputerSettingsPage from './ComputerSettingsPage.svelte';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { getPinnedBackend } from '../../transport/backends';
import { setCarriedSessionScopes } from '../../transport/scopes';
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

  /** Makes the page's held setup stale: a reread fails and the page keeps
   * showing the last setup read, so the next save reads it again first;
   * that read answers when the returned function is called. */
  async function staleSetup(): Promise<(view: SetupView) => void> {
    let failed = false;
    setBindingMock('ProjectTrackerSetup', async () => {
      failed = true;
      throw new Error('the computer did not answer');
    });
    trackerSetupUpdated(HOME_BACKEND);
    await waitFor(() => expect(failed).toBe(true));
    let answer!: (view: SetupView) => void;
    setBindingMock('ProjectTrackerSetup', () => new Promise<SetupView>((resolve) => { answer = resolve; }));
    return (view) => answer(view);
  }

  it('makes an edit in the scope chosen when it was made, though the scope changes while its save waits', async () => {
    answerTypes();
    const held = setupWith(undefined, { on: true, rules: [rules(), rules({ from: 1_800_000_000_000, hours_per_point: 6 })], support: [] });
    stored = held;
    render(TrackerPointsSettings);
    await screen.findByTestId('tracker-credit-hours');
    const answer = await staleSetup();
    await change('tracker-credit-hours', 8);
    // This sprint on was chosen; Every sprint is picked before the read
    // the save waits for answers.
    await fireEvent.click(screen.getByRole('radio', { name: 'Every sprint' }));
    answer(held);
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0].credit!.rules.map((r) => r.hours_per_point)).toEqual([4, 6, 8]);
  });

  it('picks a support board or type as the page showed it, whatever the setup read before the save picked', async () => {
    answerTypes();
    stored = setupWith(undefined, { on: true, rules: [rules()], support: [] });
    paces = () => [paceOf(), paceOf({ board: '2', name: 'Support board' })];
    render(TrackerPointsSettings);
    const type = async (id: string) => (await screen.findAllByTestId('tracker-credit-type')).find((b) => b.dataset.type === id)!;
    await type('10004');
    const answer = await staleSetup();
    // Another window picked board 2 and type 10004; this page still shows
    // neither and picks both.
    const site = 'https://example.atlassian.net';
    const other = setupWith(undefined, { on: true, rules: [rules()], support: [{ tracker: 'jira', site, types: ['10004'], labels: [], boards: ['2'] }] });
    await fireEvent.click(screen.getAllByTestId('tracker-credit-board').find((b) => b.dataset.board === '2')!);
    answer({ ...other, revision: 'other' });
    await waitFor(() => expect(saves).toHaveLength(1));
    expect(saves[0].credit!.support).toEqual([{ tracker: 'jira', site, types: ['10004'], labels: [], boards: ['2'] }]);
    await waitFor(() => expect((screen.getAllByTestId('tracker-credit-board').find((b) => b.dataset.board === '2')!).getAttribute('aria-pressed')).toBe('true'));
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

  it('says a board estimated in time counts its support hours as they are in a project that counts tickets', async () => {
    answerTypes();
    stored = setupWith({ count: 'tickets' }, { on: true, rules: [rules()], support: [] });
    paces = () => [paceOf({ estimate: 'time', counts: 'none' })];
    render(TrackerPointsSettings);
    await screen.findByTestId('tracker-credit-review');
    expect(line('tracker-credit-support-line')).toBe('1 ticket for every 4 hours logged on support tickets, which carry no estimate. On a board estimated in time, the hours count as they are.');
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

  it('keeps a number typed while an earlier save of the field was out', async () => {
    answerTypes();
    stored = setupWith(undefined, { on: true, rules: [rules()], support: [] });
    let refuse!: () => void;
    setBindingMock('SetProjectTrackerSetup', (_project: string, _revision: string, setup: Setup) => new Promise((resolve) => {
      saves.push(setup);
      refuse = () => resolve({ saved: false, refused: 'the setup changed', stored });
    }));
    render(TrackerPointsSettings);
    const hours = (await screen.findByTestId('tracker-credit-hours')) as HTMLInputElement;
    hours.value = '6';
    await fireEvent.change(hours);
    await waitFor(() => expect(saves).toHaveLength(1));
    // The person types 7 before the save of 6 is refused.
    hours.value = '7';
    refuse();
    expect((await screen.findByTestId('settings-tracker-points-save-note')).textContent).toContain('The setup changed.');
    // The field's own settling runs after the page's.
    for (let i = 0; i < 3; i++) await tick();
    expect(hours.value).toBe('7');
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

describe('the Forge accounts item', () => {
  const account = (login: string, how: string, extra: Partial<ForgeAccountView> = {}): ForgeAccountView =>
    ({ host: 'github.com', login, how, ...extra }) as ForgeAccountView;
  const user2 = { id: 'acc-user2', name: 'User 2' };
  let listed: ForgeAccountView[];
  let words: [string, ForgeAccountInput][];
  let reads: number;

  beforeEach(() => {
    stored = setupWith(undefined, { on: true, rules: [rules()], support: [] });
    setBindingMock('TrackerBoardProjects', async () => [{ id: '10000', key: 'WEB', name: 'Web' }]);
    setBindingMock('TrackerProjectIssueTypes', async () => []);
    listed = [
      account('ghost', 'nobody', { name: 'Ghost Dev' }),
      account('nameless', 'nobody'),
      account('new', 'unchecked', { name: 'New Dev' }),
      account('ci', 'not_a_person', { name: 'CI' }),
      account('me', 'own', { name: 'User One', person: { id: 'acc-user1', name: 'User 1' } }),
      account('u2', 'name', { name: 'User 2', person: user2 }),
      account('alias', 'paired', { name: 'Alias', person: { id: 'acc-user3' } }),
    ];
    reads = 0;
    setBindingMock('TrackerForgeAccounts', async (): Promise<ForgeAccountsView> => {
      reads++;
      return { accounts: listed } as ForgeAccountsView;
    });
    words = [];
    // The computer answers the accounts as the word leaves them.
    setBindingMock('SetTrackerForgeAccount', async (connection: string, word: ForgeAccountInput): Promise<ForgeAccountsView> => {
      words.push([connection, word]);
      listed = listed.map((a) => {
        if (a.login !== word.login) return a;
        if (word.who === 'person') return account(a.login, 'paired', { name: a.name, person: { id: word.person!, name: word.person_name } });
        return account(a.login, word.who === 'not_a_person' ? 'not_a_person' : 'unchecked', { name: a.name });
      });
      return { accounts: listed } as ForgeAccountsView;
    });
  });

  const row = (login: string) => screen.getAllByTestId('tracker-forge-account').find((el) => el.dataset.login === login)!;
  const who = (login: string) => within(row(login)).getByTestId('tracker-forge-account-who').textContent?.trim();
  const buttons = (login: string) => within(row(login)).getAllByRole('button').map((b) => b.textContent?.trim());

  it('lists each account with who it is and how that is known, and the changes each offers', async () => {
    render(TrackerPointsSettings);
    await screen.findAllByTestId('tracker-forge-account');
    expect(screen.getAllByTestId('tracker-forge-account').map((el) => el.textContent?.replace(/\s+/g, ' ').trim().split(' Pick a person')[0].split(' Change person')[0])).toEqual([
      'Ghost Dev · ghost on github.com Nobody: no person in the tracker, or more than one, has this name. Its reviews count for nobody.',
      'nameless on github.com Nobody: the forge gives this account no name. Its reviews count for nobody.',
      'New Dev · new on github.com Not matched yet. The tracker is asked within a few minutes.',
      'CI · ci on github.com Not a person, as set here. Its reviews count for nobody.',
      'User One · me on github.com User 1, the account this computer reads the forge as',
      'User 2 · u2 on github.com User 2, matched by name',
      'Alias · alias on github.com acc-user3, as set here',
    ]);
    expect(buttons('ghost')).toEqual(['Pick a person…', 'Not a person']);
    expect(buttons('ci')).toEqual(['Pick a person…', 'Match by name']);
    expect(buttons('u2')).toEqual(['Change person…', 'Not a person']);
    expect(buttons('alias')).toEqual(['Change person…', 'Not a person', 'Match by name']);
    expect(reads).toBe(1);
    expect(screen.queryByTestId('tracker-forge-accounts-more')).toBeNull();
  });

  it('names an account once where the forge gives its login as its name', async () => {
    listed = [account('same', 'unchecked', { name: 'same' })];
    render(TrackerPointsSettings);
    const row = await screen.findByTestId('tracker-forge-account');
    expect(row.textContent?.replace(/\s+/g, ' ').trim().split(' Pick a person')[0]).toBe('same on github.com Not matched yet. The tracker is asked within a few minutes.');
  });

  it('shows only while credit is on and a rule counts comments or approvals', async () => {
    stored = setupWith(undefined, { on: true, rules: [rules({ review_need: 1, review_actions: ['logged_time'] })], support: [] });
    render(TrackerPointsSettings);
    await screen.findByTestId('tracker-credit-review');
    expect(screen.queryByTestId('tracker-forge-accounts')).toBeNull();
    expect(reads, 'nothing is read for a list that does not show').toBe(0);

    await fireEvent.click(screen.getAllByTestId('tracker-credit-action').find((b) => b.dataset.action === 'approved')!);
    await screen.findAllByTestId('tracker-forge-account');
    expect(reads).toBe(1);

    await fireEvent.click(screen.getByRole('switch', { name: 'Count credit' }));
    await waitFor(() => expect(screen.queryByTestId('tracker-forge-accounts')).toBeNull());
  });

  it("pairs an account with a person found in the tracker by the text typed, asking the boards' projects", async () => {
    const searches: unknown[][] = [];
    setBindingMock('TrackerPeople', async (...args: unknown[]) => {
      searches.push(args);
      return args[2] === '' ? { users: [], more: true } : { users: [user2, { id: 'acc-user4' }], more: false };
    });
    render(TrackerPointsSettings);
    await screen.findAllByTestId('tracker-forge-account');
    await fireEvent.click(within(row('ghost')).getByTestId('tracker-forge-account-pick'));
    const find = await screen.findByTestId('ticket-picker-find');
    expect(find.getAttribute('aria-label')).toBe('Find who ghost is');
    await fireEvent.input(find, { target: { value: 'use' } });
    const items = await screen.findAllByTestId('ticket-picker-item');
    expect(items.map((el) => el.textContent?.trim()), 'a person with no name is listed by ID').toEqual(['User 2', 'acc-user4']);
    expect(searches).toEqual([['jira-1', ['10000'], ''], ['jira-1', ['10000'], 'use']]);
    await fireEvent.click(items[0]);
    await waitFor(() => expect(who('ghost')).toBe('User 2, as set here'));
    expect(words).toEqual([['jira-1', { host: 'github.com', login: 'ghost', who: 'person', person: 'acc-user2', person_name: 'User 2' }]]);
    expect(buttons('ghost')).toEqual(['Change person…', 'Not a person', 'Match by name']);
    expect(screen.queryByTestId('ticket-picker-find'), 'a pick closes the list').toBeNull();
    expect(reads, 'the list shows the answer of the word').toBe(1);
  });

  it('says an account is no person, and has one match by name again', async () => {
    render(TrackerPointsSettings);
    await screen.findAllByTestId('tracker-forge-account');
    await fireEvent.click(within(row('u2')).getByTestId('tracker-forge-account-no-person'));
    await waitFor(() => expect(who('u2')).toBe('Not a person, as set here. Its reviews count for nobody.'));
    await fireEvent.click(within(row('alias')).getByTestId('tracker-forge-account-by-name'));
    await waitFor(() => expect(who('alias')).toBe('Not matched yet. The tracker is asked within a few minutes.'));
    expect(words).toEqual([
      ['jira-1', { host: 'github.com', login: 'u2', who: 'not_a_person' }],
      ['jira-1', { host: 'github.com', login: 'alias', who: 'by_name' }],
    ]);
  });

  it('says under its account a word the computer refused, until the next word on it, and shows what the computer holds', async () => {
    render(TrackerPointsSettings);
    await screen.findAllByTestId('tracker-forge-account');
    let refuse = true;
    setBindingMock('SetTrackerForgeAccount', async (_c: string, word: ForgeAccountInput) => {
      if (refuse) throw new Error('this tracker connection is turned off');
      listed = listed.map((a) => (a.login === word.login ? account(a.login, 'not_a_person', { name: a.name }) : a));
      return { accounts: listed };
    });
    await fireEvent.click(within(row('ghost')).getByTestId('tracker-forge-account-no-person'));
    expect((await within(row('ghost')).findByTestId('tracker-forge-account-failed')).textContent).toBe('This tracker connection is turned off.');
    await waitFor(() => expect(reads, 'a failed word reads what the computer holds').toBe(2));
    expect(within(row('nameless')).queryByTestId('tracker-forge-account-failed')).toBeNull();
    refuse = false;
    await fireEvent.click(within(row('ghost')).getByTestId('tracker-forge-account-no-person'));
    await waitFor(() => expect(who('ghost')).toBe('Not a person, as set here. Its reviews count for nobody.'));
    expect(within(row('ghost')).queryByTestId('tracker-forge-account-failed')).toBeNull();
  });

  it('says a read that failed and reads again when asked, and says when no account or more than it lists acted', async () => {
    let fail = true;
    setBindingMock('TrackerForgeAccounts', async () => {
      reads++;
      if (fail) throw new Error('the database is locked');
      return { accounts: reads === 2 ? [] : listed, more: reads > 2 };
    });
    render(TrackerPointsSettings);
    expect((await screen.findByTestId('tracker-forge-accounts-error')).textContent?.trim()).toBe('The forge accounts could not be read: The database is locked.');
    fail = false;
    await fireEvent.click(screen.getByTestId('tracker-forge-accounts-refresh'));
    expect((await screen.findByTestId('tracker-forge-accounts-none')).textContent?.trim()).toBe(
      "No account has approved or commented on a pull request of these boards' tickets that this computer has read.");
    expect(screen.queryByTestId('tracker-forge-accounts-error')).toBeNull();
    await fireEvent.click(screen.getByTestId('tracker-forge-accounts-refresh'));
    expect((await screen.findByTestId('tracker-forge-accounts-more')).textContent?.trim()).toBe(
      'More accounts have acted than are listed: those set here, then those matched to nobody, then the rest by name.');
    expect(screen.getAllByTestId('tracker-forge-account')).toHaveLength(listed.length);
  });

  it("lists each connection's accounts under its site when the project's boards are of two", async () => {
    paces = () => [paceOf(), paceOf({ connection: 'jira-2', site: 'https://example2.atlassian.net', board: '7', name: 'OPS board' }), paceOf({ board: '3', name: 'Second' })];
    const asked: string[] = [];
    setBindingMock('TrackerForgeAccounts', async (connection: string) => {
      asked.push(connection);
      return { accounts: [account(connection === 'jira-1' ? 'ghost' : 'other', 'nobody')] };
    });
    render(TrackerPointsSettings);
    await waitFor(() => expect(screen.getAllByTestId('tracker-forge-account')).toHaveLength(2));
    expect(asked.sort(), 'one read a connection, however many boards').toEqual(['jira-1', 'jira-2']);
    expect(screen.getAllByTestId('tracker-forge-accounts-connection').map((el) => [el.dataset.connection, el.textContent?.includes('example2.atlassian.net'), within(el).getByTestId('tracker-forge-account').dataset.login]))
      .toEqual([['jira-1', false, 'ghost'], ['jira-2', true, 'other']]);
    await fireEvent.click(within(screen.getAllByTestId('tracker-forge-accounts-connection')[1]).getByTestId('tracker-forge-account-no-person'));
    await waitFor(() => expect(words).toEqual([['jira-2', { host: 'github.com', login: 'other', who: 'not_a_person' }]]));
  });

  it('offers no word to a device that may not operate the trackers of an attached computer, and reads its accounts there', async () => {
    stageBackend();
    noteProject('p1', 'laptop');
    setCarriedSessionScopes('laptop', ['tracker:read']);
    const pins: (string | null)[] = [];
    setBindingMock('TrackerForgeAccounts', async () => {
      pins.push(getPinnedBackend());
      return { accounts: listed };
    });
    render(ComputerSettingsPage, { props: { backend: 'laptop', Page: TrackerPointsSettings, needsComputer: false } });
    await screen.findAllByTestId('tracker-forge-account');
    expect(pins).toEqual(['laptop']);
    const controls = within(row('alias')).getAllByRole('button') as HTMLButtonElement[];
    expect(controls.map((b) => [b.disabled, b.title])).toEqual(
      Array.from({ length: 3 }, () => [true, "This device may not change a project's points and credit settings on this computer."]));
    expect((screen.getByTestId('tracker-forge-accounts-refresh') as HTMLButtonElement).disabled, 'reading again is no change').toBe(false);
    resetStagedBackends();
  });
});
