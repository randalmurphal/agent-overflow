// A dashboard's ticket list with rows selected: the bar counts them and a
// count again keeps the rows still listed; Move to moves each ticket not
// there yet through the ticket write store, says under a row what the
// tracker refused (in a toast while the list is not shown), and sums the
// batch up for a few seconds; Assign assigns each; Start a thread makes
// one thread on the tickets with the text; and each action says why it is
// off.

import { fireEvent, render, within } from '@testing-library/svelte';
import { createRawSnippet, tick } from 'svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { MoveView, WriteView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import type { TicketRow } from '../../../../bindings/agent-overflow/internal/trackerdash/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { TOAST_MS } from '../../pageTiming';
import { getAllPanes } from '../../stores/panes.svelte';
import { getToasts } from '../../stores/toast.svelte';
import type { Thread } from '../../types/models';
import TicketListActions, { type TicketListSource } from './TicketListActions.svelte';
import { ticketWriteSurfaceContext } from './ticketWriteSurface';

const SOURCE: TicketListSource = { backend: '', connection: 'c1', project: 'p1', writeBlocked: '', threadBlocked: '' };

function rowOf(n: number, status = 'To Do'): TicketRow {
  return { row: n, id: `${10100 + n}`, key: `WEB-${n}`, title: `Ticket ${n}`, status, counted: true } as TicketRow;
}

const rowSnippet = createRawSnippet((r: () => TicketRow) => ({ render: () => `<span>${r().key}</span>` }));

function draw(rows: TicketRow[], over: Partial<TicketListSource> = {}, shown = true) {
  const onTicket = vi.fn();
  const view = render(TicketListActions, {
    props: { rows, source: { ...SOURCE, ...over }, variant: 'widget', listTestId: 'list', rowTestId: 'list-row', onTicket, row: rowSnippet },
    context: ticketWriteSurfaceContext(() => shown),
  });
  return { ...view, onTicket };
}

const page = () => within(document.body);

async function flush(): Promise<void> {
  for (let i = 0; i < 10; i += 1) await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
  for (let i = 0; i < 10; i += 1) await tick();
}

/** Lets the calls and renders a press started finish, on fake timers. */
async function settle(): Promise<void> {
  for (let i = 0; i < 20; i += 1) {
    await vi.advanceTimersByTimeAsync(0);
    await tick();
  }
}

async function select(...keys: string[]): Promise<void> {
  for (const key of keys) await fireEvent.click(page().getByRole('checkbox', { name: `Select ${key}` }));
}

function applied(): WriteView {
  return { parts: [{ step: 'move', state: 'applied' }] } as WriteView;
}

function refused(message: string): WriteView {
  return { parts: [{ step: 'move', state: 'refused', refusal: { reason: 'tracker', message } }] } as WriteView;
}

const toDone = (id: string, fields = false): MoveView => ({ id, name: 'Finish', to: { id: 's3', name: 'Done' }, ...(fields ? { fields: [{ id: 'resolution' }] } : {}) } as MoveView);

afterEach(() => {
  vi.useRealTimers();
});

describe('selecting rows', () => {
  it('counts the selection, keeps the rows still listed when they are counted again, and clears', async () => {
    const view = draw([rowOf(1), rowOf(2), rowOf(3)]);
    expect(view.queryByTestId('ticket-list-bar')).toBeNull();
    expect(view.getByTestId('list').dataset.selecting).toBe('false');
    await select('WEB-1', 'WEB-3');
    expect(view.getByTestId('ticket-list-count').textContent).toBe('2 selected');
    expect(view.getByTestId('list').dataset.selecting).toBe('true');

    await view.rerender({ rows: [rowOf(3), rowOf(2), rowOf(4)] });
    expect(view.getByTestId('ticket-list-count').textContent).toBe('1 selected');
    expect(page().getByRole('checkbox', { name: 'Select WEB-3' }).getAttribute('aria-checked')).toBe('true');
    // A ticket that left the list and came back is not selected again.
    await view.rerender({ rows: [rowOf(1), rowOf(3)] });
    expect(page().getByRole('checkbox', { name: 'Select WEB-1' }).getAttribute('aria-checked')).toBe('false');

    await fireEvent.click(view.getByTestId('ticket-list-clear'));
    expect(view.queryByTestId('ticket-list-bar')).toBeNull();
  });

  it('opens a row\'s ticket from the row, not from its box', async () => {
    const view = draw([rowOf(1)]);
    await select('WEB-1');
    expect(view.onTicket).not.toHaveBeenCalled();
    await fireEvent.click(view.getByTestId('list-row'));
    expect(view.onTicket).toHaveBeenCalledWith(rowOf(1));
  });
});

