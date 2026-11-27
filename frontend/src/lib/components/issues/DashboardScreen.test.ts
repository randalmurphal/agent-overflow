// The Dashboards screen around its store: a failed read of the board's
// dashboards after a list landed shows beside that list, and Refresh
// reads the list again.

import { render } from '@testing-library/svelte';
import { flushSync, tick } from 'svelte';
import { afterEach, describe, expect, it } from 'vitest';
import type { Answer, DashboardView } from '../../../../bindings/agent-overflow/internal/trackerdash/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { trackerDashboardsUpdated } from '../../stores/trackerBoardEvents';
import DashboardScreen from './DashboardScreen.svelte';

const VIEW = { id: 'overview_sprints', title: 'Overview', default_range: 'sprints_5', ranges: [{ id: 'sprints_5', label: 'Last 5 sprints' }], widgets: [] } as unknown as DashboardView;
const SOURCE = { backend: 'home', board: { backend: 'home', projectId: 'p1', connection: 'conn-1', board: '1' }, subscription: 'sub-1', changes: 0 };

async function flush(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await tick();
  flushSync();
}

let unmount: (() => void) | null = null;
afterEach(() => {
  unmount?.();
  unmount = null;
});

describe('the list of dashboards', () => {
  it('shows a failed read beside the list it keeps, and Refresh reads it again', async () => {
    const answers: Array<() => Promise<DashboardView[]>> = [
      async () => [VIEW],
      async () => { throw new Error('the computer went away'); },
      async () => [VIEW],
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
