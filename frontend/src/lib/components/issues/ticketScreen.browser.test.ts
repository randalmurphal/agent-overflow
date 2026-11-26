// Real-Chromium suite for the ticket's narrow layout: under 720 wide the
// ticket shows status, assignee, priority and points in one row over the
// content, at a phone's width as at a wider one; and a Start work asked for
// (a Board card's) shows its form in the layout the ticket settles on,
// narrow or wide, whether the ticket's detail is there when it mounts or
// arrives after. And a description emptied in its editor is sent as a
// clear from the text it was edited from, whose stale refusal keeps the
// editor open with the tracker's sentence.

import { afterEach, describe, expect, it } from 'vitest';
import { mount, unmount } from 'svelte';
import { SvelteMap } from 'svelte/reactivity';
import '../../../app.css';
import TicketScreen from './TicketScreen.svelte';
import type { DetailView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import type { TrackerTicketAttachment, TrackerTicketState } from '../../stores/trackerTicket.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';
import { noteProject } from '../../transport/entityIndex';
import { addProjectLocal } from '../../stores/projects.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { waitFor } from '../../../test/helpers/browserFrames';

const mounted: { app: object; host: HTMLElement }[] = [];

afterEach(() => {
  for (const { app, host } of mounted.splice(0)) {
    unmount(app);
    host.remove();
  }
});

const DETAIL = {
  card: {
    id: '1', key: 'WEB-1', title: 'Issue 1', status: { id: 's2', name: 'In Progress', group: 'in_progress' },
    assignee: { id: 'u1', name: 'User 1' }, priority: { id: '2', name: 'High' }, type: { id: 't1', name: 'Task' }, created: 0, updated: 0,
  },
  url: 'https://example.atlassian.net/browse/WEB-1', description: 'The description', fields: [], comments: [], links: [], attachments: [], history: [],
  rows: [{ board: '1', estimate: 3, unit: 'points' }], moves: [],
  sprints: [{ id: '8', name: 'Sprint 8', state: 'active' }, { id: '90' }, { id: '91' }],
} as unknown as DetailView;

const state = (detail: DetailView | null): TrackerTicketState => ({
  detail, readAt: detail ? 1 : 0, failure: null, ended: false, refreshing: false, refreshNotice: null, readError: null,
  earlier: { pages: [], next: null, loading: false, error: null }, poker: null,
});

/** Mounts the ticket `width` wide. With `ask`, its open asked for the
 * Start work form, in project p1, until the screen ends the ask; the
 * ticket holds `detail` until `land` gives it the detail. */
function mountTicket(width: number, ask?: { detail: DetailView | null }) {
  const host = document.createElement('div');
  host.style.cssText = `position: fixed; top: 0; left: 0; width: ${width}px; height: 700px; display: flex; flex-direction: column;`;
  document.body.appendChild(host);
  const held = new SvelteMap<string, TrackerTicketState | boolean>([['state', state(ask ? ask.detail : DETAIL)], ['asked', ask !== undefined]]);
  const ref = { backend: HOME_BACKEND, connection: 'c1', issue: '1' };
  const attachment: TrackerTicketAttachment = {
    key: 'ticket', ref, get current() { return held.get('state') as TrackerTicketState; }, error: null,
    tracker: { kind: 'jira', site: 'https://example.atlassian.net', account: null, confirmed: true }, setShown: () => {}, setPokerShown: () => {}, release: () => {},
  };
  const app = mount(TicketScreen, {
    target: host,
    props: {
      attachment, seed: { key: 'WEB-1', title: 'Issue 1', card: null }, board: '1', computerName: 'This computer',
      project: ask ? 'p1' : null, onProject: () => {}, offline: false, compact: false, shown: true, trail: 0,
      focusAsked: false, onFocusTaken: () => {},
      get startWorkAsked() { return held.get('asked') as boolean; },
      onStartWorkEnded: () => { held.set('asked', false); },
      onBack: () => {}, onClose: () => {}, onOpenIssue: () => {},
    },
  });
  mounted.push({ app, host });
  return { host, land: () => held.set('state', state(DETAIL)), asked: () => held.get('asked') as boolean };
}

describe('the narrow ticket', () => {
  for (const width of [360, 600]) {
    it(`shows status, assignee, priority and points in one row at ${width} wide`, async () => {
      const { host } = mountTicket(width);
      const strip = () => host.querySelector<HTMLElement>('[data-testid="ticket-narrow-row"]');
      await waitFor(() => strip() !== null, 'the narrow row');
      const fields = [...strip()!.querySelectorAll<HTMLElement>('[data-testid="ticket-field"]')];
      expect(fields.map((f) => f.dataset.field)).toEqual(['status', 'assignee', 'priority', 'points']);
      const rects = fields.map((f) => f.getBoundingClientRect());
      expect(new Set(rects.map((r) => Math.round(r.top))).size).toBe(1);
      for (const r of rects) expect(r.right).toBeLessThanOrEqual(width);
      expect(fields.map((f) => f.querySelector('[data-testid="ticket-field-value"]')?.textContent?.trim())).toEqual(['In Progress', 'User 1', 'High', '3']);
    });
  }
});

describe('the ticket\'s sprints', () => {
  it('names each sprint, and those no board of the ticket holds in words, never by ID', async () => {
    // Narrow: the Sprint field is in the Details rail it opens.
    const { host } = mountTicket(600);
    const details = () => host.querySelector<HTMLElement>('[data-testid="ticket-details"]');
    await waitFor(() => details() !== null, 'the Details control');
    details()!.click();
    const field = () => host.querySelector<HTMLElement>('[data-testid="ticket-rail"] [data-testid="ticket-field"][data-field="sprint"] [data-testid="ticket-field-value"]');
    await waitFor(() => !!field()?.textContent?.trim(), 'the Sprint field');
    expect(field()!.textContent?.trim()).toBe('Sprint 8, 2 sprints on other boards');
  });
});

describe('a Start work asked for', () => {
  function work(): void {
    noteProject('p1', HOME_BACKEND);
    addProjectLocal({ id: 'p1', name: 'alpha', path: '/repos/alpha', createdAt: 1, updatedAt: 1, sortPosition: 1, archived: false });
    setBindingMock('GetTicketWork', async () => ({ key: 'WEB-1', pullRequests: [], branches: [], problems: [] }));
    setBindingMock('ProjectTrackerSetup', async () => ({ revision: 'r1', setup: { status_groups: [], ticket_pins: [], start_work: [] } }));
  }
  const form = () => document.querySelector<HTMLElement>('[data-testid="start-work-form"]');

  for (const width of [390, 600, 900]) {
    for (const order of ['detail first', 'measured first'] as const) {
      it(`shows its form ${width} wide, the ticket's detail ${order === 'detail first' ? 'there when it mounts' : 'arriving after it is measured'}`, async () => {
        work();
        const ticket = mountTicket(width, { detail: order === 'detail first' ? DETAIL : null });
        if (order === 'measured first') {
          await waitFor(() => ticket.host.querySelector('[data-testid="ticket-screen"]') !== null, 'the ticket');
          expect(form()).toBeNull();
          ticket.land();
        }
        await waitFor(() => form() !== null || !ticket.asked(), 'the ask taken');
        await waitFor(() => form() !== null, 'the Start work form');
        expect(ticket.asked()).toBe(false);
        // Work is the one in the layout the ticket settled on: in the
        // Details rail over a narrow ticket, open for it.
        const rail = ticket.host.querySelector<HTMLElement>('[data-testid="ticket-rail"]');
        expect(rail?.querySelector('[data-testid="ticket-work"]')).not.toBeNull();
        expect(ticket.host.querySelector('[data-testid="ticket-narrow-row"]') !== null).toBe(width < 720);
      });
    }
  }

  it('ends when the person closes the Details it opened before the ticket was read', async () => {
    work();
    const ticket = mountTicket(390, { detail: null });
    const rail = () => ticket.host.querySelector<HTMLElement>('[data-testid="ticket-rail"]');
    await waitFor(() => rail() !== null, 'Details opened for the ask');
    ticket.host.querySelector<HTMLElement>('[data-testid="ticket-details-close"]')!.click();
    await waitFor(() => rail() === null, 'Details closed');
    expect(ticket.asked()).toBe(false);
    ticket.land();
    await waitFor(() => ticket.host.querySelector('[data-testid="ticket-details"]') !== null, 'the Details control');
    ticket.host.querySelector<HTMLElement>('[data-testid="ticket-details"]')!.click();
    await waitFor(() => rail()?.querySelector('[data-testid="ticket-work-start"]') != null, 'Work in Details');
    await waitFor(() => !(rail()!.querySelector<HTMLButtonElement>('[data-testid="ticket-work-start"]')!.disabled), 'Start work ready');
    expect(form()).toBeNull();
  });
});

describe('a description emptied in its editor', () => {
  it('is sent from the text it was edited from, and a stale refusal keeps the editor with its sentence', async () => {
    const sent: unknown[] = [];
    setBindingMock('EditTrackerIssue', async (connection: string, request: unknown) => {
      sent.push([connection, request]);
      return { parts: [{ issue: '1', step: 'edit', state: 'refused', refusal: { reason: 'stale', message: 'Description on WEB-1 changed since the write was planned; plan it again' } }] };
    });
    const { host } = mountTicket(900);
    const section = () => host.querySelector<HTMLElement>('[data-testid="ticket-section"][data-field="description"]');
    await waitFor(() => section()?.querySelector('[data-testid="ticket-section-edit"]') != null, 'the description\'s Edit');
    section()!.querySelector<HTMLElement>('[data-testid="ticket-section-edit"]')!.click();
    await waitFor(() => section()?.querySelector('[data-testid="ticket-section-editor"]') != null, 'the editor');
    const editor = section()!.querySelector<HTMLTextAreaElement>('[data-testid="ticket-section-editor"]')!;
    editor.value = '';
    editor.dispatchEvent(new Event('input', { bubbles: true }));
    section()!.querySelector<HTMLElement>('[data-testid="ticket-section-save"]')!.click();
    await waitFor(() => sent.length === 1, 'the clear sent');
    expect(sent[0]).toEqual(['c1', { issue: '1', fields: [{ field: 'description', markdown: { markdown: '', from: 'The description' } }], accept_loss: false }]);
    await waitFor(() => section()?.textContent?.includes('changed since the write was planned') === true, 'the stale refusal');
    expect(section()!.querySelector('[data-testid="ticket-section-editor"]')).not.toBeNull();
  });
});
