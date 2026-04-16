import type { Thread } from '../types/models';
import { ListThreads } from '../../../bindings/agent-overflow/app.js';

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

export function updateThreadTitle(id: string, title: string): void {
  threads = threads.map((t) => t.id === id ? { ...t, title } : t);
}
