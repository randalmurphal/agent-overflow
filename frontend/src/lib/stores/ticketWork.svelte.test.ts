// A ticket's Work read, Ask and the Start work run: the thread first and
// nothing on the ticket when it fails, the move and the assignment after
// reading the ticket again, "already" for what it holds, a refused move
// that leaves the thread standing, Try again that never makes a second
// thread, the defaults saved under the revision read, and the thread
// opening only when every step held.

import { describe, expect, it } from 'vitest';
import { tick } from 'svelte';
import type { DetailView, SetupView, TicketSubscription, TicketView, WriteView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { trackerIssueUpdated } from './trackerBoardEvents';
import { attachTrackerTicket, type TrackerTicketRef } from './trackerTicket.svelte';
import { ticketWrite, type TicketWriteContext } from './trackerTicketWrites.svelte';
import {
  askAboutTicket,
  loadStartWorkDefaults,
  loadTicketWork,
  retryStartWork,
  startWork,
  startWorkRun,
  ticketWork,
  type StartWorkPlan,
} from './ticketWork.svelte';
import { getAllPanes } from './panes.svelte';
import { getThreadById } from './threads.svelte';
import { getToasts } from './toast.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import { DisconnectedError, TransportError } from '../transport/wsClient';
import { setBindingMock } from '../../test/mocks/bindings-app';
import type { Thread } from '../types/models';

const REF: TrackerTicketRef = { backend: HOME_BACKEND, connection: 'c1', issue: '10101' };

// What the tracker holds now; each read of the view answers it.
let held: DetailView;

function detail(status: { id: string; name?: string }, assignee?: string): DetailView {
  return {
    card: { id: '10101', key: 'WEB-1', title: 'Retry cursor', status, assignee: assignee ? { id: assignee, name: 'User 1' } : null, created: 0, updated: 0 },
    url: 'https://example.atlassian.net/browse/WEB-1', description: '', fields: [], comments: [], links: [], attachments: [],
    history: [], rows: [],
    moves: status.id === 's1' ? [{ id: '21', name: 'Start', to: { id: 's2', name: 'In Progress' } }] : [{ id: '31', name: 'Stop', to: { id: 's1', name: 'To Do' } }],
  } as unknown as DetailView;
}

function thread(id: string): Thread {
  return {
    id, projectId: 'p1', title: 'WEB-1 Retry cursor', provider: 'claude', model: '', createdAt: 1, updatedAt: 1, archived: false,
  } as unknown as Thread;
}

async function flush(): Promise<void> {
  for (let i = 0; i < 10; i += 1) await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
}

let seq = 10;

/** The ticket open on a surface, its Refresh reading what `held` is. */
async function openTicket() {
  held = detail({ id: 's1', name: 'To Do' });
  seq = 10;
  setBindingMock('SubscribeTrackerTicket', async () => ({ id: 'sub-1', seq, poll_every: 10_000, loaded: true, detail: held, read_at: 100 } as TicketSubscription));
  setBindingMock('TrackerTicket', async () => ({ loaded: true, detail: held, read_at: 200 } as TicketView));
  setBindingMock('SetTrackerSubscriptionVisible', async () => {});
  setBindingMock('ReleaseTrackerSubscription', async () => {});
  // A thread mounted in a pane tells its computer.
  setBindingMock('SwitchThread', async (id: string) => thread(id));
  // The computer tells the read's event before Refresh answers.
  const refresh = setBindingMock('RefreshTrackerSubscription', async () => {
    seq += 1;
    trackerIssueUpdated({ connection: 'c1', issue: '10101', seq, changed: true }, HOME_BACKEND);
  });
  const ticket = attachTrackerTicket(REF, true);
  await flush();
  const ctx: TicketWriteContext = { key: ticket.key, ref: ticket.ref, label: 'WEB-1' };
  return { ticket, ctx, refresh, read: () => ticket.current };
}

const PLAN: StartWorkPlan = {
  repository: 'p1',
  status: { id: 's2', name: 'In Progress' },
  assign: true,
  me: 'acc-1',
  branch: 'WEB-1-retry-cursor',
  save: null,
};

function applied(): WriteView {
  return { parts: [{ step: 'edit', state: 'applied' }] } as WriteView;
}

function refused(message: string): WriteView {
  return { parts: [{ step: 'move', state: 'refused', refusal: { reason: 'tracker', message } }] } as WriteView;
}

function steps(key: string): Record<string, string> {
  const run = startWorkRun(key);
  return Object.fromEntries(Object.entries(run?.steps ?? {}).map(([k, v]) => [k, v.kind]));
}

function mounted(id: string): boolean {
  return [...getAllPanes().values()].some((p) => p.threadId === id);
}

describe('the Work read', () => {
  it('keeps the newest read and a dropped connection keeps the last answer quietly', async () => {
    const { ticket } = await openTicket();
    const answers: Array<(v: unknown) => void> = [];
    const fails: Array<(e: unknown) => void> = [];
    setBindingMock('GetTicketWork', () => new Promise((resolve, reject) => { answers.push(resolve); fails.push(reject); }));
    const first = loadTicketWork(ticket.key, REF);
    const second = loadTicketWork(ticket.key, REF);
    answers[1]({ key: 'WEB-1', pullRequests: [], branches: [{ projectId: 'p1', name: 'WEB-1-new' }], problems: [] });
    answers[0]({ key: 'WEB-1', pullRequests: [], branches: [{ projectId: 'p1', name: 'WEB-1-old' }], problems: [] });
    await Promise.all([first, second]);
    expect(ticketWork(ticket.key)?.work?.branches.map((b) => b.name)).toEqual(['WEB-1-new']);

    const third = loadTicketWork(ticket.key, REF);
    fails[2](new DisconnectedError('closed'));
    await third;
    expect(ticketWork(ticket.key)).toMatchObject({ loading: false, error: null });
    expect(ticketWork(ticket.key)?.work?.branches.map((b) => b.name)).toEqual(['WEB-1-new']);

    const fourth = loadTicketWork(ticket.key, REF);
    fails[3](new TransportError('ticket_not_read', 'this computer has not read the ticket yet: open it first'));
    await fourth;
    expect((ticketWork(ticket.key)!.error as Error).message).toContain('has not read the ticket yet');
    ticket.release();
    expect(ticketWork(ticket.key)).toBeNull();
  });
});

describe('Ask', () => {
  it('makes the linked thread, lists it and opens it, writing nothing to the ticket', async () => {
    const { ticket, ctx } = await openTicket();
    const start = setBindingMock('StartTicketThread', async () => thread('t-ask'));
    const move = setBindingMock('MoveTrackerIssue', async () => applied());
    await askAboutTicket(ctx, 'p1');
    expect(start).toHaveBeenCalledWith('p1', expect.objectContaining({ connectionId: 'c1', issueId: '10101', origin: 'ask' }));
    expect(getThreadById('t-ask')).toBeTruthy();
    expect(mounted('t-ask')).toBe(true);
    expect(move).not.toHaveBeenCalled();
    ticket.release();
  });

  it('says why when the thread cannot be made', async () => {
    const { ticket, ctx } = await openTicket();
    setBindingMock('StartTicketThread', async () => { throw new TransportError('ticket_thread_invalid', 'the project is not on this computer'); });
    await askAboutTicket(ctx, 'p1');
    expect(getToasts().at(-1)?.message).toBe('Could not start a thread on WEB-1: The project is not on this computer.');
    ticket.release();
  });
});

describe('Start work', () => {
  it('makes the thread, then moves and assigns after reading the ticket again, and opens the thread', async () => {
    const { ticket, ctx, refresh, read } = await openTicket();
    const order: string[] = [];
    setBindingMock('StartTicketThread', async (_p: string, req: unknown) => { order.push(`thread ${JSON.stringify(req)}`); return thread('t-work'); });
    setBindingMock('MoveTrackerIssue', async (_c: string, input: { move: string }) => { order.push(`move ${input.move}`); held = detail({ id: 's2', name: 'In Progress' }, held.card.assignee?.id); return applied(); });
    setBindingMock('EditTrackerIssue', async (_c: string, input: { fields: unknown[] }) => { order.push(`edit ${JSON.stringify(input.fields)}`); return applied(); });
    setBindingMock('GetTicketWork', async () => ({ key: 'WEB-1', pullRequests: [], branches: [], problems: [] }));
    await startWork(ctx, PLAN, read);
    await flush();
    // The move settles first: the assignment is decided from the ticket
    // as the move left it.
    expect(order).toEqual([
      'thread {"connectionId":"c1","issueId":"10101","origin":"start_work","branch":"WEB-1-retry-cursor"}',
      'move 21',
      'edit [{"field":"assignee","value":{"kind":"user","choices":[{"id":"acc-1"}]}}]',
    ]);
    expect(refresh).toHaveBeenCalledTimes(1);
    expect(startWorkRun(ticket.key)).toBeNull();
    expect(mounted('t-work')).toBe(true);
    ticket.release();
  });

  it('sends nothing for what the ticket already holds', async () => {
    const { ticket, ctx, read } = await openTicket();
    setBindingMock('StartTicketThread', async () => thread('t-held'));
    // Someone else moved and assigned it after the form was read.
    setBindingMock('RefreshTrackerSubscription', async () => {
      held = detail({ id: 's2', name: 'In Progress' }, 'acc-1');
      seq += 1;
      trackerIssueUpdated({ connection: 'c1', issue: '10101', seq, changed: true }, HOME_BACKEND);
    });
    const move = setBindingMock('MoveTrackerIssue', async () => applied());
    const edit = setBindingMock('EditTrackerIssue', async () => applied());
    setBindingMock('GetTicketWork', async () => ({ key: 'WEB-1', pullRequests: [], branches: [], problems: [] }));
    await startWork(ctx, PLAN, read);
    expect(move).not.toHaveBeenCalled();
    expect(edit).not.toHaveBeenCalled();
    expect(mounted('t-held')).toBe(true);
    ticket.release();
  });

  it('assigns from the ticket as the move left it: a move that assigned it leaves nothing to send', async () => {
    const { ticket, ctx, read } = await openTicket();
    setBindingMock('StartTicketThread', async () => thread('t-order'));
    // The tracker's transition assigns the ticket to the mover.
    const move = setBindingMock('MoveTrackerIssue', async () => {
      held = detail({ id: 's2', name: 'In Progress' }, 'acc-1');
      seq += 1;
      trackerIssueUpdated({ connection: 'c1', issue: '10101', seq, changed: true }, HOME_BACKEND);
      return applied();
    });
    const edit = setBindingMock('EditTrackerIssue', async () => applied());
    setBindingMock('GetTicketWork', async () => ({ key: 'WEB-1', pullRequests: [], branches: [], problems: [] }));
    await startWork(ctx, PLAN, read);
    expect(move).toHaveBeenCalledTimes(1);
    expect(edit).not.toHaveBeenCalled();
    expect(mounted('t-order')).toBe(true);
    ticket.release();
  });

  it('sends nothing and says so when the read before the writes failed, even though Refresh answered', async () => {
    const { ticket, ctx, read } = await openTicket();
    // The form saw the ticket as already in progress and already this
    // person's; someone else took it since, and the tracker's read fails.
    held = detail({ id: 's2', name: 'In Progress' }, 'acc-1');
    seq += 1;
    trackerIssueUpdated({ connection: 'c1', issue: '10101', seq, changed: true }, HOME_BACKEND);
    await flush();
    held = detail({ id: 's1', name: 'To Do' }, 'acc-2');
    setBindingMock('StartTicketThread', async () => thread('t-stale'));
    setBindingMock('RefreshTrackerSubscription', async () => {
      seq += 1;
      trackerIssueUpdated({ connection: 'c1', issue: '10101', seq, changed: false, failure: { kind: 'rate_limited', message: 'The tracker is limiting requests.', at: 1 } }, HOME_BACKEND);
    });
    const move = setBindingMock('MoveTrackerIssue', async () => applied());
    const edit = setBindingMock('EditTrackerIssue', async () => applied());
    setBindingMock('GetTicketWork', async () => ({ key: 'WEB-1', pullRequests: [], branches: [], problems: [] }));
    await startWork(ctx, PLAN, read);
    expect(steps(ticket.key)).toEqual({ thread: 'applied', move: 'not_sent', assign: 'not_sent', defaults: 'skipped' });
    expect(startWorkRun(ticket.key)?.steps.assign).toMatchObject({ text: expect.stringContaining('The tracker is limiting requests.') });
    expect(move).not.toHaveBeenCalled();
    expect(edit).not.toHaveBeenCalled();
    expect(mounted('t-stale')).toBe(false);
    ticket.release();
  });

  it('reads nothing again when neither a move nor an assignment is asked for', async () => {
    const { ticket, ctx, refresh, read } = await openTicket();
    setBindingMock('StartTicketThread', async () => thread('t-plain'));
    setBindingMock('GetTicketWork', async () => ({ key: 'WEB-1', pullRequests: [], branches: [], problems: [] }));
    await startWork(ctx, { ...PLAN, status: null, assign: false }, read);
    expect(refresh).not.toHaveBeenCalled();
    expect(mounted('t-plain')).toBe(true);
    ticket.release();
  });

  it.each([
    ['the connection dropped', new DisconnectedError('closed')],
    ['the call timed out', new TransportError('timeout', 'RPC 7 timed out after 30000ms')],
  ])('does not say the thread was not made when %s before its reply, and Try again asks for it again', async (_what, err) => {
    const { ticket, ctx, read } = await openTicket();
    const start = setBindingMock('StartTicketThread', async () => { throw err; });
    const move = setBindingMock('MoveTrackerIssue', async () => applied());
    await startWork(ctx, PLAN, read);
    const run = startWorkRun(ticket.key)!;
    expect(run.steps.thread).toMatchObject({ kind: 'unknown', text: expect.stringContaining('may have been made') });
    expect(JSON.stringify(run.steps)).not.toContain('was not made');
    expect(move).not.toHaveBeenCalled();

    // The computer made it: the same call answers that thread.
    start.mockImplementation(async () => thread('t-lost'));
    setBindingMock('EditTrackerIssue', async () => applied());
    setBindingMock('GetTicketWork', async () => ({ key: 'WEB-1', pullRequests: [], branches: [], problems: [] }));
    await retryStartWork(ctx, read);
    expect(start).toHaveBeenCalledTimes(2);
    expect(mounted('t-lost')).toBe(true);
    ticket.release();
  });

  it('changes nothing on the ticket when the thread cannot be made', async () => {
    const { ticket, ctx, refresh, read } = await openTicket();
    setBindingMock('StartTicketThread', async () => { throw new TransportError('ticket_thread_branch_invalid', 'git refuses the branch name "a..b"'); });
    const move = setBindingMock('MoveTrackerIssue', async () => applied());
    await startWork(ctx, { ...PLAN, branch: 'a..b' }, read);
    expect(steps(ticket.key)).toEqual({ thread: 'failed', move: 'not_sent', assign: 'not_sent', defaults: 'not_sent' });
    expect(refresh).not.toHaveBeenCalled();
    expect(move).not.toHaveBeenCalled();
    ticket.release();
  });

  it('keeps the thread when the tracker refuses the move, and Try again sends only the move', async () => {
    const { ticket, ctx, read } = await openTicket();
    const start = setBindingMock('StartTicketThread', async () => thread('t-refused'));
    const move = setBindingMock('MoveTrackerIssue', async () => refused('A resolution is required.'));
    const edit = setBindingMock('EditTrackerIssue', async () => { held = detail(held.card.status, 'acc-1'); return applied(); });
    setBindingMock('GetTicketWork', async () => ({ key: 'WEB-1', pullRequests: [], branches: [], problems: [] }));
    await startWork(ctx, PLAN, read);
    await flush();
    expect(steps(ticket.key)).toEqual({ thread: 'applied', move: 'refused', assign: 'applied', defaults: 'skipped' });
    const run = startWorkRun(ticket.key)!;
    expect(run.steps.move).toMatchObject({ text: 'A resolution is required.' });
    expect(run.thread?.id).toBe('t-refused');
    expect(getThreadById('t-refused')).toBeTruthy();
    expect(mounted('t-refused')).toBe(false);
    // The write store holds the refusal under the Status field too.
    expect(ticketWrite(ticket.key, 'status')?.outcome?.kind).toBe('refused');

    move.mockImplementation(async () => { held = detail({ id: 's2', name: 'In Progress' }, 'acc-1'); return applied(); });
    await retryStartWork(ctx, read);
    expect(start).toHaveBeenCalledTimes(1);
    expect(move).toHaveBeenCalledTimes(2);
    expect(edit).toHaveBeenCalledTimes(1);
    expect(startWorkRun(ticket.key)).toBeNull();
    expect(mounted('t-refused')).toBe(true);
    ticket.release();
  });

  it('settles an unknown move by the next read instead of sending it again', async () => {
    const { ticket, ctx, read } = await openTicket();
    setBindingMock('StartTicketThread', async () => thread('t-unknown'));
    const move = setBindingMock('MoveTrackerIssue', async () => {
      // The tracker made the move, and the answer was lost.
      held = detail({ id: 's2', name: 'In Progress' }, held.card.assignee?.id);
      return { parts: [{ step: 'move', state: 'unknown' }] } as WriteView;
    });
    setBindingMock('EditTrackerIssue', async () => { held = detail(held.card.status, 'acc-1'); return applied(); });
    setBindingMock('GetTicketWork', async () => ({ key: 'WEB-1', pullRequests: [], branches: [], problems: [] }));
    await startWork(ctx, PLAN, read);
    await flush();
    expect(steps(ticket.key).move).toBe('unknown');
    await retryStartWork(ctx, read);
    expect(move).toHaveBeenCalledTimes(1);
    expect(startWorkRun(ticket.key)).toBeNull();
    ticket.release();
  });

  it('does not assign after a move whose reply was lost: the move assigned it, and Try again finds it already done', async () => {
    const { ticket, ctx, read } = await openTicket();
    setBindingMock('StartTicketThread', async () => thread('t-move-lost'));
    // The tracker's transition assigns the ticket to the mover; the reply
    // and its event never arrive.
    const move = setBindingMock('MoveTrackerIssue', async () => {
      held = detail({ id: 's2', name: 'In Progress' }, 'acc-1');
      throw new DisconnectedError('closed');
    });
    const edit = setBindingMock('EditTrackerIssue', async () => applied());
    setBindingMock('GetTicketWork', async () => ({ key: 'WEB-1', pullRequests: [], branches: [], problems: [] }));
    await startWork(ctx, PLAN, read);
    await flush();
    expect(steps(ticket.key)).toEqual({ thread: 'applied', move: 'unknown', assign: 'not_sent', defaults: 'skipped' });
    expect(startWorkRun(ticket.key)?.steps.assign).toMatchObject({ text: 'Not decided: the move is not confirmed. Try again reads the ticket first.' });
    expect(edit).not.toHaveBeenCalled();

    await retryStartWork(ctx, read);
    expect(move).toHaveBeenCalledTimes(1);
    expect(edit).not.toHaveBeenCalled();
    expect(startWorkRun(ticket.key)).toBeNull();
    expect(mounted('t-move-lost')).toBe(true);
    ticket.release();
  });

  it('does not call the ticket yours after a move that timed out: the move gave it away, and Try again assigns it', async () => {
    const { ticket, ctx, read } = await openTicket();
    // The ticket is this person's when the run starts.
    held = detail({ id: 's1', name: 'To Do' }, 'acc-1');
    setBindingMock('StartTicketThread', async () => thread('t-move-timeout'));
    const move = setBindingMock('MoveTrackerIssue', async () => {
      held = detail({ id: 's2', name: 'In Progress' }, 'acc-2');
      throw new TransportError('timeout', 'RPC 9 timed out after 30000ms');
    });
    const edit = setBindingMock('EditTrackerIssue', async () => { held = detail(held.card.status, 'acc-1'); return applied(); });
    setBindingMock('GetTicketWork', async () => ({ key: 'WEB-1', pullRequests: [], branches: [], problems: [] }));
    await startWork(ctx, PLAN, read);
    await flush();
    expect(steps(ticket.key)).toEqual({ thread: 'applied', move: 'unknown', assign: 'not_sent', defaults: 'skipped' });

    await retryStartWork(ctx, read);
    expect(move).toHaveBeenCalledTimes(1);
    expect(edit).toHaveBeenCalledTimes(1);
    expect(startWorkRun(ticket.key)).toBeNull();
    ticket.release();
  });

  it('saves the defaults under the revision the form read, and says when the setup moved', async () => {
    const { ticket, ctx, read } = await openTicket();
    held = detail({ id: 's2', name: 'In Progress' }, 'acc-1');
    const view = (revision: string): SetupView => ({ revision, setup: { status_groups: [], ticket_pins: [], start_work: [{ tracker: 'jira', site: 'https://example.atlassian.net', project: 'OPS', assign: false }] } } as SetupView);
    setBindingMock('ProjectTrackerSetup', async () => view('r1'));
    await loadStartWorkDefaults(ticket.key, 'p1');
    setBindingMock('StartTicketThread', async () => thread('t-defaults'));
    setBindingMock('GetTicketWork', async () => ({ key: 'WEB-1', pullRequests: [], branches: [], problems: [] }));
    const save = setBindingMock('SetProjectTrackerSetup', async () => ({ saved: false, refused: 'this change was not saved: the project\'s tracker setup changed after it was read', stored: view('r2') }));
    const place = { tracker: 'jira', site: 'https://example.atlassian.net', project: 'WEB' };
    await startWork(ctx, { ...PLAN, repository: 'p2', save: { project: 'p1', place, branchTemplate: 'feature/{key}' } }, read);
    expect(save).toHaveBeenCalledWith('p1', 'r1', expect.objectContaining({
      start_work: [
        { tracker: 'jira', site: 'https://example.atlassian.net', project: 'OPS', assign: false },
        { ...place, status: 's2', assign: true, branch: 'feature/{key}', repository: 'p2' },
      ],
    }));
    expect(startWorkRun(ticket.key)?.steps.defaults).toMatchObject({ kind: 'refused', text: expect.stringContaining('changed after it was read') });

    save.mockImplementation(async () => ({ saved: true, stored: view('r3') }));
    await retryStartWork(ctx, read);
    expect(save).toHaveBeenLastCalledWith('p1', 'r2', expect.anything());
    expect(startWorkRun(ticket.key)).toBeNull();
    ticket.release();
  });
});
