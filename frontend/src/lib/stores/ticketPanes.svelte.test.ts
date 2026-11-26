import { beforeEach, describe, expect, it, vi } from 'vitest';
import { createTicketTrail, ticketPaneTrailState, type OpenTicket } from './issuesPane.svelte';
import { setCompactLayoutForTest } from './layoutMode.svelte';
import { getPaneLayoutItems, setPaneLayoutItemsForTest } from './paneLayout.svelte';
import { focusPane, getFocusedPaneId } from './panes.svelte';
import {
  closeTicketPane,
  onTicketPanesChanged,
  openTicketPane,
  restoreTicketPane,
  setTicketPaneTicket,
  takeTicketPaneHandoff,
  ticketPaneCarrier,
  ticketPaneRecord,
  ticketPaneRefs,
} from './ticketPanes.svelte';
import { HOME_BACKEND } from '../transport/backendKey';

function ticket(issue: string): OpenTicket {
  return { ref: { backend: HOME_BACKEND, connection: 'c1', issue }, seed: { key: `WEB-${issue}`, title: `Issue ${issue}`, card: null }, project: null };
}

const OPTIONS = { board: '1' };

describe('ticket panes', () => {
  beforeEach(() => {
    setCompactLayoutForTest(false);
    setPaneLayoutItemsForTest([
      { id: 'tool-issues', paneId: 'tool-issues', kind: 'issues', widthPx: 600 },
      { id: 'thread-1', paneId: 'thread-1', kind: 'thread', widthPx: 600 },
    ]);
    focusPane('tool-issues');
  });

  it('opens a ticket in a pane right of the focused one, focused and asking the focus for its ticket', () => {
    const open = ticket('1');
    const paneId = openTicketPane(open, OPTIONS);
    expect(paneId).toMatch(/^ticket-/);
    expect(getPaneLayoutItems().map((item) => item.kind)).toEqual(['issues', 'ticket', 'thread']);
    expect(getFocusedPaneId()).toBe(paneId);
    const record = ticketPaneRecord(paneId)!;
    expect(record.ticket).toBe(open);
    expect(record.trail).toEqual([]);
    expect(record.board).toBe('1');
    expect(record.focus.askedFor(open)).toBe(true);
    record.focus.end();
  });

  it('focuses the pane already showing a ticket instead of opening a second, and lets go of what the open brought at once', () => {
    const first = openTicketPane(ticket('1'), OPTIONS);
    ticketPaneRecord(first)!.focus.end();
    focusPane('tool-issues');
    const handoff = vi.fn();
    expect(openTicketPane(ticket('1'), { ...OPTIONS, handoff })).toBe(first);
    expect(getPaneLayoutItems().filter((item) => item.kind === 'ticket')).toHaveLength(1);
    expect(getFocusedPaneId()).toBe(first);
    expect(handoff).toHaveBeenCalledTimes(1);
    const record = ticketPaneRecord(first)!;
    expect(record.focus.askedFor(record.ticket)).toBe(true);
    record.focus.end();
  });

  it('is not offered under the compact layout, and refuses an open there', () => {
    setCompactLayoutForTest(true);
    expect(() => openTicketPane(ticket('1'), OPTIONS)).toThrow(/compact/);
    expect(getPaneLayoutItems().some((item) => item.kind === 'ticket')).toBe(false);
    setCompactLayoutForTest(false);
  });

  it('drops the record, ends the focus ask and lets go of the move\'s hold with the pane, however its item leaves', () => {
    const changes = vi.fn();
    const stop = onTicketPanesChanged(changes);
    const handoffA = vi.fn();
    const a = openTicketPane(ticket('1'), { ...OPTIONS, handoff: handoffA });
    const b = openTicketPane(ticket('2'), OPTIONS);
    const askA = ticketPaneRecord(a)!;
    expect(ticketPaneRefs().map((ref) => ref.issue)).toEqual(['1', '2']);

    closeTicketPane(a);
    expect(ticketPaneRecord(a)).toBeNull();
    expect(handoffA).toHaveBeenCalledTimes(1);
    expect(askA.focus.askedFor(askA.ticket)).toBe(false);
    expect(ticketPaneRefs().map((ref) => ref.issue)).toEqual(['2']);

    // A layout that no longer holds the item (a reset, a restore, a thread
    // taking its slot) drops it the same way.
    setPaneLayoutItemsForTest([]);
    expect(ticketPaneRecord(b)).toBeNull();
    expect(ticketPaneRefs()).toEqual([]);
    expect(changes.mock.calls.length).toBeGreaterThanOrEqual(4);
    stop();
  });

  it('hands what held the ticket to the first taker only', () => {
    const handoff = vi.fn();
    const paneId = openTicketPane(ticket('1'), { ...OPTIONS, handoff });
    expect(takeTicketPaneHandoff(paneId)).toBe(handoff);
    expect(takeTicketPaneHandoff(paneId)).toBeNull();
    closeTicketPane(paneId);
    expect(handoff).not.toHaveBeenCalled();
  });

  it('names every ticket a pane shows or has on its Back trail, and saves only the ticket shown', () => {
    const paneId = openTicketPane(ticket('1'), OPTIONS);
    setTicketPaneTicket(paneId, ticket('2'), [ticket('1')]);
    expect(ticketPaneRefs().map((ref) => ref.issue)).toEqual(['2', '1']);
    expect(ticketPaneCarrier(paneId)).toEqual({ backend: HOME_BACKEND, connection: 'c1', issue: '2' });
    expect(() => setTicketPaneTicket('tool-issues', ticket('3'), [])).toThrow(/not a ticket pane/);
  });

  it('closes the pane when its trail closes, and refuses to close what is not a ticket pane', () => {
    const paneId = openTicketPane(ticket('1'), OPTIONS);
    const trail = createTicketTrail({ state: ticketPaneTrailState(paneId), shown: () => true });
    trail.close();
    expect(ticketPaneRecord(paneId)).toBeNull();
    expect(getPaneLayoutItems().some((item) => item.paneId === paneId)).toBe(false);
    trail.destroy();
    expect(() => closeTicketPane('tool-issues')).toThrow(/not a ticket pane/);
    expect(getPaneLayoutItems().some((item) => item.paneId === 'tool-issues')).toBe(true);
  });

  it('restores a pane on its saved ticket with no trail, asking no focus', () => {
    restoreTicketPane('ticket-a', { backend: HOME_BACKEND, connection: 'c1', issue: '7' });
    const record = ticketPaneRecord('ticket-a')!;
    // Its key and title come with its subscription's first answer; no
    // project is saved, so Work asks the person for one.
    expect(record.ticket).toEqual({ ref: { backend: HOME_BACKEND, connection: 'c1', issue: '7' }, seed: { key: '', title: '', card: null }, project: null });
    expect(record.board).toBeNull();
    expect(record.focus.askedFor(record.ticket)).toBe(false);
  });
});
