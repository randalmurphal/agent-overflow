// Real-Chromium suite for the ticket's narrow layout: under 720 wide the
// ticket shows status, assignee, priority and points in one row over the
// content, at a phone's width as at a wider one; and a Start work asked for
// (a Board card's) shows its form in the layout the ticket settles on,
// narrow or wide, whether the ticket's detail is there when it mounts or
// arrives after. And a description emptied in its editor is sent as a
// clear from the text it was edited from, whose stale refusal keeps the
// editor open with the tracker's sentence. And the project's pins: a field
// pinned from its row and a section from its header on every open ticket
// of the project, a refused or failed pin said naming it and the project
// with what is stored shown, a pin this ticket lacks unpinned from behind
// the rail's control, no field pin on Linear, and none with no project.
// And Work on a narrow ticket reads the ticket's pull requests only when
// Details opens, not for the wide layout the screen has before it is
// measured.

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
import type { Setup, SetupView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import { getToasts } from '../../stores/toast.svelte';
import { TransportError } from '../../transport/wsClient';
import { setCarriedSessionScopes } from '../../transport/scopes';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';

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
  return mountWith(width, { detail: ask ? ask.detail : DETAIL, asked: ask !== undefined, project: ask ? 'p1' : null });
}

function mountWith(width: number, opts: { detail: DetailView | null; asked?: boolean; project: string | null; kind?: 'jira' | 'linear'; key?: string; left?: number }) {
  const host = document.createElement('div');
  host.style.cssText = `position: fixed; top: 0; left: ${opts.left ?? 0}px; width: ${width}px; height: 700px; display: flex; flex-direction: column;`;
  document.body.appendChild(host);
  const held = new SvelteMap<string, TrackerTicketState | boolean>([['state', state(opts.detail)], ['asked', opts.asked ?? false]]);
  const ref = { backend: HOME_BACKEND, connection: 'c1', issue: opts.detail?.card.id ?? '1' };
  const kind = opts.kind ?? 'jira';
  const attachment: TrackerTicketAttachment = {
    key: opts.key ?? 'ticket', ref, get current() { return held.get('state') as TrackerTicketState; }, error: null,
    tracker: { kind, site: kind === 'jira' ? SITE : 'example', account: null, confirmed: true }, setShown: () => {}, setPokerShown: () => {}, release: () => {},
  };
  const app = mount(TicketScreen, {
    target: host,
    props: {
      attachment, seed: { key: 'WEB-1', title: 'Issue 1', card: null }, board: '1', computerName: 'This computer',
      project: opts.project, onProject: () => {}, offline: false, compact: false, shown: true, trail: 0,
      focusAsked: false, onFocusTaken: () => {},
      get startWorkAsked() { return held.get('asked') as boolean; },
      onStartWorkEnded: () => { held.set('asked', false); },
      onBack: () => {}, onClose: () => {}, onOpenIssue: () => {},
    },
  });
  mounted.push({ app, host });
  return { host, land: () => held.set('state', state(DETAIL)), asked: () => held.get('asked') as boolean };
}

const SITE = 'https://example.atlassian.net';

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

