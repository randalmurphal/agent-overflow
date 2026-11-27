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
import { BULK_AT_ONCE } from './ticketSelection';
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

  it('closes an open action when a count again takes one of its tickets out, so a pick never writes it', async () => {
    setBindingMock('TrackerAssignableUsers', async () => ({ users: [{ id: 'u1', name: 'User 1' }] }));
    const edits: string[] = [];
    setBindingMock('EditTrackerIssue', async (_c: string, input: { issue: string }) => { edits.push(input.issue); return applied(); });
    setBindingMock('TrackerMoves', async () => [toDone('n')]);
    const view = draw([rowOf(1), rowOf(2)]);
    await select('WEB-1', 'WEB-2');
    for (const action of ['ticket-list-assign', 'ticket-list-move', 'ticket-list-thread']) {
      await fireEvent.click(view.getByTestId(action));
      await flush();
      expect(document.body.querySelector('[role="dialog"], [role="menu"], form')).not.toBeNull();
      await view.rerender({ rows: [rowOf(1)] });
      await flush();
      expect(document.body.querySelector('[data-testid="ticket-picker-find"], [data-testid="ticket-list-move-title"], [data-testid="ticket-list-thread-form"]')).toBeNull();
      await view.rerender({ rows: [rowOf(1), rowOf(2)] });
      await select('WEB-2');
    }
    await fireEvent.click(view.getByTestId('ticket-list-assign'));
    await vi.waitFor(() => expect(page().getByTestId('ticket-picker-item')).toBeTruthy());
    expect(page().getByLabelText('Find assign 2 tickets to')).toBeTruthy();
    await view.rerender({ rows: [rowOf(1)] });
    await flush();
    await fireEvent.click(view.getByTestId('ticket-list-assign'));
    await vi.waitFor(() => expect(page().getByTestId('ticket-picker-item')).toBeTruthy());
    await fireEvent.click(page().getByTestId('ticket-picker-item'));
    await vi.waitFor(() => expect(view.getByTestId('ticket-list-note').textContent?.trim()).toBe('1 ticket assigned to User 1'));
    expect(edits).toEqual(['10101']);
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

  it('moves tickets back to where its rows still show them, once this list has moved them away', async () => {
    // Jira lists a move into Done from Done when every status may move there.
    const toDo = (id: string): MoveView => ({ id, name: 'Reopen', to: { id: 's1', name: 'To Do' } } as MoveView);
    setBindingMock('TrackerMoves', async (_c: string, issue: string) => [toDo(`r${issue}`), toDone(`n${issue}`)]);
    const sent: string[] = [];
    setBindingMock('MoveTrackerIssue', async (_c: string, input: { issue: string; move: string }) => { sent.push(input.move); return applied(); });
    const view = draw([rowOf(1, 'Done'), rowOf(2, 'Done')]);
    const moveBoth = async (to: string) => {
      await select('WEB-1', 'WEB-2');
      await fireEvent.click(view.getByTestId('ticket-list-move'));
      await vi.waitFor(() => expect(page().getAllByTestId('ticket-list-move-status')).toHaveLength(2));
      return page().getAllByTestId('ticket-list-move-status').find((b) => b.textContent?.includes(to))!;
    };
    const done = await moveBoth('Done');
    expect(done.textContent).toContain('They are all in Done already');
    await fireEvent.keyDown(document, { key: 'Escape' });
    await fireEvent.click(view.getByTestId('ticket-list-clear'));
    await fireEvent.click(await moveBoth('To Do'));
    await vi.waitFor(() => expect(view.getByTestId('ticket-list-note').textContent?.trim()).toBe('2 tickets moved to To Do'));
    await fireEvent.click(await moveBoth('Done'));
    await vi.waitFor(() => expect(view.getByTestId('ticket-list-note').textContent?.trim()).toBe('2 tickets moved to Done'));
    expect(sent).toEqual(['r10101', 'r10102', 'n10101', 'n10102']);

    // Counted again, the rows say where the tickets are.
    await view.rerender({ rows: [rowOf(1, 'To Do'), rowOf(2, 'To Do')] });
    expect((await moveBoth('To Do')).textContent).toContain('They are all in To Do already');
  });

  it('plans from each ticket\'s own move as it settles, so a slow batch never undoes a later move', async () => {
    const to = (id: string, sid: string, name: string): MoveView => ({ id, name, to: { id: sid, name } } as MoveView);
    setBindingMock('TrackerMoves', async (_c: string, issue: string) => [to(`r${issue}`, 's2', 'Review'), to(`d${issue}`, 's3', 'Done')]);
    let releaseB: () => void = () => {};
    setBindingMock('MoveTrackerIssue', (_c: string, input: { issue: string; move: string }) =>
      input.move === 'r10102' ? new Promise((resolve) => { releaseB = () => resolve(applied()); }) : Promise.resolve(applied()));
    const view = draw([rowOf(1), rowOf(2)]);
    const statusOf = async (name: string) => {
      await vi.waitFor(() => expect(page().getAllByTestId('ticket-list-move-status')).toHaveLength(2));
      return page().getAllByTestId('ticket-list-move-status').find((b) => b.textContent?.includes(name))!;
    };
    await select('WEB-1', 'WEB-2');
    await fireEvent.click(view.getByTestId('ticket-list-move'));
    await fireEvent.click(await statusOf('Review'));
    await vi.waitFor(() => expect(view.queryAllByTestId('ticket-write-pending')).toHaveLength(1));

    // WEB-1 is in Review while WEB-2 still saves.
    await select('WEB-1');
    await fireEvent.click(view.getByTestId('ticket-list-move'));
    expect((await statusOf('Review')).textContent).toContain('It is in Review already');
    await fireEvent.click(await statusOf('Done'));
    await flush();
    releaseB();
    await flush();

    await select('WEB-1');
    await fireEvent.click(view.getByTestId('ticket-list-move'));
    expect((await statusOf('Done')).textContent).toContain('It is in Done already');
    expect((await statusOf('Review')).textContent).not.toContain('already');
  });

  it('closes Move to when a count again changes a selected ticket\'s status', async () => {
    setBindingMock('TrackerMoves', async () => [toDone('n')]);
    const view = draw([rowOf(1, 'Done'), rowOf(2)]);
    await select('WEB-1', 'WEB-2');
    await fireEvent.click(view.getByTestId('ticket-list-move'));
    await vi.waitFor(() => expect(page().getByTestId('ticket-list-move-status')).toBeTruthy());
    await view.rerender({ rows: [rowOf(1), rowOf(2)] });
    await flush();
    expect(page().queryByTestId('ticket-list-move-title')).toBeNull();
    expect(view.getByTestId('ticket-list-count').textContent).toBe('2 selected');
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
      expect((view.getByTestId(id) as HTMLButtonElement).disabled).toBe(true);
    }
    // Said in words, which a tap on a phone reaches, not only in a tip.
    expect(view.getByTestId('ticket-list-off').textContent?.trim()).toBe('This device may not change tickets on this computer.');
  });

  it('stops the moves a closed menu still waited to read, so the next action is not queued behind them', async () => {
    const asked: string[] = [];
    const held: Array<() => void> = [];
    setBindingMock('TrackerMoves', (_c: string, issue: string) => {
      asked.push(issue);
      return new Promise((resolve) => held.push(() => resolve([toDone(`n${issue}`)])));
    });
    const rows = Array.from({ length: 10 }, (_, i) => rowOf(i + 1));
    const view = draw(rows);
    await select(...rows.map((r) => r.key));
    await fireEvent.click(view.getByTestId('ticket-list-move'));
    await flush();
    expect(asked).toHaveLength(BULK_AT_ONCE);
    await fireEvent.keyDown(document, { key: 'Escape' });
    await flush();
    expect(page().queryByTestId('ticket-list-move-title')).toBeNull();
    while (held.length > 0) held.shift()!();
    await flush();
    expect(asked).toHaveLength(BULK_AT_ONCE);
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
    expect(view.getByTestId('ticket-list-off').textContent?.trim()).toBe('A thread takes at most 8 tickets, one file each; 9 are selected.');
    view.unmount();

    const blocked = draw([rowOf(1)], { threadBlocked: 'Pick the project its thread goes in.' });
    await select('WEB-1');
    expect((blocked.getByTestId('ticket-list-thread') as HTMLButtonElement).disabled).toBe(true);
    expect(blocked.getByTestId('ticket-list-off').textContent?.trim()).toBe('Pick the project its thread goes in.');
  });

  it('starts one thread at a time, and a refused start gives back its text and selection', async () => {
    let answer: (ok: boolean) => void = () => {};
    setBindingMock('StartTicketsThread', () => new Promise((resolve, reject) => {
      answer = (ok) => (ok ? resolve({ id: 't-1', projectId: 'p1' } as unknown as Thread) : reject(new Error('the tracker did not answer')));
    }));
    const view = draw([rowOf(1), rowOf(2)]);
    await select('WEB-1');
    await fireEvent.click(view.getByTestId('ticket-list-thread'));
    await fireEvent.input(page().getByTestId('ticket-list-thread-text'), { target: { value: 'about one' } });
    await fireEvent.click(page().getByTestId('ticket-list-thread-start'));
    await flush();
    await select('WEB-2');
    expect((view.getByTestId('ticket-list-thread') as HTMLButtonElement).disabled).toBe(true);
    expect(view.getByTestId('ticket-list-off').textContent).toContain('A thread is being started on 1 ticket');
    await fireEvent.click(view.getByTestId('ticket-list-clear'));
    answer(false);
    await flush();
    expect(view.getByTestId('ticket-list-count').textContent).toBe('1 selected');
    expect(page().getByRole('checkbox', { name: 'Select WEB-1' }).getAttribute('aria-checked')).toBe('true');
    await fireEvent.click(view.getByTestId('ticket-list-thread'));
    expect((page().getByTestId('ticket-list-thread-text') as HTMLInputElement).value).toBe('about one');
  });

  it('keeps typed text with the tickets it was typed for', async () => {
    const view = draw([rowOf(1), rowOf(2)]);
    await select('WEB-1', 'WEB-2');
    await fireEvent.click(view.getByTestId('ticket-list-thread'));
    await fireEvent.input(page().getByTestId('ticket-list-thread-text'), { target: { value: 'compare these two' } });
    await view.rerender({ rows: [rowOf(1)] });
    await flush();
    await fireEvent.click(view.getByTestId('ticket-list-thread'));
    expect((page().getByTestId('ticket-list-thread-text') as HTMLInputElement).value).toBe('');
  });

  it('keeps the text and the selection when the start is refused, even with two tickets', async () => {
    let answer: (ok: boolean) => void = () => {};
    setBindingMock('StartTicketsThread', () => new Promise((resolve, reject) => {
      answer = (ok) => (ok ? resolve({ id: 't-1', projectId: 'p1' } as unknown as Thread) : reject(new Error('the tracker did not answer')));
    }));
    setBindingMock('SwitchThread', async (id: string) => ({ id, projectId: 'p1' } as unknown as Thread));
    const view = draw([rowOf(1), rowOf(2), rowOf(3)]);
    await select('WEB-1', 'WEB-2');
    await fireEvent.click(view.getByTestId('ticket-list-thread'));
    await fireEvent.input(page().getByTestId('ticket-list-thread-text'), { target: { value: 'why stuck' } });
    await fireEvent.click(page().getByTestId('ticket-list-thread-start'));
    answer(false);
    await flush();
    expect(view.getByTestId('ticket-list-count').textContent).toBe('2 selected');
    await fireEvent.click(view.getByTestId('ticket-list-thread'));
    expect((page().getByTestId('ticket-list-thread-text') as HTMLInputElement).value).toBe('why stuck');
  });
});
