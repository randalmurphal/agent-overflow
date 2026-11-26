// The ticket store and its writes: one handle across the surfaces that
// show a ticket, its visibility vote, the order of events against the
// subscribe answer, the one read of the view in flight, a gap, the
// earlier comments a person loaded, and each write's lifecycle: sent once
// per target, kept past the ticket's close, cleared when the view lands
// or the connection drops, and Save anyway as the person's second press.
// Planning poker's follow per surface, its reads and the site's support;
// files attached one at a time with what became of each; links added and
// removed; the poker writes' one target; a connection's link types.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { flushSync, tick } from 'svelte';
import type {
  BoardPatch,
  BoardView,
  CommentPageView,
  DetailView,
  MoveView,
  PokerView,
  TicketSubscription,
  TicketView,
  WriteView,
} from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { trackerBoardUpdated, trackerIssueUpdated, trackerTicketsGap, type TrackerIssueEvent } from './trackerBoardEvents';
import { attachTrackerBoard } from './trackerBoard.svelte';
import {
  askTrackerPokerSupport,
  attachTrackerTicket,
  loadEarlierTrackerComments,
  refreshTrackerTicket,
  trackerPokerSupport,
  type TrackerTicketRef,
} from './trackerTicket.svelte';
import { installDiagnosticsCapture } from '../../test/helpers/diagnostics';
import {
  addTicketComment,
  attachTicketFiles,
  clearTicketPokerVotes,
  dismissTicketWrite,
  drawTicketWrite,
  editTicketComment,
  editTicketFields,
  fieldTarget,
  linkTicket,
  loadTicketLinkTypes,
  moveTicket,
  openTicketMove,
  rankTicket,
  revealTicketPoker,
  ticketAttachOutcomes,
  ticketLinkTypes,
  ticketMoveDraft,
  ticketUploads,
  ticketWrite,
  unlinkTicket,
  voteTicketPoker,
  type TicketWriteContext,
} from './trackerTicketWrites.svelte';
import { trackerTicketKey } from './trackerTicket.svelte';
import { getToasts } from './toast.svelte';
import { __setTransportStatusForTest } from './transportStatus.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import { takePinnedBackend } from '../transport/backends';
import { DisconnectedError, TransportError } from '../transport/wsClient';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { stageBackend } from '../../test/helpers/backends';
import { errString } from '../utils/errors';

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
    expect((ticket.current!.readError as Error).message).toBe('the copy could not be read');
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
    expect(ticket.current?.earlier.pages.flatMap((p) => p.comments).map((c) => c.id)).toEqual(['8']);
    expect(ticket.current?.earlier.next).toBe('');
    __setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await flush();
    expect(mocks.subscribe).toHaveBeenCalledTimes(2);
    expect(ticket.current?.earlier.pages.flatMap((p) => p.comments).map((c) => c.id)).toEqual(['8']);
    ticket.release();

    mocks.subscribe.mockRejectedValue(new TransportError('tracker_connection_off', 'this tracker connection is turned off'));
    const off = attachTrackerTicket(REF, true);
    await flush();
    expect(off.current?.ended).toBe(true);
    expect(off.current?.earlier.pages).toEqual([]);
    off.release();
  });
});

