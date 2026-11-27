// The Dashboards screen around its store: a failed read of the board's
// dashboards after a list landed shows beside that list, and Refresh
// reads the list again; a dashboard's widgets, the proposals and their
// controls are the list's, so a count that fails leaves every one of them
// to press.

import { fireEvent, render } from '@testing-library/svelte';
import { flushSync, tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { TrackerDashboardsView } from '../../../../bindings/agent-overflow/internal/app/models';
import type { Answer, DashboardView, ProposalView, WidgetView } from '../../../../bindings/agent-overflow/internal/trackerdash/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';
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
    const screen = render(DashboardScreen, { props: { source: SOURCE, boardName: 'WEB board', shown: true, compact: false, computerName: 'Laptop', onOpenTicket: () => {} } });
    unmount = screen.unmount;
    await flush();
    expect(screen.getByTestId('dashboard-title').textContent).toBe('Overview');
    trackerDashboardsUpdated('home');
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
    const screen = render(DashboardScreen, { props: { source: LAPTOP, boardName: 'WEB board', shown: true, compact: false, computerName: 'Laptop', onOpenTicket: () => {} } });
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