describe('Move to', () => {
  it('moves each ticket not there yet, leaves the one there, says under a row what was refused, and sums up for a few seconds', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    setBindingMock('TrackerMoves', async (_c: string, issue: string) => (issue === '10103' ? [] : [toDone(`m${issue}`, true), toDone(`n${issue}`)]));
    const sent: string[] = [];
    setBindingMock('MoveTrackerIssue', async (_c: string, input: { issue: string; move: string }) => {
      sent.push(`${input.issue} ${input.move}`);
      return input.issue === '10102' ? refused('WEB-2 needs a reviewer first') : applied();
    });
    const view = draw([rowOf(1), rowOf(2), rowOf(3, 'Done')]);
    await select('WEB-1', 'WEB-2', 'WEB-3');
    await fireEvent.click(view.getByTestId('ticket-list-move'));
    expect(page().getByTestId('ticket-list-move-title').textContent).toBe('Move 3 tickets to');
    await settle();
    expect(page().getAllByTestId('ticket-list-move-status')).toHaveLength(1);
    await fireEvent.click(page().getByTestId('ticket-list-move-status'));
    await settle();
    expect(view.getByTestId('ticket-list-note').dataset.done).toBe('true');

    expect(sent.sort()).toEqual(['10101 n10101', '10102 n10102']);
    expect(view.getByTestId('ticket-list-note').textContent?.trim()).toBe('1 of 2 tickets moved to Done, 1 already in Done');
    expect(view.queryByTestId('ticket-list-bar')).toBeNull();
    const notes = view.getAllByTestId('ticket-write-outcome');
    expect(notes.map((n) => n.textContent?.trim())).toEqual(['WEB-2 needs a reviewer first']);
    expect(getToasts()).toHaveLength(0);

    await vi.advanceTimersByTimeAsync(TOAST_MS - 1);
    expect(view.getByTestId('ticket-list-note')).toBeTruthy();
    await vi.advanceTimersByTimeAsync(1);
    await tick();
    expect(view.queryByTestId('ticket-list-note')).toBeNull();
  });

  it('says in a toast what the tracker refused while the list is not shown', async () => {
    setBindingMock('TrackerMoves', async (_c: string, issue: string) => [toDone(`n${issue}`)]);
    setBindingMock('MoveTrackerIssue', async (_c: string, input: { issue: string }) => (input.issue === '10102' ? refused('WEB-2 needs a reviewer first') : applied()));
    const view = draw([rowOf(1), rowOf(2)], {}, false);
    await select('WEB-1', 'WEB-2');
    await fireEvent.click(view.getByTestId('ticket-list-move'));
    await vi.waitFor(() => expect(page().getAllByTestId('ticket-list-move-status')).toHaveLength(1));
    await fireEvent.click(page().getByTestId('ticket-list-move-status'));
    await vi.waitFor(() => expect(view.getByTestId('ticket-list-note').dataset.done).toBe('true'));
    await flush();
    expect(view.queryAllByTestId('ticket-write-outcome')).toHaveLength(0);
    expect(getToasts().map((t) => t.message)).toEqual(['The tracker refused the change to WEB-2: WEB-2 needs a reviewer first']);
  });

  it('offers a status a move reaches only with fields, off and naming the ticket', async () => {
    setBindingMock('TrackerMoves', async (_c: string, issue: string) => [toDone(`n${issue}`, issue === '10102')]);
    const view = draw([rowOf(1), rowOf(2)]);
    await select('WEB-1', 'WEB-2');
    await fireEvent.click(view.getByTestId('ticket-list-move'));
    await vi.waitFor(() => expect(page().getAllByTestId('ticket-list-move-status')).toHaveLength(1));
    const status = page().getByTestId('ticket-list-move-status');
    expect(status.textContent).toContain('Moving WEB-2 to Done asks for fields; move it from the ticket');
    expect(status.getAttribute('aria-disabled') ?? String((status as HTMLButtonElement).disabled)).toBe('true');
  });

  it('says which ticket\'s moves could not be read', async () => {
    setBindingMock('TrackerMoves', async (_c: string, issue: string) => {
      if (issue === '10102') throw new Error('the tracker did not answer');
      return [toDone('n')];
    });
    const view = draw([rowOf(1), rowOf(2)]);
    await select('WEB-1', 'WEB-2');
    await fireEvent.click(view.getByTestId('ticket-list-move'));
    await vi.waitFor(() => expect(page().getByTestId('ticket-list-move-failed').textContent).toContain('Could not read where WEB-2 can move: The tracker did not answer.'));
  });

  it('is off, with Assign, when this device may not write to the tracker', async () => {
    const view = draw([rowOf(1)], { writeBlocked: 'This device may not change tickets on this computer.' });
    await select('WEB-1');
    for (const id of ['ticket-list-move', 'ticket-list-assign']) {
      const button = view.getByTestId(id) as HTMLButtonElement;
      expect(button.disabled).toBe(true);
      expect(button.title).toBe('This device may not change tickets on this computer.');
    }
  });
});

