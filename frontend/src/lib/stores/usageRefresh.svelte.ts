// Usage events invalidate persisted queries; no token or cost totals live here.
// Thread listeners exist only while a usage surface is mounted.
let version = $state(0);
const listeners = new Map<string, Set<(error?: string) => void>>();

export function getUsageRefreshVersion(): number { return version; }

export function onThreadUsageRefresh(threadId: string, listener: (error?: string) => void): () => void {
  let entries = listeners.get(threadId);
  if (!entries) { entries = new Set(); listeners.set(threadId, entries); }
  entries.add(listener);
  return () => {
    entries.delete(listener);
    if (!entries.size) listeners.delete(threadId);
  };
}

export function bumpUsageRefresh(threadId: string, error?: string): void {
  version += 1;
  for (const listener of listeners.get(threadId) ?? []) listener(error);
}

export function resetUsageRefreshForTest(): void {
  version = 0;
  listeners.clear();
}
