import { untrack } from 'svelte';
import { SvelteMap } from 'svelte/reactivity';
import type { Item } from '../types/models';
import { GetThreadRequestStates } from './bindings';
import { onThreadAgentRowsChanged, type ThreadAgentRowChange } from './eventsThreadAgentRows';
import { onBackendStatusChange } from './transportStatus.svelte';
import { threadBackend } from '../transport/entityIndex';
import { answeredRequestTokens } from '../utils/answeredRequestTokens';

// Whether the answer to each request a thread made has reached that thread,
// as the request ledger says (GetThreadRequestStates: the confirmed wake row,
// loaded in a pane or not, or the reply of a call that collected the answer
// and so left no wake row). A mounted tool row watches
// its own token; the asks of one task are read in one call per thread, in
// reads of at most MAX_TOKENS, and a change frame naming a token, or the
// thread, a lost frame or its computer reconnecting, reads them again. A
// token is asked from when a row asks, so a frame during its first read
// reads it again; a thread has one read in flight at a time, and what is
// asked meanwhile reads after it, so an older answer never lands last.
// A token the ledger does not
// know, a request from before it, falls back to the answer rows the pane
// has loaded.
//
// Kept per thread for the session, outside any pane. A token a mounted row
// watches stays; past MAX_TOKENS a thread drops its least recently asked
// unwatched token, and past MAX_THREADS the least recently read thread
// nobody watches goes.
const MAX_THREADS = 64;
// The backend's bound on one read (maxThreadRequestStates), and the
// unwatched tokens a thread keeps.
const MAX_TOKENS = 256;

/** What the ledger said: answered or not, or null for a token it does not know. */
type Ledger = boolean | null;

// A row reads its own token's key, which changes only when its answer does.
const answered = new SvelteMap<string, SvelteMap<string, Ledger>>();
// Recency, kept apart so that reordering wakes no reader: threads in the
// order they were last read, each with its tokens in the order last asked.
const recency = new Map<string, Set<string>>();
// The mounted rows watching each token, by thread.
const watched = new Map<string, Map<string, number>>();
// Tokens to read, by thread: until the next microtask, or until the
// thread's read in flight settles.
const due = new Map<string, Set<string>>();
// Threads with a read in flight. One read per thread at a time, so an older
// answer never lands over a newer one; what is asked meanwhile reads after.
const reading = new Set<string>();
// Bumped by a reset, so a read from before it applies nothing.
let epoch = 0;
let unsubscribe: (() => void) | null = null;

/**
 * Whether the answer to a request threadId made has landed in its timeline:
 * the ledger's answer, or for a token it does not know, whether the loaded
 * items hold a row answering it. Unknown until the ledger has been read,
 * which this asks for.
 */
export function requestAnswered(threadId: string | null | undefined, items: readonly Item[] | undefined, token: string): boolean {
  if (!threadId || !token) return false;
  const known = answered.get(threadId)?.get(token);
  if (known === undefined) ask(threadId, [token]);
  return known ?? answeredRequestTokens(items).has(token);
}

/**
 * Keeps threadId's token cached while a mounted row shows it, so rows past
 * MAX_TOKENS do not push each other out. Returns the release.
 */
export function watchRequestAnswer(threadId: string | null | undefined, token: string): () => void {
  if (!threadId || !token) return () => {};
  let thread = watched.get(threadId);
  if (!thread) {
    thread = new Map();
    watched.set(threadId, thread);
  }
  thread.set(token, (thread.get(token) ?? 0) + 1);
  // Read untracked: the watch belongs to the row's mount, not to its answer.
  if (untrack(() => answered.get(threadId)?.get(token)) === undefined) ask(threadId, [token]);
  let released = false;
  return () => {
    if (released) return;
    released = true;
    const count = (thread.get(token) ?? 1) - 1;
    if (count > 0) {
      thread.set(token, count);
      return;
    }
    thread.delete(token);
    if (thread.size === 0 && watched.get(threadId) === thread) watched.delete(threadId);
  };
}

/**
 * Asks for tokens' answers. A token counts as asked from here, before its
 * read starts, so a frame naming it during that read reads it again.
 */
