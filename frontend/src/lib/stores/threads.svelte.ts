import type { Thread } from '../types/models';
import { ListThreads } from '../../../wailsjs/go/main/App';

let threads: Thread[] = $state([]);

export function getThreads(): Thread[] {
  return threads;
}

export async function refreshThreads(): Promise<void> {
  try {
    threads = await ListThreads();
  } catch (err) {
    console.error('Failed to load threads:', err);
  }
}

export function prependThread(thread: Thread): void {
  threads = [thread, ...threads];
}

export function removeThread(id: string): void {
  threads = threads.filter((t) => t.id !== id);
}

export function updateThreadInList(updated: Thread): void {
  threads = threads.map((t) => (t.id === updated.id ? updated : t));
}
