// Real-Chromium suite for the ticket's narrow layout: under 720 wide the
// ticket shows status, assignee, priority and points in one row over the
// content, at a phone's width as at a wider one.

import { afterEach, describe, expect, it } from 'vitest';
import { mount, unmount } from 'svelte';
import '../../../app.css';
import TicketScreen from './TicketScreen.svelte';
import type { DetailView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import type { TrackerTicketAttachment, TrackerTicketState } from '../../stores/trackerTicket.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';
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
} as unknown as DetailView;

function mountTicket(width: number) {
  const host = document.createElement('div');
  host.style.cssText = `position: fixed; top: 0; left: 0; width: ${width}px; height: 700px; display: flex;`;
  document.body.appendChild(host);
  const state: TrackerTicketState = {
    detail: DETAIL, readAt: 1, failure: null, ended: false, refreshing: false, refreshNotice: null, readError: null,
    earlier: { pages: [], next: null, loading: false, error: null }, poker: null,
  };
  const ref = { backend: HOME_BACKEND, connection: 'c1', issue: '1' };
  const attachment: TrackerTicketAttachment = { key: 'ticket', ref, current: state, error: null, setShown: () => {}, setPokerShown: () => {}, release: () => {} };
  const app = mount(TicketScreen, {
    target: host,
    props: {
      attachment, seed: { key: 'WEB-1', title: 'Issue 1', card: null }, board: '1', kind: 'jira', computerName: 'This computer', account: null,
      offline: false, compact: false, trail: 0, placement: 'over', besideFits: false,
      onPlacement: () => {}, onBack: () => {}, onClose: () => {}, onOpenIssue: () => {},
    },
  });
  mounted.push({ app, host });
  return host;
}

describe('the narrow ticket', () => {
  for (const width of [360, 600]) {
    it(`shows status, assignee, priority and points in one row at ${width} wide`, async () => {
      const host = mountTicket(width);
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
