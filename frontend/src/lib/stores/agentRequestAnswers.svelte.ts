import { SvelteMap } from 'svelte/reactivity';
import type { Item } from '../types/models';
import type { ThreadAgentRow } from './bindings';
import { answeredRequestTokens } from '../utils/answeredRequestTokens';

// Whether each request a thread made has its answer in that thread's
// timeline, as the Agents listing (ListThreadAgentRows) last reported it: a
// row's wakeItemId names the confirmed wake row, loaded or not. Kept per
// thread for the session, outside any pane, so a tool row reads it wherever
// the thread is shown. The least recently listed thread goes first.
const MAX_THREADS = 64;

const listed = new SvelteMap<string, ReadonlyMap<string, boolean>>();

/** Records a listing of threadId's agents: its own requests and whether each was answered. */
export function noteListedAgentRows(threadId: string, rows: readonly ThreadAgentRow[]): void {
  const answered = new Map<string, boolean>();
  for (const row of rows) {
    if (row.callerThreadId === threadId) answered.set(row.token, Boolean(row.wakeItemId));
  }
  listed.delete(threadId);
  listed.set(threadId, answered);
  if (listed.size > MAX_THREADS) listed.delete(listed.keys().next().value!);
}

/**
 * Whether the answer to a request threadId made has landed in its timeline.
 * The listing decides every request it covers; one it does not (settled
 * past its window, past its bound, or not read yet) falls back to the wake
 * rows the pane has loaded.
 */
export function requestAnswered(threadId: string | null | undefined, items: readonly Item[] | undefined, token: string): boolean {
  const answered = threadId ? listed.get(threadId)?.get(token) : undefined;
  return answered ?? answeredRequestTokens(items).has(token);
}

export function __resetAgentRequestAnswersForTest(): void {
  listed.clear();
}
