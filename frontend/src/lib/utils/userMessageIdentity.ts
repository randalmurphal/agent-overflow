import type { Item } from '../types/models';
import { parseUserMessageMeta } from './userMessageMeta';

const identities = new WeakMap<Item, { meta: Item['meta']; threadId: string; value: string | null }>();

/** A sent message keeps its presentation identity when its backend item ID arrives. */
export function userMessageIdentity(item: Item): string | null {
  if (item.kind !== 'user_text' || item.parentId) return null;
  const cached = identities.get(item);
  if (cached && cached.meta === item.meta && cached.threadId === item.threadId) return cached.value;
  const meta = parseUserMessageMeta(item.meta);
  const identity = meta.wire_only !== true && typeof meta.sendId === 'string' && meta.sendId !== ''
    ? `send:${JSON.stringify([item.threadId, meta.sendId])}`
    : null;
  identities.set(item, { meta: item.meta, threadId: item.threadId, value: identity });
  return identity;
}