function ask(threadId: string, tokens: readonly string[]): void {
  if (!unsubscribe) {
    const stopFrames = onThreadAgentRowsChanged(readAgain);
    const stopStatus = onBackendStatusChange((backend, status) => {
      if (status.status === 'connected') readComputerAgain(backend);
    });
    unsubscribe = () => {
      stopFrames();
      stopStatus();
    };
  }
  const order = recency.get(threadId) ?? new Set<string>();
  recency.delete(threadId);
  recency.set(threadId, order);
  let set = due.get(threadId);
  if (!set) {
    if (![...due.keys()].some((id) => !reading.has(id))) queueMicrotask(flush);
    set = new Set();
    due.set(threadId, set);
  }
  // Plain maps only: a row asks while its answer is being derived.
  for (const token of tokens) {
    order.delete(token);
    order.add(token);
    set.add(token);
  }
}

function flush(): void {
  for (const [threadId, tokens] of [...due]) {
    if (reading.has(threadId)) continue;
    due.delete(threadId);
    void read(threadId, [...tokens]);
  }
}

/** One read of a thread's tokens, in calls of at most MAX_TOKENS; then what was asked meanwhile. */
async function read(threadId: string, tokens: string[]): Promise<void> {
  const at = epoch;
  reading.add(threadId);
  const call = async (chunk: string[]) => {
    try {
      const states = await GetThreadRequestStates(threadId, chunk);
      if (at === epoch) apply(threadId, chunk, states ?? []);
    } catch (err) {
      // The rows keep what they showed; the next frame, gap or reconnect reads again.
      console.warn('agent request answers: read', threadId, err);
    }
  };
  const calls: Promise<void>[] = [];
  for (let from = 0; from < tokens.length; from += MAX_TOKENS) calls.push(call(tokens.slice(from, from + MAX_TOKENS)));
  await Promise.all(calls);
  if (at !== epoch) return;
  reading.delete(threadId);
  if (due.has(threadId)) flush();
}

function apply(threadId: string, tokens: readonly string[], states: readonly { token: string; answered: boolean }[]): void {
  const order = recency.get(threadId);
  // The thread left the cache while it was read.
  if (!order) return;
  let thread = answered.get(threadId);
  if (!thread) {
    thread = new SvelteMap();
    answered.set(threadId, thread);
  }
  const byToken = new Map(states.map((state) => [state.token, state.answered]));
  for (const token of tokens) {
    // A token trimmed while it was read stays out.
    if (!order.has(token)) continue;
    const state = byToken.get(token) ?? null;
    if (!thread.has(token) || thread.get(token) !== state) thread.set(token, state);
  }
  trim(thread, order, watched.get(threadId));
  for (const id of recency.keys()) {
    if (recency.size <= MAX_THREADS) break;
    if (watched.has(id) || id === threadId) continue;
    answered.delete(id);
    recency.delete(id);
  }
}

/** Drops a thread's least recently asked unwatched tokens past MAX_TOKENS. */
function trim(thread: SvelteMap<string, Ledger>, order: Set<string>, watching: ReadonlyMap<string, number> | undefined): void {
  let unwatched = 0;
  for (const token of order) if (!watching?.has(token)) unwatched++;
  for (const token of order) {
    if (unwatched <= MAX_TOKENS) return;
    if (watching?.has(token)) continue;
    thread.delete(token);
    order.delete(token);
    unwatched--;
  }
}

/** Every cached thread of `backend` reads again: its frames may have been lost, or a read failed while it was away. */
function readComputerAgain(backend: string): void {
  // A snapshot: asking moves each thread to the end of recency.
  for (const [threadId, order] of [...recency]) {
    const owner = threadBackend(threadId);
    if (owner === undefined || owner === backend) ask(threadId, [...order]);
  }
}

function readAgain(change: ThreadAgentRowChange | null, backend: string): void {
  if (!change) {
    readComputerAgain(backend);
    return;
  }
  const order = recency.get(change.threadId);
  if (!order) return;
  if (!change.id) ask(change.threadId, [...order]);
  else if (order.has(change.id)) ask(change.threadId, [change.id]);
}

export function __resetAgentRequestAnswersForTest(): void {
  answered.clear();
  recency.clear();
  watched.clear();
  due.clear();
  reading.clear();
  epoch++;
  unsubscribe?.();
  unsubscribe = null;
}