describe('Work on a narrow ticket', () => {
  it('reads nothing while Details is closed, and once when it opens', async () => {
    let stored = 0;
    let searched = 0;
    setBindingMock('GetTicketWork', async () => { stored++; return { key: 'WEB-1', pullRequests: [], branches: [], problems: [] }; });
    setBindingMock('SearchTicketWork', async () => { searched++; return { key: 'WEB-1', pullRequests: [], branches: [], problems: [] }; });
    const ticket = mountWith(360, { detail: DETAIL, project: null, key: 'narrow-work' });
    await waitFor(() => ticket.host.querySelector('[data-testid="ticket-details"]') !== null, 'the narrow ticket');
    expect([stored, searched]).toEqual([0, 0]);
    ticket.host.querySelector<HTMLElement>('[data-testid="ticket-details"]')!.click();
    await waitFor(() => ticket.host.querySelector('[data-testid="ticket-rail"] [data-testid="ticket-work"]') !== null, 'Work in Details');
    await waitFor(() => searched > 0, 'the search');
    expect([stored, searched]).toEqual([1, 1]);
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

describe('the project\'s pins', () => {
  const FIELDS = [
    { id: 'customfield_1', name: 'Tier', kind: 'option', value: { kind: 'option', choices: [{ id: 'g', name: 'Gold' }] } },
    { id: 'customfield_2', name: 'Story points', kind: 'number', value: {} },
    { id: 'customfield_3', name: 'Acceptance', kind: 'rich_text', value: { kind: 'rich_text', texts: ['It works'] } },
  ];
  const ticket = (id: string, key: string): DetailView => ({ ...DETAIL, card: { ...DETAIL.card, id, key }, fields: FIELDS } as unknown as DetailView);
  let stored: SetupView;
  let saves: Setup[];

  function project(): void {
    noteProject('p1', HOME_BACKEND);
    addProjectLocal({ id: 'p1', name: 'alpha', path: '/repos/alpha', createdAt: 1, updatedAt: 1, sortPosition: 1, archived: false });
    setBindingMock('GetTicketWork', async () => ({ key: 'WEB-1', pullRequests: [], branches: [], problems: [] }));
    stored = { revision: '', setup: { status_groups: [], ticket_pins: [], start_work: [] } } as SetupView;
    saves = [];
    setBindingMock('ProjectTrackerSetup', async () => stored);
    setBindingMock('SetProjectTrackerSetup', async (_p: string, revision: string, setup: Setup) => {
      saves.push(setup);
      if (revision !== stored.revision) return { saved: false, refused: "this change was not saved: the project's tracker setup changed after it was read; it is shown as it is now", stored };
      stored = { revision: `r${saves.length}`, setup } as SetupView;
      return { saved: true, stored };
    });
  }

  const q = (host: HTMLElement, sel: string) => host.querySelector<HTMLElement>(sel);
  // The rail's rows above its "more fields" control: the card's and the
  // pinned ones.
  const railIds = (host: HTMLElement) => {
    const out: string[] = [];
    for (const el of host.querySelector('[data-testid="ticket-fields"]')?.children ?? []) {
      if ((el as HTMLElement).dataset.testid === 'ticket-more-fields') break;
      if ((el as HTMLElement).dataset.testid === 'ticket-field') out.push((el as HTMLElement).dataset.field!);
    }
    return out;
  };
  const shownSections = (host: HTMLElement) => [...host.querySelectorAll<HTMLElement>('[data-testid="ticket-content"] [data-testid="ticket-section"], [data-testid="ticket-content"] [data-testid="ticket-links"], [data-testid="ticket-content"] [data-testid="ticket-attachments"]')]
    .map((el) => el.dataset.field ?? el.dataset.testid);
  const fieldRow = (host: HTMLElement, id: string) => q(host, `[data-testid="ticket-rail"] [data-testid="ticket-field"][data-field="${id}"]`);

  it('shows the ticket as it was while nothing is pinned, with a pin on each row and section', async () => {
    project();
    const { host } = mountWith(900, { detail: ticket('1', 'WEB-1'), project: 'p1' });
    await waitFor(() => q(host, '[data-testid="ticket-section-pin"]') !== null, 'the pins offered');
    expect(railIds(host)).toEqual(['type', 'status', 'assignee', 'priority', 'points', 'sprint']);
    expect(q(host, '[data-testid="ticket-more-fields"]')!.textContent?.trim()).toBe('2 more fields, 1 filled');
    expect(shownSections(host)).toEqual(['description', 'ticket-links', 'ticket-attachments']);
    expect(q(host, '[data-testid="ticket-more-sections"]')!.textContent?.trim()).toBe('1 more section: Acceptance');
    expect(q(host, '[data-testid="ticket-field"][data-field="status"] [data-testid="ticket-field-pin"]'), 'the card\'s rows take no pin').toBeNull();
  });

  it('pins a field from its row and a section from its header, on every open ticket of the project', async () => {
    project();
    const one = mountWith(900, { detail: ticket('1', 'WEB-1'), project: 'p1', key: 't1' });
    const two = mountWith(900, { detail: ticket('2', 'WEB-2'), project: 'p1', key: 't2', left: 900 });
    await waitFor(() => q(one.host, '[data-testid="ticket-section-pin"]') !== null && q(two.host, '[data-testid="ticket-section-pin"]') !== null, 'the setup read');
    q(one.host, '[data-testid="ticket-more-fields"]')!.click();
    await waitFor(() => fieldRow(one.host, 'customfield_2') !== null, 'the field behind the control');
    q(fieldRow(one.host, 'customfield_2')!, '[data-testid="ticket-field-pin"]')!.click();
    await waitFor(() => railIds(two.host).includes('customfield_2'), 'the pin on the other ticket');
    expect(saves[0].ticket_pins).toEqual([{ tracker: 'jira', site: SITE, fields: ['customfield_2'] }]);
    expect(railIds(one.host).slice(-1)).toEqual(['customfield_2']);
    expect(q(two.host, '[data-testid="ticket-more-fields"]')!.textContent?.trim()).toBe('1 more field, 1 filled');

    q(one.host, '[data-testid="ticket-more-sections"]')!.click();
    await waitFor(() => q(one.host, '[data-testid="ticket-section"][data-field="customfield_3"] [data-testid="ticket-section-pin"]') !== null, 'the folded section');
    q(one.host, '[data-testid="ticket-section"][data-field="customfield_3"] [data-testid="ticket-section-pin"]')!.click();
    await waitFor(() => q(two.host, '[data-testid="ticket-more-sections"]') === null, 'nothing left folded on the other ticket');
    expect(shownSections(two.host)).toEqual(['description', 'ticket-links', 'ticket-attachments', 'customfield_3']);

    q(two.host, '[data-testid="ticket-section"][data-field="description"] [data-testid="ticket-section-pin"]')!.click();
    await waitFor(() => q(one.host, '[data-testid="ticket-more-sections"]')?.textContent?.trim() === '1 more section: Description', 'the description folded');
    expect(saves.at(-1)!.ticket_pins).toEqual([{ tracker: 'jira', site: SITE, fields: ['customfield_2'], sections: ['links', 'attachments', 'customfield_3'] }]);
  });

  it('says a pin that lost to another save was not made, naming it and the project, and shows what is stored on every ticket of the project', async () => {
    project();
    const { host } = mountWith(900, { detail: ticket('1', 'WEB-1'), project: 'p1' });
    const other = mountWith(900, { detail: ticket('2', 'WEB-2'), project: 'p1', key: 't2', left: 900 });
    await waitFor(() => q(host, '[data-testid="ticket-section-pin"]') !== null && q(other.host, '[data-testid="ticket-section-pin"]') !== null, 'the setup read');
    // Another window pinned Tier since, and this window has not heard it.
    stored = { revision: 'other', setup: { status_groups: [], ticket_pins: [{ tracker: 'jira', site: SITE, fields: ['customfield_1'] }], start_work: [] } } as SetupView;
    q(host, '[data-testid="ticket-more-fields"]')!.click();
    await waitFor(() => fieldRow(host, 'customfield_2') !== null, 'the field behind the control');
    q(fieldRow(host, 'customfield_2')!, '[data-testid="ticket-field-pin"]')!.click();
    await waitFor(() => getToasts().length > 0, 'the toast');
    expect(getToasts()[0].message).toBe("Story points for alpha was not pinned. This change was not saved: the project's tracker setup changed after it was read; it is shown as it is now.");
    await waitFor(() => railIds(host).includes('customfield_1') && railIds(other.host).includes('customfield_1'), 'the stored pin on both tickets');
    expect(railIds(host)).not.toContain('customfield_2');
    expect(railIds(other.host)).not.toContain('customfield_2');
  });

  it('says a pin whose save failed in the computer\'s words, and shows every ticket of the project what is stored after it', async () => {
    project();
    const { host } = mountWith(900, { detail: ticket('1', 'WEB-1'), project: 'p1' });
    const other = mountWith(900, { detail: ticket('2', 'WEB-2'), project: 'p1', key: 't2', left: 900 });
    await waitFor(() => q(host, '[data-testid="ticket-section-pin"]') !== null && q(other.host, '[data-testid="ticket-section-pin"]') !== null, 'the pins');
    // The write may have happened: what the file holds after it is read again.
    stored = { revision: 'r9', setup: { status_groups: [], ticket_pins: [{ tracker: 'jira', site: SITE, sections: ['description', 'attachments'] }], start_work: [] } } as SetupView;
    const sentence = "this change may or may not be stored: the computer that holds the project could not finish updating /x/tracker.json (sync failed); read the project's tracker setup again before you change it again";
    setBindingMock('SetProjectTrackerSetup', async () => { throw new TransportError('tracker_setup_unsure', sentence, { detail: { ref: 'r', method: 'SetProjectTrackerSetup', at: 1, chain: [sentence] } }); });
    q(host, '[data-testid="ticket-links-pin"]')!.click();
    await waitFor(() => getToasts().length > 0, 'the toast');
    expect(getToasts()[0].message).toBe(`Could not unpin Linked tickets for alpha: T${sentence.slice(1)}.`);
    await waitFor(() => !shownSections(host).includes('ticket-links') && !shownSections(other.host).includes('ticket-links'), 'what is stored on both tickets');
    expect(shownSections(other.host)).toEqual(['description', 'ticket-attachments']);
  });

  it('lists a pinned field this ticket lacks behind the control, so it can be unpinned', async () => {
    project();
    stored = { revision: 'r0', setup: { status_groups: [], ticket_pins: [{ tracker: 'jira', site: SITE, fields: ['customfield_9', 'customfield_1'] }], start_work: [] } } as SetupView;
    const { host } = mountWith(900, { detail: ticket('1', 'WEB-1'), project: 'p1' });
    await waitFor(() => railIds(host).includes('customfield_1'), 'the pinned field');
    q(host, '[data-testid="ticket-more-fields"]')!.click();
    await waitFor(() => q(host, '[data-testid="ticket-pin-missing"]') !== null, 'the missing pin');
    expect(q(host, '[data-testid="ticket-pin-missing"]')!.textContent).toContain('customfield_9');
    q(host, '[data-testid="ticket-pin-missing-unpin"]')!.click();
    await waitFor(() => q(host, '[data-testid="ticket-pin-missing"]') === null, 'the pin removed');
    expect(saves[0].ticket_pins).toEqual([{ tracker: 'jira', site: SITE, fields: ['customfield_1'] }]);
  });

  it('reaches an unpinned section from the menu: Description… opens its editor in the folded line', async () => {
    project();
    stored = { revision: 'r0', setup: { status_groups: [], ticket_pins: [{ tracker: 'jira', site: SITE, sections: ['links', 'attachments'] }], start_work: [] } } as SetupView;
    const { host } = mountWith(900, { detail: ticket('1', 'WEB-1'), project: 'p1' });
    await waitFor(() => q(host, '[data-testid="ticket-more-sections"]')?.textContent?.trim() === '2 more sections: Description, Acceptance', 'the folded description');
    q(host, '[data-testid="ticket-menu-button"]')!.click();
    await waitFor(() => document.querySelector('[data-testid="ticket-menu-description"]') !== null, 'the menu');
    document.querySelector<HTMLElement>('[data-testid="ticket-menu-description"]')!.click();
    await waitFor(() => q(host, '[data-testid="ticket-section"][data-field="description"] [data-testid="ticket-section-editor"]') !== null, 'the description editor');
  });

  it('offers a device that may not operate the project\'s trackers no pin press, and says why', async () => {
    project();
    stageBackend();
    try {
      noteProject('p1', 'laptop');
      setCarriedSessionScopes('laptop', ['tracker:read']);
      const { host } = mountWith(900, { detail: ticket('1', 'WEB-1'), project: 'p1' });
      await waitFor(() => q(host, '[data-testid="ticket-section-pin"]') !== null, 'the pins');
      const pin = q(host, '[data-testid="ticket-section-pin"]') as HTMLButtonElement;
      expect(pin.disabled).toBe(true);
      expect(pin.title).toBe('This device may not change the pins of alpha.');
    } finally {
      resetStagedBackends();
    }
  });

  it('offers a Linear ticket\'s sections a pin and its fixed fields none', async () => {
    project();
    const linear = { ...ticket('1', 'ENG-1'), fields: [{ id: 'due_date', name: 'Due date', kind: 'date', value: {} }] } as unknown as DetailView;
    const { host } = mountWith(900, { detail: linear, project: 'p1', kind: 'linear' });
    await waitFor(() => q(host, '[data-testid="ticket-section-pin"]') !== null, 'the section pins');
    q(host, '[data-testid="ticket-more-fields"]')!.click();
    await waitFor(() => fieldRow(host, 'due_date') !== null, 'the field behind the control');
    expect(q(host, '[data-testid="ticket-field-pin"]')).toBeNull();
  });

  it('offers no pin to a ticket with no project, and says pins that cannot be read', async () => {
    project();
    const none = mountWith(900, { detail: ticket('1', 'WEB-1'), project: null });
    await waitFor(() => q(none.host, '[data-testid="ticket-more-sections"]') !== null, 'the ticket');
    expect(q(none.host, '[data-testid="ticket-section-pin"]')).toBeNull();

    setBindingMock('ProjectTrackerSetup', async () => { throw new TransportError('tracker_setup_unreadable', "this project's tracker setup cannot be read from /x/tracker.json on the computer that holds the project (bad JSON); fix the file or remove it, and it is read again at its next use"); });
    const broken = mountWith(900, { detail: ticket('2', 'WEB-2'), project: 'p1', key: 't2', left: 900 });
    await waitFor(() => q(broken.host, '[data-testid="ticket-pins-error"]') !== null, 'the error');
    expect(q(broken.host, '[data-testid="ticket-pins-error"]')!.textContent).toContain('/x/tracker.json');
    expect(q(broken.host, '[data-testid="ticket-section-pin"]')).toBeNull();
    expect(shownSections(broken.host)).toEqual(['description', 'ticket-links', 'ticket-attachments']);
  });
});
