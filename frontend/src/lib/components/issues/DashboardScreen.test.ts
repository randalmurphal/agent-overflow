// The Dashboards screen around its store: a failed read of the board's
// dashboards after a list landed shows beside that list, and Refresh
// reads the list again; a dashboard's widgets, the proposals and their
// controls are the list's, so a count that fails leaves every one of them
// to press. A ticket list's moves go to the board's connection, and what
// the tracker refused shows under the row while the dashboard is shown
// and in a toast while it is not.

import { fireEvent, render, within } from '@testing-library/svelte';
import { flushSync, tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { TrackerDashboardsView } from '../../../../bindings/agent-overflow/internal/app/models';
import type { Answer, DashboardView, ProposalView, WidgetView } from '../../../../bindings/agent-overflow/internal/trackerdash/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { getToasts } from '../../stores/toast.svelte';
import { trackerDashboardsUpdated } from '../../stores/trackerBoardEvents';
import { setCarriedSessionScopes } from '../../transport/scopes';
import DashboardScreen from './DashboardScreen.svelte';

const VIEW = { id: 'overview_sprints', title: 'Overview', default_range: 'sprints_5', ranges: [{ id: 'sprints_5', label: 'Last 5 sprints' }], widgets: [] } as unknown as DashboardView;
const SOURCE = { backend: 'home', board: { backend: 'home', projectId: 'p1', connection: 'conn-1', board: '1' }, subscription: 'sub-1', changes: 0 };

async function flush(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await tick();
  flushSync();
}

function listed(dashboards: DashboardView[], proposals: ProposalView[] = []): TrackerDashboardsView {
  return { dashboards, proposals, ask: '' } as TrackerDashboardsView;
}

function widget(id: string, title: string): WidgetView {
  return { id, title, form: 'number', width: 3, unit: 'tickets', how: { words: [], counts: '', range: '', rules: '', sql: '', reads: [] } } as unknown as WidgetView;
}

const KEPT = { ...VIEW, id: 'd_1', title: 'Mine', widgets: [widget('w_1', 'Open')] } as unknown as DashboardView;
const PROPOSED = { view: widget('w_9', 'Proposed'), thread: 't-1' } as unknown as ProposalView;

/** Whether el takes a press: neither it nor what holds it is inert or disabled. */
function pressable(el: HTMLElement): boolean {
  return el.closest('[inert]') === null && !(el as HTMLButtonElement).disabled;
}

let unmount: (() => void) | null = null;
afterEach(() => {
  unmount?.();
  unmount = null;
});

describe('the list of dashboards', () => {
  it('shows a failed read beside the list it keeps, and Refresh reads it again', async () => {
    const answers: Array<() => Promise<TrackerDashboardsView>> = [
      async () => listed([VIEW]),
      async () => { throw new Error('the computer went away'); },
      async () => listed([VIEW]),
    ];
    let lists = 0;
    setBindingMock('TrackerDashboards', () => answers[lists++]());
    setBindingMock('EvaluateTrackerDashboard', () => new Promise<Answer>(() => {}));
    const screen = render(DashboardScreen, { props: { source: SOURCE, boardName: 'WEB board', shown: true, compact: false, computerName: 'Laptop', onOpenTicket: () => {}, pins: [] } });
    unmount = screen.unmount;
    await flush();
    expect(screen.getByTestId('dashboard-title').textContent).toBe('Overview');
    trackerDashboardsUpdated('home', { project: 'p1', board: '1', counts: true });
    await flush();
    expect(screen.getByTestId('dashboard-toolbar')).toBeTruthy();
    expect(screen.getByTestId('dashboard-list-error').textContent).toContain("Could not list this board's dashboards again");
    (screen.component as unknown as { refresh: () => void }).refresh();
    await flush();
    expect(lists).toBe(3);
    expect(screen.queryByTestId('dashboard-list-error')).toBeNull();
  });
});

describe('a count that fails', () => {
  // The board is a paired computer's whose session grants changing its
  // dashboards and starting threads.
  const LAPTOP = { ...SOURCE, backend: 'laptop', board: { ...SOURCE.board, backend: 'laptop' } };
  beforeEach(() => setCarriedSessionScopes('laptop', ['tracker:read', 'tracker:operate', 'threads:read', 'threads:operate']));

  function mount() {
    const screen = render(DashboardScreen, { props: { source: LAPTOP, boardName: 'WEB board', shown: true, compact: false, computerName: 'Laptop', onOpenTicket: () => {}, pins: [] } });
    unmount = screen.unmount;
    return screen;
  }

  it('before its first answer still lays out the proposals with Keep and Dismiss', async () => {
    setBindingMock('TrackerDashboards', async () => listed([VIEW, KEPT], [PROPOSED]));
    setBindingMock('EvaluateTrackerDashboard', async () => { throw new Error('dashboard_too_slow'); });
    const screen = mount();
    await flush();
    expect(screen.getByTestId('dashboard-error')).toBeTruthy();
    expect(pressable(screen.getByTestId('dashboard-keep'))).toBe(true);
    expect(pressable(screen.getByTestId('dashboard-dismiss'))).toBe(true);
  });

  it('says Counting… in a widget only while its count is out', async () => {
    setBindingMock('TrackerDashboards', async () => listed([KEPT], [PROPOSED]));
    let refuse: (err: Error) => void = () => {};
    setBindingMock('EvaluateTrackerDashboard', () => new Promise<Answer>((_resolve, reject) => { refuse = reject; }));
    const screen = mount();
    await flush();
    expect(screen.getAllByTestId('dashboard-widget-waiting').map((el) => el.textContent)).toEqual(['Counting…', 'Counting…']);
    refuse(new Error('dashboard_too_slow'));
    await flush();
    expect(screen.getByTestId('dashboard-error')).toBeTruthy();
    expect(screen.getAllByTestId('dashboard-widget-waiting').map((el) => el.textContent)).toEqual(['Not counted', 'Not counted']);
  });

  it('on a kept dashboard picked from Overview leaves Edit, Remove, Keep and Dismiss to press, and none of Overview\'s numbers', async () => {
    setBindingMock('TrackerDashboards', async () => listed([VIEW, KEPT], [PROPOSED]));
    const overview = { range: { id: 'sprints_5', label: 'Last 5 sprints', periods: [], previous: '' }, estimates: 'points', at: 1, widgets: [{ id: 'w_9', value: { v: 4 } }] } as unknown as Answer;
    setBindingMock('EvaluateTrackerDashboard', async (_sub: string, r: { dashboard: string }) => {
      if (r.dashboard === 'd_1') throw new Error('dashboard_too_slow');
      return overview;
    });
    const removed: unknown[][] = [];
    setBindingMock('RemoveTrackerWidget', async (...args: unknown[]) => { removed.push(args); });
    const screen = mount();
    await flush();
    expect(screen.getByTestId('dashboard-number-value').textContent).toBe('4');
    await fireEvent.click(screen.getByTestId('dashboard-picker'));
    await flush();
    await fireEvent.click(screen.getByTestId('dashboard-pick-d_1'));
    await flush();
    expect(screen.getByTestId('dashboard-title').textContent).toBe('Mine');
    expect(screen.getByTestId('dashboard-error').textContent).toContain('Could not count the dashboard');
    expect(screen.queryAllByTestId('dashboard-number-value')).toHaveLength(0);
    expect(pressable(screen.getByTestId('dashboard-keep'))).toBe(true);
    expect(pressable(screen.getByTestId('dashboard-dismiss'))).toBe(true);
    const edit = screen.getByTestId('dashboard-edit');
    expect(pressable(edit)).toBe(true);
    await fireEvent.click(edit);
    await flush();
    const remove = screen.getByTestId('dashboard-remove');
    expect(pressable(remove)).toBe(true);
    await fireEvent.click(remove);
    await flush();
    expect(removed).toEqual([['sub-1', 'd_1', 'w_1']]);
  });
});

describe('a ticket list', () => {
  const LAPTOP = { ...SOURCE, backend: 'laptop', board: { ...SOURCE.board, backend: 'laptop' } };
  const LIST = { ...VIEW, id: 'd_2', widgets: [{ ...widget('w_list', 'Stuck'), form: 'list', width: 6 }] } as unknown as DashboardView;
  const ROWS = [
    { row: 1, id: '10101', key: 'WEB-1', title: 'First', status: 'To Do', counted: true },
    { row: 2, id: '10102', key: 'WEB-2', title: 'Second', status: 'To Do', counted: true },
  ];

  function mount(scopes: string[]) {
    setCarriedSessionScopes('laptop', scopes);
    setBindingMock('TrackerDashboards', async () => listed([LIST]));
    setBindingMock('EvaluateTrackerDashboard', async () => ({ range: { id: 'sprints_5', label: 'Last 5 sprints', periods: [], previous: '' }, estimates: 'points', at: 1, widgets: [{ id: 'w_list', tickets: ROWS }] } as unknown as Answer));
    const screen = render(DashboardScreen, { props: { source: LAPTOP, boardName: 'WEB board', shown: true, compact: false, computerName: 'Laptop', onOpenTicket: () => {}, pins: [] } });
    unmount = screen.unmount;
    return screen;
  }

  async function moveBoth(screen: ReturnType<typeof mount>): Promise<void> {
    for (const key of ['WEB-1', 'WEB-2']) await fireEvent.click(screen.getByRole('checkbox', { name: `Select ${key}` }));
    await fireEvent.click(screen.getByTestId('ticket-list-move'));
    await vi.waitFor(() => expect(within(document.body).getByTestId('ticket-list-move-status')).toBeTruthy());
    await fireEvent.click(within(document.body).getByTestId('ticket-list-move-status'));
    await vi.waitFor(() => expect(screen.getByTestId('ticket-list-note').dataset.done).toBe('true'));
    await flush();
  }

  it('writes the board\'s connection and says what was refused under the row while shown, else in a toast', async () => {
    setBindingMock('TrackerMoves', async () => [{ id: '31', name: 'Finish', to: { id: 's3', name: 'Done' } }]);
    const moved: string[] = [];
    setBindingMock('MoveTrackerIssue', async (connection: string, input: { issue: string }) => {
      moved.push(`${connection} ${input.issue}`);
      return { parts: [{ step: 'move', state: input.issue === '10102' ? 'refused' : 'applied', refusal: { reason: 'tracker', message: 'Needs a reviewer' } }] };
    });
    const screen = mount(['tracker:read', 'tracker:operate', 'threads:operate']);
    await flush();
    await moveBoth(screen);
    expect(moved.sort()).toEqual(['conn-1 10101', 'conn-1 10102']);
    expect(screen.getAllByTestId('ticket-write-outcome').map((n) => n.textContent?.trim())).toEqual(['Needs a reviewer']);
    expect(getToasts()).toHaveLength(0);

    await fireEvent.click(screen.getByTestId('ticket-write-dismiss'));
    await screen.rerender({ shown: false });
    await moveBoth(screen);
    expect(screen.queryAllByTestId('ticket-write-outcome')).toHaveLength(0);
    expect(getToasts().map((t) => t.message)).toEqual(['The tracker refused the change to WEB-2: Needs a reviewer']);
  });

  it('closes the drill a thread was started from, so the thread opens with nothing over it', async () => {
    setCarriedSessionScopes('laptop', ['tracker:read', 'tracker:operate', 'threads:operate']);
    const NUM = { ...VIEW, id: 'd_3', widgets: [{ ...widget('w_num', 'Stuck'), now: true }] } as unknown as DashboardView;
    setBindingMock('TrackerDashboards', async () => listed([NUM]));
    setBindingMock('EvaluateTrackerDashboard', async () => ({ range: { id: 'sprints_5', label: 'Last 5 sprints', periods: [], previous: '' }, estimates: 'points', at: 1, widgets: [{ id: 'w_num', value: { v: 2 } }] } as unknown as Answer));
    setBindingMock('TrackerDashboardDrill', async () => ({ title: 'Stuck', value: { v: 2 }, unit: 'tickets', estimates: 'points', count: 2, tickets: 2, counted: 2, page: 0, pages: 1, rows: ROWS, at: 1 }));
    setBindingMock('StartTicketsThread', async () => ({ id: 't-drill', projectId: 'p1' }));
    setBindingMock('SwitchThread', async (id: string) => ({ id, projectId: 'p1' }));
    const screen = render(DashboardScreen, { props: { source: LAPTOP, boardName: 'WEB board', shown: true, compact: false, computerName: 'Laptop', onOpenTicket: () => {}, pins: [] } });
    unmount = screen.unmount;
    await flush();
    await fireEvent.click(screen.getByTestId('dashboard-number'));
    const page = within(document.body);
    await vi.waitFor(() => expect(page.getAllByTestId('dashboard-drill-row')).toHaveLength(2));
    for (const key of ['WEB-1', 'WEB-2']) await fireEvent.click(page.getByRole('checkbox', { name: `Select ${key}` }));
    await fireEvent.click(page.getByTestId('ticket-list-thread'));
    await fireEvent.click(page.getByTestId('ticket-list-thread-start'));
    await vi.waitFor(() => expect(page.queryByTestId('dashboard-drill')).toBeNull());
  });

  it('leaves a drill opened after a start alone when that start ends', async () => {
    setCarriedSessionScopes('laptop', ['tracker:read', 'tracker:operate', 'threads:operate']);
    const NUM = { ...VIEW, id: 'd_3', widgets: [{ ...widget('w_num', 'Stuck'), now: true }] } as unknown as DashboardView;
    setBindingMock('TrackerDashboards', async () => listed([NUM]));
    setBindingMock('EvaluateTrackerDashboard', async () => ({ range: { id: 'sprints_5', label: 'Last 5 sprints', periods: [], previous: '' }, estimates: 'points', at: 1, widgets: [{ id: 'w_num', value: { v: 2 } }] } as unknown as Answer));
    setBindingMock('TrackerDashboardDrill', async () => ({ title: 'Stuck', value: { v: 2 }, unit: 'tickets', estimates: 'points', count: 2, tickets: 2, counted: 2, page: 0, pages: 1, rows: ROWS, at: 1 }));
    let made: () => void = () => {};
    setBindingMock('StartTicketsThread', () => new Promise((resolve) => { made = () => resolve({ id: 't-drill', projectId: 'p1' }); }));
    setBindingMock('SwitchThread', async (id: string) => ({ id, projectId: 'p1' }));
    const screen = render(DashboardScreen, { props: { source: LAPTOP, boardName: 'WEB board', shown: true, compact: false, computerName: 'Laptop', onOpenTicket: () => {}, pins: [] } });
    unmount = screen.unmount;
    await flush();
    const page = within(document.body);
    const openDrill = async () => {
      await fireEvent.click(screen.getByTestId('dashboard-number'));
      await vi.waitFor(() => expect(page.getAllByTestId('dashboard-drill-row')).toHaveLength(2));
      await fireEvent.click(page.getByRole('checkbox', { name: 'Select WEB-1' }));
      await fireEvent.click(page.getByTestId('ticket-list-thread'));
    };
    await openDrill();
    await fireEvent.click(page.getByTestId('ticket-list-thread-start'));
    await fireEvent.keyDown(document, { key: 'Escape' });
    await vi.waitFor(() => expect(page.queryByTestId('dashboard-drill')).toBeNull());
    await openDrill();
    await fireEvent.input(page.getByTestId('ticket-list-thread-text'), { target: { value: 'the second question' } });
    made();
    await flush();
    await flush();
    expect(page.getByTestId('dashboard-drill')).toBeTruthy();
    expect((page.getByTestId('ticket-list-thread-text') as HTMLInputElement).value).toBe('the second question');
  });

  it('offers no move or assignment where this device may not change tickets', async () => {
    const screen = mount(['tracker:read', 'threads:operate']);
    await flush();
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Select WEB-1' }));
    expect((screen.getByTestId('ticket-list-move') as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByTestId('ticket-list-assign') as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByTestId('ticket-list-thread') as HTMLButtonElement).disabled).toBe(false);
  });
});

describe("a widget's limit and pin", () => {
  const LAPTOP = { ...SOURCE, backend: 'laptop', board: { ...SOURCE.board, backend: 'laptop' } };
  const STUCK = { ...widget('stuck', 'Stuck right now'), good: 'down', now: true } as unknown as WidgetView;
  const PACE = { ...widget('pace', 'Usual pace'), good: 'up', unit: 'estimate' } as unknown as WidgetView;
  const LIST = { ...widget('stuck_list', 'Stuck tickets'), form: 'list', width: 12 } as unknown as WidgetView;
  const OVERVIEW = { ...VIEW, widgets: [STUCK, PACE, LIST] } as unknown as DashboardView;
  const ANSWER = {
    range: { id: 'sprints_5', label: 'Last 5 sprints', periods: [], previous: '' }, estimates: 'points', at: 1,
    widgets: [{ id: 'stuck', value: { v: 4 } }, { id: 'pace', value: { v: 20 } }, { id: 'stuck_list', tickets: [] }],
  } as unknown as Answer;
  /** The board's pins (its subscription's) and the list's limits and
   * proposals asked for the row. */
  type Tiles = { pins: string[] | null; limits: Record<string, number>; proposed: string[] };

  function mount(tiles: Tiles, opts: { compact?: boolean; proposals?: ProposalView[] } = {}) {
    setCarriedSessionScopes('laptop', ['tracker:read', 'tracker:operate', 'threads:read', 'threads:operate']);
    const state = { tiles };
    setBindingMock('TrackerDashboards', async () => ({ ...listed([OVERVIEW, KEPT], opts.proposals ?? []), tiles: { limits: state.tiles.limits, proposed: state.tiles.proposed, max: 5 } }));
    setBindingMock('EvaluateTrackerDashboard', async () => ANSWER);
    const screen = render(DashboardScreen, { props: { source: LAPTOP, boardName: 'WEB board', shown: true, compact: opts.compact ?? false, computerName: 'Laptop', onOpenTicket: () => {}, pins: tiles.pins } });
    unmount = screen.unmount;
    /** The board's computer stored `next`: its subscription tells the pins
     * and its dashboards event the rest. */
    async function set(next: Tiles): Promise<void> {
      state.tiles = next;
      await screen.rerender({ pins: next.pins });
      trackerDashboardsUpdated('laptop', { project: 'p1', board: '1', counts: false });
      await flush();
    }
    return { screen, set };
  }
  const cell = (screen: { container: HTMLElement }, id: string) => screen.container.querySelector<HTMLElement>(`[data-testid="dashboard-widget"][data-widget="${id}"]`)!;

  it('sets a number\'s limit from its bell, marks the number past it, and turns it off', async () => {
    const limits: unknown[][] = [];
    setBindingMock('SetTrackerWidgetLimit', async (...args: unknown[]) => { limits.push(args); });
    const { screen, set } = mount({ pins: [], limits: {}, proposed: [] });
    await flush();
    const stuck = cell(screen, 'stuck');
    expect(within(stuck).getByTestId('dashboard-limit-toggle').title).toBe('Warn me at a limit');
    expect(within(cell(screen, 'stuck_list')).queryByTestId('dashboard-limit-toggle')).toBeNull();
    await fireEvent.click(within(stuck).getByTestId('dashboard-limit-toggle'));
    await flush();
    const pop = within(document.body).getByTestId('dashboard-limit');
    expect(pop.textContent).toContain('Warn me when this is above');
    expect(within(pop).getByTestId('dashboard-limit-value').textContent).toBe('4');
    expect(within(pop).getByTestId('dashboard-limit-note').textContent).toBe('No limit is set.');
    await fireEvent.click(within(pop).getByTestId('dashboard-limit-switch'));
    await flush();
    expect(limits).toEqual([['sub-1', 'stuck', 4]]);

    // The board's computer stored 3 and said so.
    await set({ pins: [], limits: { stuck: 3 }, proposed: [] });
    expect(stuck.dataset.over).toBe('true');
    expect(within(stuck).getByTestId('dashboard-over').textContent?.trim()).toBe('over your limit of 3 tickets');
    expect(within(stuck).getByTestId('dashboard-limit-toggle').title).toBe('Warns above 3 tickets');
    expect(within(pop).getByTestId('dashboard-limit-note').textContent).toBe('The tile turns amber.');
    await fireEvent.click(within(pop).getByTestId('dashboard-limit-raise'));
    await flush();
    await fireEvent.click(within(pop).getByTestId('dashboard-limit-switch'));
    await flush();
    expect(limits).toEqual([['sub-1', 'stuck', 4], ['sub-1', 'stuck', 4], ['sub-1', 'stuck', null]]);
  });

  it('raises twice from the limit the first raise stored while the list read after it is out', async () => {
    const writes: unknown[] = [];
    setBindingMock('SetTrackerWidgetLimit', async (_sub: string, _id: string, limit: unknown) => {
      writes.push(limit);
      // The computer tells the change before it answers.
      trackerDashboardsUpdated('laptop', { project: 'p1', board: '1', counts: false });
    });
    const { screen } = mount({ pins: [], limits: { stuck: 3 }, proposed: [] });
    await flush();
    // Every list after the first is out until the test answers it.
    const lists: Array<() => void> = [];
    setBindingMock('TrackerDashboards', () => new Promise((resolve) => lists.push(() => resolve({ ...listed([OVERVIEW, KEPT]), tiles: { limits: { stuck: 4 }, proposed: [], max: 5 } }))));
    await fireEvent.click(within(cell(screen, 'stuck')).getByTestId('dashboard-limit-toggle'));
    await flush();
    const pop = within(document.body).getByTestId('dashboard-limit');
    await fireEvent.click(within(pop).getByTestId('dashboard-limit-raise'));
    await flush();
    expect(lists.length).toBeGreaterThan(0);
    await fireEvent.click(within(pop).getByTestId('dashboard-limit-raise'));
    await flush();
    expect(writes).toEqual([4, 5]);
    expect(within(pop).getByTestId('dashboard-limit-value').textContent).toBe('5');
  });

  it('shows a limit only while the list has it in the widget\'s unit now, and again when it is back', async () => {
    const { screen, set } = mount({ pins: [], limits: { pace: 25 }, proposed: [] });
    await flush();
    const pace = cell(screen, 'pace');
    expect(pace.dataset.over).toBe('true');
    // The project counts in tickets now: the list leaves the limit set in
    // points out, so the widget shows none and no mark.
    await set({ pins: [], limits: {}, proposed: [] });
    expect(pace.dataset.over).toBe('false');
    expect(within(pace).queryByTestId('dashboard-over')).toBeNull();
    expect(within(pace).getByTestId('dashboard-limit-toggle').dataset.on).toBe('false');
    await set({ pins: [], limits: { pace: 25 }, proposed: [] });
    expect(pace.dataset.over).toBe('true');
    expect(within(pace).getByTestId('dashboard-limit-toggle').dataset.on).toBe('true');
  });

  it('warns below a limit where up is good news', async () => {
    const { screen } = mount({ pins: ['pace'], limits: { pace: 25 }, proposed: [] });
    await flush();
    const pace = cell(screen, 'pace');
    expect(pace.dataset.over).toBe('true');
    expect(within(pace).getByTestId('dashboard-over').textContent?.trim()).toBe('under your limit of 25 points');
    await fireEvent.click(within(pace).getByTestId('dashboard-limit-toggle'));
    await flush();
    const pop = within(document.body).getByTestId('dashboard-limit');
    expect(pop.textContent).toContain('Warn me when this is below');
    expect(within(pop).getByTestId('dashboard-limit-note').textContent).toBe('The tile turns amber, here and on the board row.');
  });

  it('pins a widget to the board row and takes no pin once the row is full', async () => {
    const pins: unknown[][] = [];
    setBindingMock('PinTrackerWidget', async (...args: unknown[]) => { pins.push(args); });
    const { screen, set } = mount({ pins: [], limits: {}, proposed: [] });
    await flush();
    const pin = within(cell(screen, 'stuck_list')).getByTestId('dashboard-pin');
    expect(pin.title).toBe('Show on the board row');
    await fireEvent.click(pin);
    await flush();
    expect(pins).toEqual([['sub-1', 'stuck_list', true]]);

    await set({ pins: ['a', 'b', 'c', 'd', 'stuck_list'], limits: {}, proposed: [] });
    const on = within(cell(screen, 'stuck_list')).getByTestId('dashboard-pin');
    expect(on.getAttribute('aria-pressed')).toBe('true');
    expect(on.title).toBe('Take off the board row');
    const full = within(cell(screen, 'stuck')).getByTestId('dashboard-pin') as HTMLButtonElement;
    expect(full.disabled).toBe(true);
    expect(full.title).toBe("The board's row already shows 5 tiles. Take one off to pin this one.");
    await fireEvent.click(on);
    await flush();
    expect(pins).toEqual([['sub-1', 'stuck_list', true], ['sub-1', 'stuck_list', false]]);
  });

  it('shows no bell or pin until the board\'s pins are read', async () => {
    const { screen } = mount({ pins: null, limits: {}, proposed: [] });
    await flush();
    expect(screen.getAllByTestId('dashboard-widget')).toHaveLength(3);
    expect(screen.queryAllByTestId('dashboard-limit-toggle')).toHaveLength(0);
    expect(screen.queryAllByTestId('dashboard-pin')).toHaveLength(0);
  });

  it('under compact shows only the controls switched on until the dashboard is edited, Overview included', async () => {
    const { screen } = mount({ pins: ['stuck_list'], limits: {}, proposed: [] }, { compact: true });
    await flush();
    expect(screen.queryAllByTestId('dashboard-limit-toggle')).toHaveLength(0);
    expect(screen.getAllByTestId('dashboard-pin').map((b) => b.closest<HTMLElement>('[data-widget]')!.dataset.widget)).toEqual(['stuck_list']);
    await fireEvent.click(screen.getByTestId('dashboard-edit'));
    await flush();
    expect(screen.getAllByTestId('dashboard-limit-toggle')).toHaveLength(2);
    expect(screen.getAllByTestId('dashboard-pin')).toHaveLength(3);
    expect(screen.queryAllByTestId('dashboard-remove')).toHaveLength(0);
  });

  it('says what Keep does with a proposal its agent asked to show on the board row', async () => {
    const asked = { view: widget('w_8', 'Reviews waiting'), thread: 't-1' } as unknown as ProposalView;
    const { screen, set } = mount({ pins: [], limits: {}, proposed: ['w_8'] }, { proposals: [asked, PROPOSED] });
    await flush();
    expect(screen.getAllByTestId('dashboard-keep-pin').map((n) => n.textContent)).toEqual(['Keep also shows it on the board row.']);
    await set({ pins: ['a', 'b', 'c', 'd', 'e'], limits: {}, proposed: ['w_8'] });
    expect(screen.getAllByTestId('dashboard-keep-pin').map((n) => n.textContent)).toEqual(['Asked for the board row, which already shows 5 tiles: Keep adds it to a dashboard only.']);
    // A proposal takes no bell or pin until it is kept.
    expect(within(cell(screen, 'w_8')).queryByTestId('dashboard-pin')).toBeNull();
  });
});
