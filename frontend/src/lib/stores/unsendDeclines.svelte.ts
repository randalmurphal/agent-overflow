import { SvelteMap } from 'svelte/reactivity';

// Messages a person's Stop left in place because each joins an agent's
// message nothing can split from it, per thread, for this session. Kept
// outside a pane's row state so leaving the thread and coming back keeps
// them; a reload forgets them. The least recently declined thread goes first.
const MAX_THREADS = 64;
const MAX_PER_THREAD = 64;

const declined = new SvelteMap<string, ReadonlySet<string>>();

export function isUnsendDeclined(threadId: string, messageKey: string): boolean {
  return declined.get(threadId)?.has(messageKey) ?? false;
}

export function noteUnsendDeclined(threadId: string, messageKey: string): void {
  const next = new Set(declined.get(threadId));
  next.delete(messageKey);
  next.add(messageKey);
  if (next.size > MAX_PER_THREAD) next.delete(next.values().next().value!);
  declined.delete(threadId);
  declined.set(threadId, next);
  if (declined.size > MAX_THREADS) declined.delete(declined.keys().next().value!);
}

export function __resetUnsendDeclinesForTest(): void {
  declined.clear();
}
