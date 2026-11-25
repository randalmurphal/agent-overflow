// The ticket store and its writes: one handle across the surfaces that
// show a ticket, its visibility vote, the order of events against the
// subscribe answer, the one read of the view in flight, a gap, the
// earlier comments a person loaded, and each write's lifecycle: sent once
// per target, kept past the ticket's close, cleared when the view lands
// or the connection drops, and Save anyway as the person's second press.

import { afterEach, describe, expect, it } from 'vitest';
import { tick } from 'svelte';
import type {
  CommentPageView,
  DetailView,
  TicketSubscription,
  TicketView,
  WriteView,
} from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { trackerIssueUpdated, trackerTicketsGap, type TrackerIssueEvent } from './trackerBoardEvents';
import {
  attachTrackerTicket,
  loadEarlierTrackerComments,
  refreshTrackerTicket,
  type TrackerTicketRef,
} from './trackerTicket.svelte';
import {
  addTicketComment,
  editTicketComment,
  editTicketFields,
  fieldTarget,
  moveTicket,
  ticketWrite,
  type TicketWriteContext,
} from './trackerTicketWrites.svelte';
import { getToasts } from './toast.svelte';
import { __setTransportStatusForTest } from './transportStatus.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import { takePinnedBackend } from '../transport/backends';
import { DisconnectedError, TransportError } from '../transport/wsClient';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { stageBackend } from '../../test/helpers/backends';

const REF: TrackerTicketRef = { backend: HOME_BACKEND, connection: 'c1', issue: '10101' };

function detail(title = 'First story', extra: Partial<DetailView> = {}): DetailView {
  return {
    card: { id: '10101', key: 'WEB-1', title, status: { id: 's1', name: 'To Do' }, created: 0, updated: 0 },
    url: 'https://example.atlassian.net/browse/WEB-1', description: '', fields: [], comments: [], links: [], attachments: [],
    history: [], moves: [], rows: [], ...extra,
  } as unknown as DetailView;
}

function answer(extra: Partial<TicketSubscription> = {}): TicketSubscription {
  return { id: 'sub-1', seq: 10, poll_every: 10_000, loaded: true, detail: detail(), read_at: 100, ...extra } as TicketSubscription;
}

function event(seq: number, extra: Partial<TrackerIssueEvent> = {}): TrackerIssueEvent {
  return { connection: 'c1', issue: '10101', seq, ...extra };
}

function applied(): WriteView {
  return { parts: [{ step: 'edit', state: 'applied' }] } as WriteView;
}

async function flush(): Promise<void> {
  for (let i = 0; i < 10; i += 1) await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
}

/** A binding mock whose answers the test releases one at a time. */
function held<T>(name: string): { calls: unknown[][]; answer: (value: T) => void; fail: (err: unknown) => void } {
  const waiting: Array<{ resolve: (v: T) => void; reject: (e: unknown) => void }> = [];
  const calls: unknown[][] = [];
  setBindingMock(name, (...args: unknown[]) => {
    calls.push(args);
    return new Promise<T>((resolve, reject) => waiting.push({ resolve, reject }));
  });
  return {
    calls,
    answer: (value) => waiting.shift()!.resolve(value),
    fail: (err) => waiting.shift()!.reject(err),
  };
}

function install(first: Partial<TicketSubscription> = {}) {
  let n = 0;
  return {
    subscribe: setBindingMock('SubscribeTrackerTicket', async () => {
      n += 1;
      return answer({ id: `sub-${n}`, ...first });
    }),
    read: setBindingMock('TrackerTicket', async (): Promise<TicketView> => ({ loaded: true, detail: detail('Read again'), read_at: 200 } as TicketView)),
    visible: setBindingMock('SetTrackerSubscriptionVisible', async () => {}),
    refresh: setBindingMock('RefreshTrackerSubscription', async () => {}),
    release: setBindingMock('ReleaseTrackerSubscription', async () => {}),
  };
}

function ctxOf(ticket: { key: string; ref: TrackerTicketRef }): TicketWriteContext {
  return { key: ticket.key, ref: ticket.ref, label: 'WEB-1' };
}

afterEach(() => {
  delete (document as unknown as Record<string, unknown>).visibilityState;
});