describe('Assign', () => {
  it('assigns each selected ticket to the person picked', async () => {
    setBindingMock('TrackerAssignableUsers', async () => ({ users: [{ id: 'u1', name: 'User 1' }] }));
    const edits: string[] = [];
    setBindingMock('EditTrackerIssue', async (_c: string, input: { issue: string; fields: unknown[] }) => { edits.push(`${input.issue} ${JSON.stringify(input.fields)}`); return applied(); });
    const view = draw([rowOf(1), rowOf(2)]);
    await select('WEB-1', 'WEB-2');
    await fireEvent.click(view.getByTestId('ticket-list-assign'));
    await vi.waitFor(() => expect(page().getByTestId('ticket-picker-item').textContent).toContain('User 1'));
    await fireEvent.click(page().getByTestId('ticket-picker-item'));
    await vi.waitFor(() => expect(view.getByTestId('ticket-list-note').textContent?.trim()).toBe('2 tickets assigned to User 1'));
    expect(edits.sort()).toEqual([
      '10101 [{"field":"assignee","value":{"kind":"user","choices":[{"id":"u1"}]}}]',
      '10102 [{"field":"assignee","value":{"kind":"user","choices":[{"id":"u1"}]}}]',
    ]);
  });
});

describe('Start a thread', () => {
  it('makes one thread on the selected tickets with the text, opens it and says so', async () => {
    const start = setBindingMock('StartTicketsThread', async () => ({ id: 't-many', projectId: 'p1', title: 'WEB-1 and WEB-2', provider: 'claude', model: '', createdAt: 1, updatedAt: 1, archived: false } as unknown as Thread));
    setBindingMock('SwitchThread', async (id: string) => ({ id, projectId: 'p1' } as unknown as Thread));
    const view = draw([rowOf(1), rowOf(2)]);
    await select('WEB-1', 'WEB-2');
    await fireEvent.click(view.getByTestId('ticket-list-thread'));
    await fireEvent.input(page().getByTestId('ticket-list-thread-text'), { target: { value: ' why are these stuck ' } });
    await fireEvent.click(page().getByTestId('ticket-list-thread-start'));
    await vi.waitFor(() => expect(view.getByTestId('ticket-list-note').textContent?.trim()).toBe('Thread started with 2 tickets attached'));
    expect(start).toHaveBeenCalledWith('p1', expect.objectContaining({ connectionId: 'c1', issueIds: ['10101', '10102'], text: 'why are these stuck' }));
    expect([...getAllPanes().values()].some((p) => p.threadId === 't-many')).toBe(true);
  });

  it('is off past the tickets one thread carries, and when this device may not start threads', async () => {
    const rows = Array.from({ length: 9 }, (_, i) => rowOf(i + 1));
    const view = draw(rows);
    await select(...rows.map((r) => r.key));
    const button = view.getByTestId('ticket-list-thread') as HTMLButtonElement;
    expect(button.disabled).toBe(true);
    expect(button.title).toBe('A thread takes at most 8 tickets, one file each; 9 are selected.');
    view.unmount();

    const blocked = draw([rowOf(1)], { threadBlocked: 'Pick the project its thread goes in.' });
    await select('WEB-1');
    expect((blocked.getByTestId('ticket-list-thread') as HTMLButtonElement).title).toBe('Pick the project its thread goes in.');
  });
});
