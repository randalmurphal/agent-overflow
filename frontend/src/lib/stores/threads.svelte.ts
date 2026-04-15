import type { Thread } from '../types/models';
import { ListThreads } from '../../../wailsjs/go/main/App';

let threads: Thread[] = $state([]);

export function getThreads(): Thread[] {
  return threads;
}

export async function refreshThreads(): Promise<void> {
  try {
    threads = await ListThreads() as Thread[];
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
