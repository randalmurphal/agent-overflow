// Real-Chromium suite for a drill's keyboard focus: the drill is a dialog,
// and the lists and the form its selection opens are drawn outside it
// (portaled), so outside its focus trap. Tab from a menu closes it back
// onto its button, and Tab in the thread form stays in the form; focus
// never reaches the page behind the dialog.

import { afterEach, expect, it, vi } from 'vitest';
import { mount, unmount } from 'svelte';
import { userEvent } from 'vitest/browser';
import '../../../app.css';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import type { DrillState } from '../../stores/trackerDashboard.svelte';
import DashboardDrill from './DashboardDrill.svelte';
import type { TicketListSource } from './TicketListActions.svelte';
import { ticketWriteSurfaceContext } from './ticketWriteSurface';
import { waitFor } from '../../../test/helpers/browserFrames';

const TICKETS: TicketListSource = { backend: '', connection: 'c1', project: 'p1', writeBlocked: '', threadBlocked: '' };

const mounted: { app: object; host: HTMLElement }[] = [];

afterEach(() => {
  for (const { app, host } of mounted.splice(0)) {
    unmount(app);
    host.remove();
  }
});

const el = (testId: string) => document.querySelector<HTMLElement>(`[data-testid="${testId}"]`);

function mountDrill(): HTMLButtonElement {
  setBindingMock('TrackerMoves', async () => [{ id: 'm1', name: 'Finish', to: { id: 's3', name: 'Done' } }]);
  setBindingMock('TrackerAssignableUsers', async () => ({ users: [{ id: 'acc-user1', name: 'User 1' }], more: false }));
  const host = document.createElement('div');
  document.body.appendChild(host);
  // A control of the page behind the dialog, first in the page's Tab order.
  const behind = document.createElement('button');
  behind.textContent = 'Behind the dialog';
  host.appendChild(behind);
  const rows = [1, 2].map((n) => ({ row: n, id: `${10100 + n}`, key: `WEB-${n}`, title: `Ticket ${n}`, status: 'To Do', counted: true }));
  const drill = {
    request: { widget: 'stuck' }, loading: false, error: null,
    answer: { title: 'Stuck', value: { v: 2 }, unit: 'tickets', estimates: 'points', count: 2, tickets: 2, counted: 2, page: 0, pages: 1, rows, at: 0 },
  } as unknown as DrillState;
  const app = mount(DashboardDrill, {
    target: host,
    context: ticketWriteSurfaceContext(() => true),
    props: { drill, onPage: vi.fn(), onTicket: vi.fn(), tickets: TICKETS, onClose: vi.fn() },
  });
  mounted.push({ app, host });
  return behind;
}

async function select(): Promise<void> {
  await waitFor(() => document.querySelector('[role="checkbox"][aria-label="Select WEB-1"]') !== null, 'the drill lists its rows');
  (document.querySelector('[role="checkbox"][aria-label="Select WEB-1"]') as HTMLElement).click();
  await waitFor(() => el('ticket-list-bar') !== null, 'the selection shows its actions');
}

const inDialog = () => document.activeElement?.closest('[role="dialog"]') !== null;

it('closes a drill menu back onto its button on Tab', async () => {
  const behind = mountDrill();
  await select();
  for (const [button, opened] of [['ticket-list-move', 'ticket-list-move-title'], ['ticket-list-assign', 'ticket-picker-find']] as const) {
    el(button)!.click();
    await waitFor(() => el(opened) !== null, `${button} opens its list`);
    // The list is drawn outside the dialog and takes focus there.
    await waitFor(() => !inDialog() && document.activeElement !== document.body && document.activeElement !== behind, `the list ${button} opened takes focus`);
    await userEvent.keyboard('{Tab}');
    await waitFor(() => el(opened) === null, `Tab closes the list ${button} opened`);
    await waitFor(() => document.activeElement === el(button), `focus is back on ${button}`);
    expect(document.activeElement).not.toBe(behind);
    expect(inDialog()).toBe(true);
  }
});

it('keeps Tab inside the thread form a drill opens', async () => {
  const behind = mountDrill();
  await select();
  el('ticket-list-thread')!.click();
  const text = () => el('ticket-list-thread-text');
  await waitFor(() => text() !== null && document.activeElement === text(), 'the form opens on its text');
  await userEvent.keyboard('why is this stuck');
  await userEvent.keyboard('{Tab}');
  await waitFor(() => document.activeElement === el('ticket-list-thread-start'), 'Tab reaches Start');
  await userEvent.keyboard('{Tab}');
  await waitFor(() => document.activeElement === text(), 'Tab from Start returns to the text');
  expect((text() as HTMLInputElement).value).toBe('why is this stuck');
  await userEvent.keyboard('{Shift>}{Tab}{/Shift}');
  await waitFor(() => document.activeElement === el('ticket-list-thread-start'), 'Shift+Tab goes back to Start');
  expect(document.activeElement).not.toBe(behind);
  // Escape closes the form alone; the drill stays, with focus in it.
  await userEvent.keyboard('{Escape}');
  await waitFor(() => text() === null, 'Escape closes the form');
  expect(el('dashboard-drill-heading')).not.toBeNull();
  await waitFor(() => inDialog(), 'focus is back in the drill');
});
