import type { Thread } from '../types/models';
import { expandProject } from './sidebar.svelte';

// Presentation placeholders never enter the thread catalog. Requests for the
// same source share progress until the backend publishes their actual rows.
let pending: readonly { source: Thread; count: number }[] = $state([]);

export function pendingForks(projectId: string): readonly Thread[] {
  return pending.filter(entry => entry.source.projectId === projectId).map(entry => entry.source);
}

export async function withForkProgress(
  source: Thread,
  create: () => Promise<Thread>,
): Promise<Thread> {
  const existing = pending.some(entry => entry.source.id === source.id);
  pending = existing
    ? pending.map(entry => entry.source.id === source.id ? { ...entry, count: entry.count + 1 } : entry)
    : [...pending, { source, count: 1 }];
  if (source.projectId) expandProject(source.projectId);
  try {
    return await create();
  } finally {
    pending = pending.flatMap(entry => entry.source.id !== source.id
      ? [entry]
      : entry.count > 1 ? [{ ...entry, count: entry.count - 1 }] : []);
  }
}