describe('planning poker', () => {
  installDiagnosticsCapture();
  const votes = (value: number | null, readAt: number): PokerView => ({
    loaded: true, revealed: value !== null, votes: [{ voter: { id: 'acc-user1', name: 'User 1' }, value, mine: true }], read_at: readAt,
  } as PokerView);

  it('follows only while one surface both shows the ticket and has its poker on screen', async () => {
    install();
    const follows = setBindingMock('FollowTrackerPoker', async () => votes(null, 1));
    const over = attachTrackerTicket(REF, true);
    const beside = attachTrackerTicket(REF, false);
    await flush();
    // Shown without poker, and poker on a surface that is hidden: the
    // shared handle shows the ticket, and nobody looks at its votes.
    beside.setPokerShown(true);
    await flush();
    expect(follows).not.toHaveBeenCalled();
    expect(over.current?.poker).toBeNull();

    beside.setShown(true);
    await flush();
    expect(follows.mock.calls).toEqual([['sub-1', true]]);
    expect(beside.current?.poker).toEqual({ view: votes(null, 1), error: null });

    beside.setShown(false);
    await flush();
    expect(follows.mock.calls.at(-1)).toEqual(['sub-1', false]);
    expect(over.current?.poker).toBeNull();
    over.release();
    beside.release();
  });

  it('reads the votes once when a stop and a follow came while the first follow was out', async () => {
    install();
    let answer: ((view: PokerView) => void) | null = null;
    const follows = setBindingMock('FollowTrackerPoker', () => new Promise<PokerView>((resolve) => { answer = resolve; }));
    const reads = setBindingMock('TrackerPoker', async () => votes(5, 2));
    const ticket = attachTrackerTicket(REF, true);
    ticket.setPokerShown(true);
    await flush();
    ticket.setPokerShown(false);
    ticket.setPokerShown(true);
    answer!(votes(null, 1));
    await flush();
    expect(follows.mock.calls).toEqual([['sub-1', true]]);
    expect(reads).toHaveBeenCalledTimes(1);
    expect(ticket.current?.poker).toEqual({ view: votes(5, 2), error: null });
    ticket.release();
  });

  it('sends a refused follow again only once the screens ask again', async () => {
    install();
    const follows = setBindingMock('FollowTrackerPoker', async () => {
      throw new Error('poker is off');
    });
    const ticket = attachTrackerTicket(REF, true);
    ticket.setPokerShown(true);
    await flush();
    await flush();
    expect(follows).toHaveBeenCalledTimes(1);
    expect(errString(ticket.current?.poker?.error)).toBe('poker is off');
    ticket.setPokerShown(false);
    ticket.setPokerShown(true);
    await flush();
    expect(follows.mock.calls).toEqual([['sub-1', true], ['sub-1', false], ['sub-1', true]]);
    ticket.release();
  });

  it('stops following while the window is hidden, and follows again on its return', async () => {
    install();
    const follows = setBindingMock('FollowTrackerPoker', async () => votes(null, 1));
    const ticket = attachTrackerTicket(REF, true);
    ticket.setPokerShown(true);
    await flush();
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'hidden' });
    document.dispatchEvent(new Event('visibilitychange'));
    await flush();
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'visible' });
    document.dispatchEvent(new Event('visibilitychange'));
    await flush();
    expect(follows.mock.calls).toEqual([['sub-1', true], ['sub-1', false], ['sub-1', true]]);
    ticket.release();
  });

  it('reads the votes once per change told, and keeps a newer answer over an older one', async () => {
    install();
    const follows = held<PokerView>('FollowTrackerPoker');
    const reads = held<PokerView>('TrackerPoker');
    const ticket = attachTrackerTicket(REF, true);
    ticket.setPokerShown(true);
    await flush();
    // A change told before the follow answered: its read answers first.
    trackerIssueUpdated(event(11, { poker: true }), HOME_BACKEND);
    trackerIssueUpdated(event(12, { poker: true }), HOME_BACKEND);
    await flush();
    expect(reads.calls).toEqual([['sub-1']]);
    reads.answer(votes(5, 20));
    await flush();
    expect(ticket.current?.poker?.view?.read_at).toBe(20);
    follows.answer(votes(null, 10));
    await flush();
    expect(ticket.current?.poker?.view?.read_at).toBe(20);
    // The second change was told while the first read was out: one more.
    expect(reads.calls.length).toBe(2);
    reads.fail(new Error('the add-on did not answer'));
    await flush();
    expect(ticket.current?.poker).toEqual({ view: votes(5, 20), error: new Error('the add-on did not answer') });
    // A change of the ticket alone reads no votes.
    trackerIssueUpdated(event(13, { changed: true }), HOME_BACKEND);
    await flush();
    expect(reads.calls.length).toBe(2);
    ticket.release();
  });

  it('follows again on a new subscription, and reads the votes again on a gap', async () => {
    const mocks = install();
    const follows = setBindingMock('FollowTrackerPoker', async () => votes(null, 1));
    const reads = setBindingMock('TrackerPoker', async () => votes(3, 2));
    const ticket = attachTrackerTicket(REF, true);
    ticket.setPokerShown(true);
    await flush();
    await trackerTicketsGap(HOME_BACKEND);
    expect(reads).toHaveBeenCalledWith('sub-1');
    expect(ticket.current?.poker?.view?.read_at).toBe(2);
    __setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await flush();
    expect(mocks.subscribe).toHaveBeenCalledTimes(2);
    expect(follows.mock.calls).toEqual([['sub-1', true], ['sub-2', true]]);
    ticket.release();
  });

  it('holds a connection\'s answer whether its site has poker until a reconnect, and a failure only until the next ask', async () => {
    const support = setBindingMock('TrackerPokerSupport', async () => ({ available: false, failure: { kind: 'transient', message: 'Jira did not answer', at: 1 } }));
    expect(trackerPokerSupport(HOME_BACKEND, 'c1')).toBe('unasked');
    askTrackerPokerSupport(HOME_BACKEND, 'c1');
    askTrackerPokerSupport(HOME_BACKEND, 'c1');
    expect(trackerPokerSupport(HOME_BACKEND, 'c1')).toEqual({ asking: true });
    await flush();
    expect(support).toHaveBeenCalledTimes(1);
    expect(trackerPokerSupport(HOME_BACKEND, 'c1')).toEqual({ failed: 'Jira did not answer' });

    support.mockResolvedValue({ available: false });
    askTrackerPokerSupport(HOME_BACKEND, 'c1');
    await flush();
    expect(support).toHaveBeenCalledTimes(2);
    expect(trackerPokerSupport(HOME_BACKEND, 'c1')).toEqual({ available: false });
    askTrackerPokerSupport(HOME_BACKEND, 'c1');
    expect(support).toHaveBeenCalledTimes(2);

    support.mockResolvedValue({ available: true });
    __setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    expect(trackerPokerSupport(HOME_BACKEND, 'c1')).toBe('unasked');
    askTrackerPokerSupport(HOME_BACKEND, 'c1');
    await flush();
    expect(trackerPokerSupport(HOME_BACKEND, 'c1')).toEqual({ available: true });
  });

  it('lets an ask from before a reconnect settle nothing', async () => {
    const answers: Array<(available: boolean) => void> = [];
    setBindingMock('TrackerPokerSupport', () => new Promise((resolve) => { answers.push((available) => resolve({ available })); }));
    askTrackerPokerSupport(HOME_BACKEND, 'c1');
    __setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    askTrackerPokerSupport(HOME_BACKEND, 'c1');
    answers[0]!(false);
    await flush();
    expect(trackerPokerSupport(HOME_BACKEND, 'c1')).toEqual({ asking: true });
    answers[1]!(true);
    await flush();
    expect(trackerPokerSupport(HOME_BACKEND, 'c1')).toEqual({ available: true });
  });
});

