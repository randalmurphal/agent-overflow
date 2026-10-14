// The request tokens whose answer has landed in a timeline: the thread
// tools deliver an answer the call did not carry as a user row attributed
// to the thread that answered, stamped with the request's token
// (`meta.originThread.token`, `internal/app/app_thread_tools_wake.go`
// threadWakeOrigin). A spawn, send or ask row reads this to say its answer
// arrived without a request-state read of its own.
//
// One scan per timeline array, shared by every row that asks: the pane's
// `items` is a `$state.raw` array whose identity changes only when its
// membership does.

import type { Item } from '../types/models';
import { parseUserMessageMeta, userMessageOriginTokens } from './userMessageMeta';

const EMPTY: ReadonlySet<string> = new Set();
const memo = new WeakMap<readonly Item[], ReadonlySet<string>>();

// TODO(#95): read answered requests from slice 10's request store
// (ListThreadAgentRows and its change event) once both are merged; this
// scan sees only the rows the pane has loaded.
export function answeredRequestTokens(items: readonly Item[] | undefined): ReadonlySet<string> {
  if (!items || items.length === 0) return EMPTY;
  const cached = memo.get(items);
  if (cached) return cached;
  const tokens = new Set<string>();
  for (const item of items) {
    if (item.kind !== 'user_text') continue;
    // A joined member's token sits in its part's own originThread.
    if (!item.meta?.includes('originThread')) continue;
    for (const token of userMessageOriginTokens(parseUserMessageMeta(item.meta))) tokens.add(token);
  }
  memo.set(items, tokens);
  return tokens;
}
