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
import { parseUserMessageMeta, userMessageOrigin, userMessageOriginThread } from './userMessageMeta';

const EMPTY: ReadonlySet<string> = new Set();
const memo = new WeakMap<readonly Item[], ReadonlySet<string>>();

export function answeredRequestTokens(items: readonly Item[] | undefined): ReadonlySet<string> {
  if (!items || items.length === 0) return EMPTY;
  const cached = memo.get(items);
  if (cached) return cached;
  const tokens = new Set<string>();
  for (const item of items) {
    if (item.kind !== 'user_text' || !item.meta?.includes('originThread')) continue;
    const meta = parseUserMessageMeta(item.meta);
    if (userMessageOrigin(meta) !== 'agent-thread') continue;
    const token = userMessageOriginThread(meta)?.token;
    if (token) tokens.add(token);
  }
  memo.set(items, tokens);
  return tokens;
}
