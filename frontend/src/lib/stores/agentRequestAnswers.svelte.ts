import { untrack } from 'svelte';
import { SvelteMap } from 'svelte/reactivity';
import type { Item } from '../types/models';
import { GetThreadRequestStates } from './bindings';
import { onThreadAgentRowsChanged, type ThreadAgentRowChange } from './eventsThreadAgentRows';
import { answeredRequestTokens } from '../utils/answeredRequestTokens';

// Whether the answer to each request a thread made has reached that thread,
// as the request ledger says (GetThreadRequestStates: the confirmed wake row,
// loaded in a pane or not, or the reply of a call that collected the answer
// and so left no wake row). A mounted tool row watches
// its own token; the asks of one task are read in one call per thread, in
// reads of at most MAX_TOKENS, and a change frame naming a token, or the
// thread, or a lost frame, reads them again. A token the ledger does not
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
// Tokens to read, by thread, until the next microtask.
const due = new Map<string, Set<string>>();
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

function ask(threadId: string, tokens: readonly string[]): void {
  unsubscribe ??= onThreadAgentRowsChanged(readAgain);
  let set = due.get(threadId);
  if (!set) {
    if (due.size === 0) queueMicrotask(flush);
    set = new Set();
    due.set(threadId, set);
  }
  for (const token of tokens) set.add(token);
}

function flush(): void {
  const batch = [...due];
  due.clear();
  for (const [threadId, tokens] of batch) {
    const list = [...tokens];
    for (let at = 0; at < list.length; at += MAX_TOKENS) void read(threadId, list.slice(at, at + MAX_TOKENS));
  }
}

async function read(threadId: string, tokens: string[]): Promise<void> {
  let states;
  try {
    states = await GetThreadRequestStates(threadId, tokens);
  } catch (err) {
    // The rows keep what they showed; the next frame or gap reads again.
    console.warn('agent request answers: read', threadId, err);
    return;
  }
  const byToken = new Map((states ?? []).map((state) => [state.token, state.answered]));
  let thread = answered.get(threadId);
  if (!thread) {
    thread = new SvelteMap();
    answered.set(threadId, thread);
  }
  const order = recency.get(threadId) ?? new Set<string>();
  recency.delete(threadId);
  recency.set(threadId, order);
  for (const token of tokens) {
    order.delete(token);
    order.add(token);
    const state = byToken.get(token) ?? null;
    if (!thread.has(token) || thread.get(token) !== state) thread.set(token, state);
  }
  trim(thread, order, watched.get(threadId));
  for (const id of recency.keys()) {
    if (recency.size <= MAX_THREADS) break;
    if (watched.has(id)) continue;
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

function readAgain(change: ThreadAgentRowChange | null): void {
  if (!change) {
    for (const [threadId, order] of recency) ask(threadId, [...order]);
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
  unsubscribe?.();
  unsubscribe = null;
}