describe('the earlier comment pages held', () => {
  // A Jira-like ticket of `total` comments, comment n created at n: the
  // first page is the newest 50, and token T reads the 50 before T.
  const comment = (n: number) => ({ id: String(n), author: { id: 'u' }, body: `Comment ${n}`, created: n });
  const range = (from: number, to: number) => Array.from({ length: to - from + 1 }, (_, i) => comment(to - i));
  const first = (total: number) => detail('x', { comments: range(total - 49, total), more_comments: String(total - 50) } as never);

  it('reads them again from a moved boundary, one read per page held plus one at most, keeping every comment once', async () => {
    let total = 120;
    const mocks = install({ detail: first(total) });
    mocks.read.mockImplementation(async () => ({ loaded: true, detail: first(total), read_at: 200 }) as TicketView);
    let fail = false;
    const pages = setBindingMock('TrackerComments', async (_c: string, _i: string, token: string) => {
      if (fail) throw new TransportError('timeout', 'Jira did not answer in time');
      const end = Number(token);
      return { comments: range(Math.max(0, end - 50) + 1, end), next: end > 50 ? String(end - 50) : '' };
    });
    const ticket = attachTrackerTicket(REF, true);
    await flush();
    await loadEarlierTrackerComments(ticket.key);
    await loadEarlierTrackerComments(ticket.key);
    const shown = () => [...(ticket.current?.detail?.comments ?? []), ...(ticket.current?.earlier.pages ?? []).flatMap((p) => p.comments)].map((c) => Number(c.id));
    expect(shown()).toEqual(range(1, 120).map((c) => c.created));
    expect(pages.mock.calls.map((c) => c[2])).toEqual(['70', '20']);

    // One comment added: the two pages are read again from the new boundary.
    total = 121;
    trackerIssueUpdated(event(11, { changed: true }), HOME_BACKEND);
    await flush();
    expect(pages.mock.calls.map((c) => c[2])).toEqual(['70', '20', '71', '21']);
    expect(shown()).toEqual(range(1, 121).map((c) => c.created));
    expect(ticket.current?.earlier.next).toBe('');

    // A boundary 179 comments on: three reads, then the control reads on.
    total = 300;
    trackerIssueUpdated(event(12, { changed: true }), HOME_BACKEND);
    await flush();
    expect(pages.mock.calls.map((c) => c[2]).slice(4)).toEqual(['250', '200', '150']);
    expect(shown()).toEqual(range(101, 300).map((c) => c.created));
    expect(ticket.current?.earlier.next).toBe('100');

    // A read that fails keeps the pages before it, and the control reads on from it.
    total = 301;
    fail = true;
    trackerIssueUpdated(event(13, { changed: true }), HOME_BACKEND);
    await flush();
    expect(ticket.current?.earlier.pages).toEqual([]);
    expect(ticket.current?.earlier.next).toBe('251');
    expect(ticket.current?.earlier.loading).toBe(false);
    expect(String(ticket.current?.earlier.error)).toContain('Jira did not answer in time');
    fail = false;
    await loadEarlierTrackerComments(ticket.key);
    expect(pages.mock.calls.at(-1)?.[2]).toBe('251');
    expect(shown()).toEqual(range(202, 301).map((c) => c.created));
    ticket.release();
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
    // Its comment box is on screen.
    drawTicketWrite(ticket.key, 'new-comment', () => true);
    const outcome = await addTicketComment(ctxOf(ticket), 'Looks good');
    expect(outcome?.kind).toBe('lost');
    expect(ticketWrite(ticket.key, 'new-comment')?.outcome).toEqual({
      kind: 'lost',
      message: 'This computer: no answer came back, so the change may or may not have been made; look at the ticket before you try again.',
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
    drawTicketWrite(ticket.key, target, () => true);
    const first = editTicketComment(ctxOf(ticket), '7', 'new body', 'old body');
    edits.answer({ parts: [{ step: 'comment', state: 'refused', refusal: { reason: 'loss', message: 'This edit would lose formatting.', lost: ['a table'] } }] } as WriteView);
    expect((await first)?.kind).toBe('refused');
    expect(edits.calls).toEqual([['c1', { issue: '10101', comment: '7', markdown: 'new body', from: 'old body', accept_loss: false }]]);
    const again = ticketWrite(ticket.key, target)?.saveAnyway;
    expect(again).toBeTypeOf('function');
    // A loss refusal of the write that accepted the loss offers no second
    // Save anyway.
    const second = again!();
    edits.answer({ parts: [{ step: 'comment', state: 'refused', refusal: { reason: 'loss', message: 'This edit would lose formatting.', lost: ['a table'] } }] } as WriteView);
    expect((await second)?.kind).toBe('refused');
    expect(edits.calls[1]).toEqual(['c1', { issue: '10101', comment: '7', markdown: 'new body', from: 'old body', accept_loss: true }]);
    expect(ticketWrite(ticket.key, target)?.outcome?.kind).toBe('refused');
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

describe('a write from a Board card', () => {
  const BOARD = { backend: HOME_BACKEND, projectId: 'p1', connection: 'c1', board: '1' };
  const CARD_CTX: TicketWriteContext = { key: trackerTicketKey(REF), ref: REF, label: 'WEB-1' };

  /** A kanban board of one column that holds issue 10101's card. */
  function boardView(title = 'First story'): BoardView {
    return {
      id: '1', lifetime: 1, revision: 1, totals_revision: 1, name: 'WEB board', plans_sprints: false, estimate: { unit: 'points' },
      columns: [{ name: 'To Do', statuses: [{ id: 's1', group: 'todo' }], total: { rows: 1, estimate: 0 } }], sprints: [],
      all: { rows: 1, estimate: 0 }, backlog: { rows: 0, estimate: 0 }, sync: { read_at: 100 }, screen: [{ kind: 'column', column: 0 }],
      sections: [{ section: { kind: 'column', column: 0 }, rows: [{ order: 'a', card: detail(title).card }], next: null }],
    } as unknown as BoardView;
  }

  function installBoard() {
    setBindingMock('SubscribeTrackerBoard', async () => ({ id: 'board-sub', seq: 10, loaded: true, view: boardView(), poll_every: 10_000 }));
    setBindingMock('TrackerBoard', async () => boardView());
    setBindingMock('SetTrackerSubscriptionVisible', async () => {});
    setBindingMock('ReleaseTrackerSubscription', async () => {});
  }

  const refused = (message: string): WriteView => ({ parts: [{ step: 'edit', state: 'refused', refusal: { reason: 'tracker', message } }] }) as WriteView;
  const priority = [{ field: 'priority', value: { kind: 'option', choices: [{ id: '2' }] } as never }];

  it('keeps a card write saying Saving until the board shows it, and its refusal where a shown control draws it', async () => {
    installBoard();
    const patches = held<BoardPatch>('TrackerBoardPatch');
    setBindingMock('EditTrackerIssue', async () => {
      // The write's refresh tells the board's event before the answer.
      trackerBoardUpdated({ scope: { connection: 'c1', board: '1' }, seq: 11, changed: ['10101'] }, HOME_BACKEND);
      return applied();
    });
    const board = attachTrackerBoard(BOARD, true);
    await flush();
    const target = fieldTarget('priority');
    const release = drawTicketWrite(CARD_CTX.key, target, () => true);
    const made = editTicketFields(CARD_CTX, target, priority);
    await flush();
    expect(ticketWrite(CARD_CTX.key, target)?.pending).toBe(true);
    patches.answer({ lifetime: 1, revision: 2, sections: [{ section: { kind: 'column', column: 0 }, rows: [{ order: 'a', card: detail('Saved').card }] }] } as unknown as BoardPatch);
    expect(await made).toEqual({ kind: 'applied', note: null });
    expect(board.current?.held?.sections[0].rows[0].card.title).toBe('Saved');
    expect(ticketWrite(CARD_CTX.key, target)).toBeNull();

    setBindingMock('EditTrackerIssue', async () => refused('Priority is not on the edit screen'));
    const before = getToasts().length;
    expect((await editTicketFields(CARD_CTX, target, priority))?.kind).toBe('refused');
    expect(ticketWrite(CARD_CTX.key, target)?.outcome?.kind).toBe('refused');
    expect(getToasts().length).toBe(before);
    release();
    board.release();
  });

  it('says in a toast, and keeps nowhere, a write that settles while no control of its target is on a shown surface', async () => {
    setBindingMock('EditTrackerIssue', async () => refused('Not allowed'));
    const target = fieldTarget('priority');
    const cases: { name: string; drawers: (() => boolean)[] }[] = [
      { name: 'nothing draws it', drawers: [] },
      { name: 'a control under a covered or hidden surface draws it', drawers: [() => false] },
    ];
    for (const c of cases) {
      const releases = c.drawers.map((shown) => drawTicketWrite(CARD_CTX.key, target, shown));
      const before = getToasts().length;
      expect((await editTicketFields(CARD_CTX, target, priority))?.kind, c.name).toBe('refused');
      expect(getToasts().slice(before).map((t) => t.message), c.name).toEqual(['The tracker refused the change to WEB-1: Not allowed']);
      expect(ticketWrite(CARD_CTX.key, target), c.name).toBeNull();
      for (const release of releases) release();
    }
  });

  it('keeps an outcome while any mounted control of it remains, once on every one, and lets it go with the last, said nowhere', async () => {
    setBindingMock('EditTrackerIssue', async () => refused('Not allowed'));
    const target = fieldTarget('priority');
    // The card under a ticket that covers the board, and the ticket.
    const card = drawTicketWrite(CARD_CTX.key, target, () => false);
    const ticket = drawTicketWrite(CARD_CTX.key, target, () => true);
    const before = getToasts().length;
    await editTicketFields(CARD_CTX, target, priority);
    expect(ticketWrite(CARD_CTX.key, target)?.outcome?.kind).toBe('refused');
    ticket();
    await flush();
    expect(ticketWrite(CARD_CTX.key, target)?.outcome?.kind).toBe('refused');
    // The last control goes and another mounts in the same update: kept.
    card();
    const again = drawTicketWrite(CARD_CTX.key, target, () => true);
    await flush();
    expect(ticketWrite(CARD_CTX.key, target)?.outcome?.kind).toBe('refused');
    again();
    await flush();
    expect(ticketWrite(CARD_CTX.key, target)).toBeNull();
    // Dismissed by the person, it goes while its control stays.
    const shown = drawTicketWrite(CARD_CTX.key, target, () => true);
    await editTicketFields(CARD_CTX, target, priority);
    dismissTicketWrite(CARD_CTX.key, target);
    expect(ticketWrite(CARD_CTX.key, target)).toBeNull();
    shown();
    expect(getToasts().length).toBe(before);
  });

  it('settles into a toast when the control drawing it goes while the write is out', async () => {
    const ranks = held<WriteView>('RankTrackerIssues');
    const place = () => ({ prev: '10102', next: '' });
    let release = drawTicketWrite(CARD_CTX.key, 'rank', () => true);
    const ranked = rankTicket(CARD_CTX, '1', place);
    await flush();
    expect(ranks.calls).toEqual([['c1', { board: '1', issues: ['10101'], prev: '10102', next: undefined }]]);
    release();
    await flush();
    const before = getToasts().length;
    ranks.answer({ parts: [{ step: 'rank', state: 'refused', refusal: { reason: 'tracker', message: 'The neighbor moved' } }] } as WriteView);
    expect((await ranked)?.kind).toBe('refused');
    expect(getToasts().slice(before).map((t) => t.message)).toEqual(['The tracker refused the change to WEB-1: The neighbor moved']);
    expect(ticketWrite(CARD_CTX.key, 'rank')).toBeNull();

    release = drawTicketWrite(CARD_CTX.key, 'rank', () => true);
    const again = rankTicket(CARD_CTX, '1', place);
    ranks.answer({ parts: [{ step: 'rank', state: 'refused', refusal: { reason: 'tracker', message: 'The neighbor moved' } }] } as WriteView);
    await again;
    expect(ticketWrite(CARD_CTX.key, 'rank')?.outcome?.kind).toBe('refused');
    release();
    await flush();
    expect(ticketWrite(CARD_CTX.key, 'rank')).toBeNull();
  });

  it("wakes only the written ticket's readers once that ticket has been written", async () => {
    const other = trackerTicketKey({ ...REF, issue: '10102' });
    setBindingMock('EditTrackerIssue', async () => applied());
    await editTicketFields(CARD_CTX, fieldTarget('priority'), priority);
    let runs = 0;
    const stop = $effect.root(() => {
      $effect(() => {
        void ticketWrite(other, 'status');
        runs += 1;
      });
    });
    flushSync();
    expect(runs).toBe(1);
    const made = editTicketFields(CARD_CTX, fieldTarget('priority'), priority);
    flushSync();
    expect(ticketWrite(CARD_CTX.key, fieldTarget('priority'))?.pending).toBe(true);
    await made;
    flushSync();
    expect(runs).toBe(1);
    stop();
  });

});

describe('attaching files', () => {
  let realFetch: typeof globalThis.fetch;
  beforeEach(() => {
    realFetch = globalThis.fetch;
  });
  afterEach(() => {
    globalThis.fetch = realFetch;
  });

  /** Each PUT waits for the test to answer it, in order. */
  function heldPuts(): { names: string[]; answer: (response: Response) => void; fail: (err: unknown) => void } {
    const waiting: Array<{ resolve: (r: Response) => void; reject: (e: unknown) => void }> = [];
    const names: string[] = [];
    globalThis.fetch = ((input: RequestInfo | URL) => {
      names.push(new URL(String(input), 'http://page.test/').searchParams.get('ticket') ?? '');
      return new Promise<Response>((resolve, reject) => waiting.push({ resolve, reject }));
    }) as typeof globalThis.fetch;
    return { names, answer: (r) => waiting.shift()!.resolve(r), fail: (e) => waiting.shift()!.reject(e) };
  }

  const file = (name: string, bytes: number) => new File([new Uint8Array(bytes)], name);
  const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });

  it('sends one file at a time in the order given, saying which and its place, and takes files given meanwhile', async () => {
    install();
    const mint = setBindingMock('MintTrackerAttachmentUploadTicket', async (_c: string, _i: string, name: string) => `/attachments/tracker/upload?ticket=${name}`);
    const puts = heldPuts();
    const ticket = attachTrackerTicket(REF, true);
    await flush();
    const ctx = ctxOf(ticket);
    attachTicketFiles(ctx, [file('report.pdf', 4_100_000), file('notes.txt', 10)]);
    await flush();
    expect(ticketUploads(ticket.key)).toMatchObject({ name: 'report.pdf', size: 4_100_000, index: 1, total: 2 });
    expect(puts.names).toEqual(['report.pdf']);
    attachTicketFiles(ctx, [file('shot.png', 20)]);
    expect(ticketUploads(ticket.key)).toMatchObject({ name: 'report.pdf', size: 4_100_000, index: 1, total: 3 });
    puts.answer(json(applied()));
    await flush();
    expect(ticketUploads(ticket.key)).toMatchObject({ name: 'notes.txt', size: 10, index: 2, total: 3 });
    expect(puts.names).toEqual(['report.pdf', 'notes.txt']);
    puts.answer(json(applied()));
    await flush();
    puts.answer(json(applied()));
    await flush();
    expect(mint.mock.calls.map((c) => c[2])).toEqual(['report.pdf', 'notes.txt', 'shot.png']);
    expect(ticketUploads(ticket.key)).toBeNull();
    // Each was attached plainly: nothing is left to say.
    expect(ticketAttachOutcomes(ticket.key)).toEqual([]);
    ticket.release();
  });

  it('says what became of each file that was not attached plainly, and still sends the next', async () => {
    install();
    setBindingMock('MintTrackerAttachmentUploadTicket', async (_c: string, _i: string, name: string) => {
      if (name === 'huge.zip') throw new TransportError('tracker_attachment_too_large', 'the tracker stores files up to 2.0 GB, and this one is 3.1 GB; share it another way and link to it');
      return `/attachments/tracker/upload?ticket=${name}`;
    });
    const puts = heldPuts();
    const ticket = attachTrackerTicket(REF, true);
    await flush();
    // The attachments section on screen: the row of the file being sent
    // draws its write, and each outcome's note draws it after.
    const stop = $effect.root(() => {
      $effect(() => {
        const target = ticketUploads(ticket.key)?.target;
        if (target) return drawTicketWrite(ticket.key, target, () => true);
      });
      $effect(() => {
        const releases = ticketAttachOutcomes(ticket.key).map((o) => drawTicketWrite(ticket.key, o.target, () => true));
        return () => releases.forEach((release) => release());
      });
    });
    attachTicketFiles(ctxOf(ticket), [file('huge.zip', 1), file('changed.pdf', 1), file('cut.pdf', 1), file('later.pdf', 1)]);
    await flush();
    // The refused mint sent no byte; the next file went.
    expect(puts.names).toEqual(['changed.pdf']);
    puts.answer(json({ code: 'tracker_upload_size_changed', message: 'the file is now 5 bytes, not the 1 it was when its upload was prepared; select the file again and attach it' }, 400));
    await flush();
    puts.fail(new TypeError('Failed to fetch'));
    await flush();
    puts.answer(json({ parts: [{ step: 'attach', state: 'refused', refusal: { reason: 'tracker', message: 'Jira refused the file' } }] }));
    await flush();
    const outcomes = ticketAttachOutcomes(ticket.key).map((o) => ({ name: o.name, kind: o.outcome.kind, said: 'message' in o.outcome ? o.outcome.message : 'error' in o.outcome ? (o.outcome.error as Error).message : o.outcome.kind === 'refused' ? o.outcome.refusal.message : '' }));
    expect(outcomes).toEqual([
      { name: 'huge.zip', kind: 'failed', said: 'Not sent: the tracker stores files up to 2.0 GB, and this one is 3.1 GB; share it another way and link to it' },
      { name: 'changed.pdf', kind: 'not_sent', said: 'the file is now 5 bytes, not the 1 it was when its upload was prepared; select the file again and attach it' },
      { name: 'cut.pdf', kind: 'unknown', said: 'The file may or may not be attached; look at the ticket\'s attachments before attaching it again.' },
      { name: 'later.pdf', kind: 'refused', said: 'Jira refused the file' },
    ]);
    stop();
    ticket.release();
  });
});

describe('links and poker writes', () => {
  it('links the ticket on the side its phrase reads from, and removes a link by its other side', async () => {
    install();
    const link = setBindingMock('LinkTrackerIssues', async () => applied());
    const unlink = setBindingMock('UnlinkTrackerIssues', async () => applied());
    const ticket = attachTrackerTicket(REF, true);
    await flush();
    const ctx = ctxOf(ticket);
    await linkTicket(ctx, { type: '10000', phrase: 'is blocked by', outward: false }, '10102');
    await unlinkTicket(ctx, { id: '40001', type: '10000', relation: 'blocks', issue: { id: '10106', key: 'WEB-6', title: 'x', status: { id: 's', name: 'To Do' } } } as never);
    expect(link.mock.calls).toEqual([['c1', { type: '10000', outward: '10102', inward: '10101' }]]);
    expect(unlink.mock.calls).toEqual([['c1', { issue: '10101', link: '40001', other: '10106' }]]);
    ticket.release();
  });

  it('never lets a vote, a reveal and a clear overlap', async () => {
    install();
    const votes = held<WriteView>('VoteTrackerPoker');
    const reveal = setBindingMock('RevealTrackerPoker', async () => applied());
    const clear = setBindingMock('ClearTrackerPokerVotes', async () => applied());
    const ticket = attachTrackerTicket(REF, true);
    await flush();
    const ctx = ctxOf(ticket);
    const voted = voteTicketPoker(ctx, 5);
    expect(await revealTicketPoker(ctx)).toBeNull();
    expect(await clearTicketPokerVotes(ctx)).toBeNull();
    expect(votes.calls).toEqual([['c1', { issue: '10101', vote: 5 }]]);
    votes.answer(applied());
    expect(await voted).toEqual({ kind: 'applied', note: null });
    await revealTicketPoker(ctx);
    expect(reveal).toHaveBeenCalledWith('c1', '10101');
    expect(clear).not.toHaveBeenCalled();
    ticket.release();
  });

  it('reads a connection\'s link types once, and again after its computer connects again', async () => {
    const types = setBindingMock('TrackerLinkTypes', async () => [{ id: '10000', name: 'Blocks', outward: 'blocks', inward: 'is blocked by' }]);
    await Promise.all([loadTicketLinkTypes(HOME_BACKEND, 'c1'), loadTicketLinkTypes(HOME_BACKEND, 'c1')]);
    await loadTicketLinkTypes(HOME_BACKEND, 'c1');
    expect(types).toHaveBeenCalledTimes(1);
    expect(ticketLinkTypes(HOME_BACKEND, 'c1')?.types?.map((t) => t.name)).toEqual(['Blocks']);
    __setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    expect(ticketLinkTypes(HOME_BACKEND, 'c1')).toBeNull();
    await loadTicketLinkTypes(HOME_BACKEND, 'c1');
    expect(types).toHaveBeenCalledTimes(2);
  });

  it('lets only the newest read of a connection\'s link types settle them', async () => {
    const answers: Array<(names: string[]) => void> = [];
    setBindingMock('TrackerLinkTypes', () => new Promise((resolve) => {
      answers.push((names) => resolve(names.map((name, i) => ({ id: String(i), name, outward: name, inward: name }))));
    }));
    const before = loadTicketLinkTypes(HOME_BACKEND, 'c1');
    __setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    const after = loadTicketLinkTypes(HOME_BACKEND, 'c1');
    answers[0]!(['Old']);
    await before;
    expect(ticketLinkTypes(HOME_BACKEND, 'c1')).toEqual({ types: null, loading: true, error: null });
    answers[1]!(['New']);
    await after;
    expect(ticketLinkTypes(HOME_BACKEND, 'c1')?.types?.map((t) => t.name)).toEqual(['New']);
  });
});

describe('a move\'s form', () => {
  const RESOLVE = {
    id: '41', name: 'Resolve', to: { id: 's3', name: 'Done' },
    fields: [{ id: 'description', name: 'Description', kind: 'rich_text', editable: true, value: { kind: 'rich_text' } }],
  } as unknown as MoveView;
  const key = trackerTicketKey(REF);

  it('takes its edit bases when the ticket\'s detail first lands, and keeps them after the detail changes', async () => {
    const mocks = install({ detail: detail('First story', { description: 'What the person saw' }) });
    mocks.read.mockImplementation(async () => ({ loaded: true, detail: detail('First story', { description: 'Changed in Jira since' }), read_at: 200 } as TicketView));
    // Opened from a Board card: the ticket is not held yet.
    openTicketMove(key, RESOLVE);
    expect(ticketMoveDraft(key)?.bases).toBeNull();
    const ticket = attachTrackerTicket(REF, true);
    await flush();
    expect(ticketMoveDraft(key)?.bases).toEqual(new Map([['description', 'What the person saw']]));
    trackerIssueUpdated(event(11, { changed: true }), HOME_BACKEND);
    await flush();
    expect(mocks.read).toHaveBeenCalled();
    expect(ticket.current?.detail?.description).toBe('Changed in Jira since');
    expect(ticketMoveDraft(key)?.bases).toEqual(new Map([['description', 'What the person saw']]));
    ticket.release();
  });

  it('takes them at once from a ticket already held', async () => {
    install({ detail: detail('First story', { description: 'Held text' }) });
    const ticket = attachTrackerTicket(REF, true);
    await flush();
    openTicketMove(key, RESOLVE);
    expect(ticketMoveDraft(key)?.bases).toEqual(new Map([['description', 'Held text']]));
    ticket.release();
  });
});