describe('the ticket subscription', () => {
  it('holds one handle for every surface that shows the ticket, and votes it shown while any does', async () => {
    const mocks = install();
    const over = attachTrackerTicket(REF, false);
    await flush();
    expect(mocks.subscribe).toHaveBeenCalledWith('c1', '10101', false);
    expect(over.current?.detail?.card.title).toBe('First story');
    expect(over.current?.readAt).toBe(100);

    const beside = attachTrackerTicket(REF, true);
    await flush();
    expect(mocks.subscribe).toHaveBeenCalledTimes(1);
    expect(mocks.visible.mock.calls).toEqual([['sub-1', true]]);
    beside.setShown(false);
    await flush();
    expect(mocks.visible.mock.calls.at(-1)).toEqual(['sub-1', false]);
    over.setShown(true);
    await flush();
    expect(mocks.visible.mock.calls.at(-1)).toEqual(['sub-1', true]);
    // The surface that showed it goes: the other is hidden, so the vote
    // goes down with it.
    over.release();
    await flush();
    expect(mocks.release).not.toHaveBeenCalled();
    expect(mocks.visible.mock.calls.at(-1)).toEqual(['sub-1', false]);
    beside.release();
    await flush();
    expect(mocks.release).toHaveBeenCalledWith('sub-1');
  });

  it('votes the ticket down while the window is hidden', async () => {
    const mocks = install();
    const ticket = attachTrackerTicket(REF, true);
    await flush();
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'hidden' });
    document.dispatchEvent(new Event('visibilitychange'));
    await flush();
    expect(mocks.visible.mock.calls.at(-1)).toEqual(['sub-1', false]);
    ticket.release();
  });

  it('pins every call of the ticket to the computer that holds its connection', async () => {
    stageBackend();
    const routed: string[] = [];
    const note = (name: string) => routed.push(`${name}@${String(takePinnedBackend() ?? '')}`);
    setBindingMock('SubscribeTrackerTicket', async () => { note('subscribe'); return answer({ detail: detail('x', { more_comments: 'p2' }) }); });
    setBindingMock('TrackerTicket', async () => { note('read'); return { loaded: true, detail: detail('x', { more_comments: 'p2' }) } as TicketView; });
    setBindingMock('TrackerComments', async () => { note('comments'); return { comments: [] } as unknown as CommentPageView; });
    setBindingMock('RefreshTrackerSubscription', async () => note('refresh'));
    setBindingMock('EditTrackerIssue', async () => { note('edit'); return applied(); });
    setBindingMock('ReleaseTrackerSubscription', async () => note('release'));
    const ref = { ...REF, backend: 'laptop' };
    const ticket = attachTrackerTicket(ref, true);
    await flush();
    trackerIssueUpdated(event(11, { changed: true }), 'laptop');
    await flush();
    await loadEarlierTrackerComments(ticket.key);
    await refreshTrackerTicket(ticket.key);
    await editTicketFields(ctxOf(ticket), fieldTarget('priority'), [{ field: 'priority', value: { kind: 'option', choices: [{ id: '2' }] } as never }]);
    ticket.release();
    await flush();
    expect(routed).toEqual(['subscribe', 'read', 'comments', 'refresh', 'edit', 'release'].map((n) => `${n}@laptop`));
  });

  it('drops an event the answer holds, holds one that arrives before it, and reads the view once per change', async () => {
    install();
    const subscribe = held<TicketSubscription>('SubscribeTrackerTicket');
    const reads = held<TicketView>('TrackerTicket');
    const ticket = attachTrackerTicket(REF, true);
    await flush();
    trackerIssueUpdated(event(9, { changed: true }), HOME_BACKEND);
    trackerIssueUpdated(event(12, { changed: true, failure: { kind: 'transient', message: 'Jira did not answer', at: 5 } }), HOME_BACKEND);
    subscribe.answer(answer());
    await flush();
    expect(reads.calls).toEqual([['sub-1']]);
    expect(ticket.current?.failure?.message).toBe('Jira did not answer');
    // Two changes told while the read is out ask for one more read; each
    // event carries the ticket's failure as of it.
    const stillFailing = { kind: 'transient', message: 'Jira did not answer', at: 5 };
    trackerIssueUpdated(event(13, { changed: true, failure: stillFailing }), HOME_BACKEND);
    trackerIssueUpdated(event(14, { changed: true, failure: stillFailing }), HOME_BACKEND);
    trackerIssueUpdated(event(14, { changed: true }), HOME_BACKEND);
    reads.answer({ loaded: true, detail: detail('Renamed'), read_at: 300 } as TicketView);
    await flush();
    expect(reads.calls.length).toBe(2);
    // The events told a newer failure than the read that began before them.
    expect(ticket.current?.detail?.card.title).toBe('Renamed');
    expect(ticket.current?.failure?.message).toBe('Jira did not answer');
    reads.answer({ loaded: true, detail: detail('Renamed again'), read_at: 400 } as TicketView);
    await flush();
    expect(ticket.current?.detail?.card.title).toBe('Renamed again');
    // A read that began after the last event tells the failure as of it.
    expect(ticket.current?.failure).toBeNull();
    expect(ticket.current?.readAt).toBe(400);
    // An end is the last event.
    trackerIssueUpdated(event(15, { ended: true }), HOME_BACKEND);
    await flush();
    expect(ticket.current?.ended).toBe(true);
    trackerIssueUpdated(event(16, { changed: true }), HOME_BACKEND);
    await flush();
    expect(reads.calls.length).toBe(2);
    ticket.release();
  });

  it('reads every held ticket of the computer again on a gap, under its recovery', async () => {
    const mocks = install();
    const ticket = attachTrackerTicket(REF, true);
    const other = attachTrackerTicket({ ...REF, issue: '10102' }, true);
    await flush();
    expect(trackerTicketsGap('laptop')).toBeUndefined();
    const recovery = trackerTicketsGap(HOME_BACKEND);
    expect(recovery).toBeDefined();
    await recovery;
    expect(mocks.read).toHaveBeenCalledTimes(2);
    expect(ticket.current?.detail?.card.title).toBe('Read again');
    ticket.release();
    other.release();
  });

  it('shows a failed read of the view and reads it again with the next event or Refresh', async () => {
    const mocks = install();
    mocks.read.mockRejectedValueOnce(new Error('the copy could not be read'));
    const ticket = attachTrackerTicket(REF, true);
    await flush();
    trackerIssueUpdated(event(11, { changed: true }), HOME_BACKEND);
    await flush();
    expect((ticket.current?.readError as Error).message).toBe('the copy could not be read');
    await refreshTrackerTicket(ticket.key);
    await flush();
    expect(mocks.refresh).toHaveBeenCalledWith('sub-1');
    expect(ticket.current?.readError).toBeNull();
    expect(ticket.current?.detail?.card.title).toBe('Read again');
    ticket.release();
  });

  it('ends a ticket whose connection is turned off, and subscribes again on reconnect keeping the earlier comments', async () => {
    const mocks = install({ detail: detail('x', { comments: [{ id: '9', author: { id: 'u' }, body: 'newest', created: 9 }], more_comments: 'p2' } as never) });
    setBindingMock('TrackerComments', async () => ({ comments: [{ id: '8', author: { id: 'u' }, body: 'older', created: 8 }], next: '' }));
    const ticket = attachTrackerTicket(REF, true);
    await flush();
    await loadEarlierTrackerComments(ticket.key);
    expect(ticket.current?.earlier.comments.map((c) => c.id)).toEqual(['8']);
    expect(ticket.current?.earlier.next).toBe('');
    __setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await flush();
    expect(mocks.subscribe).toHaveBeenCalledTimes(2);
    expect(ticket.current?.earlier.comments.map((c) => c.id)).toEqual(['8']);
    ticket.release();

    mocks.subscribe.mockRejectedValue(new TransportError('tracker_connection_off', 'this tracker connection is turned off'));
    const off = attachTrackerTicket(REF, true);
    await flush();
    expect(off.current?.ended).toBe(true);
    expect(off.current?.earlier.comments).toEqual([]);
    off.release();
  });
});

