// The request tokens whose answer has landed among a timeline's loaded rows:
// the thread tools deliver an answer the call did not carry as a user row
// attributed to the thread that answered, stamped with the request's token
// (`meta.originThread.token`, `internal/app/app_thread_tools_wake.go`
// threadWakeOrigin). The fallback of `requestAnswered`
// (stores/agentRequestAnswers.svelte.ts) for a request the Agents listing
// does not cover.
//
// One scan per timeline array, shared by every row that asks: the pane's
// `items` is a `$state.raw` array whose identity changes only when its
// membership does.

import type { Item } from '../types/models';
import { parseUserMessageMeta, userMessageOriginTokens } from './userMessageMeta';

const EMPTY: ReadonlySet<string> = new Set();
const memo = new WeakMap<readonly Item[], ReadonlySet<string>>();

export function answeredRequestTokens(items: readonly Item[] | undefined): ReadonlySet<string> {
  if (!items || items.length === 0) return EMPTY;
  const cached = memo.get(items);
  if (cached) return cached;
  const tokens = new Set<string>();
  for (const item of items) {
    if (item.kind !== 'user_text') continue;
    // A joined member's token sits in its part's own originThread, or in
    // the row's token list when the join recorded no parts.
    if (!item.meta?.includes('originThread') && !item.meta?.includes('joinedOriginTokens')) continue;
    for (const token of userMessageOriginTokens(parseUserMessageMeta(item.meta))) tokens.add(token);
  }
  memo.set(items, tokens);
  return tokens;
}
