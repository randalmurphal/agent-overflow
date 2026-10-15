import { SvelteMap } from 'svelte/reactivity';
import { GetThreadRequestStates } from './bindings';
import { onThreadAgentRowsChanged, type ThreadAgentRowChange } from './eventsThreadAgentRows';

// Whether the answer to each request a thread made has landed in that
// thread's timeline, as the request ledger says (GetThreadRequestStates: the
// confirmed wake row, loaded in a pane or not). A tool row asks for its own
// token; the asks of one task are read in one call per thread, and a change
// frame naming a token, or the thread, or a lost frame, reads them again.
// Kept per thread for the session, outside any pane; the least recently
// asked thread goes first, and within one the least recently asked token.
const MAX_THREADS = 64;
// The backend's bound on one read (maxThreadRequestStates).
const MAX_TOKENS = 256;

const answered = new SvelteMap<string, SvelteMap<string, boolean>>();
// Tokens to read, by thread, until the next microtask.
const due = new Map<string, Set<string>>();
let unsubscribe: (() => void) | null = null;

/**
 * Whether the answer to a request threadId made has landed in its timeline.
 * Unknown until the ledger has been read, which this asks for.
 */
export function requestAnswered(threadId: string | null | undefined, token: string): boolean {
  if (!threadId || !token) return false;
  const known = answered.get(threadId)?.get(token);
  if (known === undefined) ask(threadId, [token]);
  return known ?? false;
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
  const byToken = new Map((states ?? []).map((state) => [state.token, Boolean(state.wakeItemId)]));
  let thread = answered.get(threadId);
  if (thread) {
    answered.delete(threadId);
  } else {
    thread = new SvelteMap();
  }
  answered.set(threadId, thread);
  if (answered.size > MAX_THREADS) answered.delete(answered.keys().next().value!);
  for (const token of tokens) {
    // A token the ledger does not name for this thread has no answer to wait on.
    thread.delete(token);
    thread.set(token, byToken.get(token) ?? false);
  }
  while (thread.size > MAX_TOKENS) thread.delete(thread.keys().next().value!);
}

function readAgain(change: ThreadAgentRowChange | null): void {
  if (!change) {
    for (const [threadId, thread] of answered) ask(threadId, [...thread.keys()]);
    return;
  }
  const thread = answered.get(change.threadId);
  if (!thread) return;
  if (!change.id) ask(change.threadId, [...thread.keys()]);
  else if (thread.has(change.id)) ask(change.threadId, [change.id]);
}

export function __resetAgentRequestAnswersForTest(): void {
  answered.clear();
  due.clear();
  unsubscribe?.();
  unsubscribe = null;
}