describe('a ticket write', () => {
  it('sends once when its target is pressed twice, and stops saying Saving once the view it published lands', async () => {
    install();
    const edits = held<WriteView>('EditTrackerIssue');
    const reads = held<TicketView>('TrackerTicket');
    const ticket = attachTrackerTicket(REF, true);
    await flush();
    const ctx = ctxOf(ticket);
    const target = fieldTarget('priority');
    const edit = [{ field: 'priority', value: { kind: 'option', choices: [{ id: '2' }] } as never }];
    const first = editTicketFields(ctx, target, edit);
    const second = editTicketFields(ctx, target, edit);
    expect(await second).toBeNull();
    expect(edits.calls.length).toBe(1);
    expect(ticketWrite(ticket.key, target)?.pending).toBe(true);
    // The write's refresh tells its event before its answer arrives.
    trackerIssueUpdated(event(11, { changed: true }), HOME_BACKEND);
    edits.answer(applied());
    await flush();
    expect(ticketWrite(ticket.key, target)?.pending).toBe(true);
    reads.answer({ loaded: true, detail: detail('Saved'), read_at: 200 } as TicketView);
    expect(await first).toEqual({ kind: 'applied', note: null });
    expect(ticketWrite(ticket.key, target)).toBeNull();
    expect(ticket.current?.detail?.card.title).toBe('Saved');
    ticket.release();
  });

  it('does not stay Saving when the connection drops after the answer, before the view it published is read', async () => {
    install();
    const reads = held<TicketView>('TrackerTicket');
    setBindingMock('MoveTrackerIssue', async () => {
      trackerIssueUpdated(event(11, { changed: true }), HOME_BACKEND);
      return { parts: [{ step: 'move', state: 'applied' }] } as WriteView;
    });
    const ticket = attachTrackerTicket(REF, true);
    await flush();
    const moved = moveTicket(ctxOf(ticket), '31');
    await flush();
    expect(ticketWrite(ticket.key, 'status')?.pending).toBe(true);
    reads.fail(new DisconnectedError('client closed'));
    expect(await moved).toEqual({ kind: 'applied', note: null });
    expect(ticketWrite(ticket.key, 'status')).toBeNull();
    ticket.release();
  });

  it('says a write whose answer a dropped connection lost may or may not have been made', async () => {
    install();
    setBindingMock('AddTrackerComment', async () => { throw new DisconnectedError('client closed'); });
    const ticket = attachTrackerTicket(REF, true);
    await flush();
    const outcome = await addTicketComment(ctxOf(ticket), 'Looks good');
    expect(outcome?.kind).toBe('lost');
    expect(ticketWrite(ticket.key, 'new-comment')?.outcome).toEqual({
      kind: 'lost',
      message: 'This computer: the connection dropped before the answer, so the change may or may not have been made; look at the ticket before you try again.',
    });
    ticket.release();
  });

  it('toasts a refused or unknown write that settles after its ticket closed, and stays silent when it was made', async () => {
    install();
    const edits = held<WriteView>('EditTrackerIssue');
    const ticket = attachTrackerTicket(REF, true);
    await flush();
    const ctx = ctxOf(ticket);
    const refused = editTicketFields(ctx, fieldTarget('type'), [{ field: 'type', value: { kind: 'option', choices: [{ id: '3' }] } as never }]);
    const unknown = editTicketFields(ctx, fieldTarget('priority'), [{ field: 'priority', value: { kind: 'option', choices: [{ id: '2' }] } as never }]);
    const made = editTicketFields(ctx, fieldTarget('assignee'), [{ field: 'assignee', value: {} as never }]);
    ticket.release();
    await flush();
    const before = getToasts().length;
    edits.answer({ parts: [{ step: 'edit', state: 'refused', refusal: { reason: 'tracker', message: 'The issue type cannot change here' } }] } as WriteView);
    edits.answer({ parts: [{ step: 'edit', state: 'unknown', failure: { kind: 'transient', message: 'Jira did not answer in time, so the change may or may not have been made; look at the ticket before you try again', at: 1 } }] } as WriteView);
    edits.answer(applied());
    await Promise.all([refused, unknown, made]);
    expect(getToasts().slice(before).map((t) => t.message)).toEqual([
      'The tracker refused the change to WEB-1: The issue type cannot change here',
      'The change to WEB-1: Jira did not answer in time, so the change may or may not have been made; look at the ticket before you try again',
    ]);
    expect(ticketWrite(ticket.key, fieldTarget('type'))).toBeNull();
  });

  it('sends Save anyway, accepting the loss, only on that second press', async () => {
    install();
    const edits = held<WriteView>('EditTrackerComment');
    const ticket = attachTrackerTicket(REF, true);
    await flush();
    const target = 'comment:7';
    const first = editTicketComment(ctxOf(ticket), '7', 'new body', 'old body');
    edits.answer({ parts: [{ step: 'comment', state: 'refused', refusal: { reason: 'loss', message: 'This edit would lose formatting.', lost: ['a table'] } }] } as WriteView);
    expect((await first)?.kind).toBe('refused');
    expect(edits.calls).toEqual([['c1', { issue: '10101', comment: '7', markdown: 'new body', from: 'old body', accept_loss: false }]]);
    const again = ticketWrite(ticket.key, target)?.saveAnyway;
    expect(again).toBeTypeOf('function');
    const second = again!();
    edits.answer(applied());
    expect(await second).toEqual({ kind: 'applied', note: null });
    expect(edits.calls[1]).toEqual(['c1', { issue: '10101', comment: '7', markdown: 'new body', from: 'old body', accept_loss: true }]);
    expect(ticketWrite(ticket.key, target)).toBeNull();
    // A refusal after the loss was accepted offers no second Save anyway.
    const third = editTicketComment(ctxOf(ticket), '7', 'other', 'old body');
    edits.answer({ parts: [{ step: 'comment', state: 'refused', refusal: { reason: 'tracker', message: 'no' } }] } as WriteView);
    await third;
    expect(ticketWrite(ticket.key, target)?.saveAnyway).toBeNull();
    ticket.release();
  });

  it('reads the ticket again when the tracker refuses a write as made against a stale ticket', async () => {
    const mocks = install();
    setBindingMock('MoveTrackerIssue', async () => ({ parts: [{ step: 'move', state: 'refused', refusal: { reason: 'stale', message: 'The ticket changed; look at it again' } }] }) as WriteView);
    const ticket = attachTrackerTicket(REF, true);
    await flush();
    await moveTicket(ctxOf(ticket), '31');
    await flush();
    expect(mocks.refresh).toHaveBeenCalledWith('sub-1');
    ticket.release();
  });
});

