// The live `SubscribePRUpdates` handle per PR key: at most one, because the
// entity store in prReviewStore.svelte.ts sources a key once however many
// panes hold it. prReviewStore writes it; the RPCs addressed to a
// subscription read it: visibility votes, the CI refresh, and the log
// follow set, which the backend keeps on the handle.

const subscriptionIdByKey = new Map<string, string>();

export function setPRSubscriptionId(key: string, id: string): void {
  subscriptionIdByKey.set(key, id);
}

/** Clears the key's handle only when it is still `id`: a superseded run's
 * late cleanup must not remove the handle the live run installed. */
export function clearPRSubscriptionId(key: string, id: string): void {
  if (subscriptionIdByKey.get(key) === id) subscriptionIdByKey.delete(key);
}

export function prSubscriptionId(key: string): string | null {
  return subscriptionIdByKey.get(key) ?? null;
}

export function prSubscriptions(): IterableIterator<[string, string]> {
  return subscriptionIdByKey.entries();
}

export function __resetPRSubscriptionsForTest(): void {
  subscriptionIdByKey.clear();
}
