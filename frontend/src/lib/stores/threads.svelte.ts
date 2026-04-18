import type { Thread } from '../types/models';
import { ListThreads } from './bindings';
import { clearThreadStatus } from './threadStatuses.svelte';
import { addToast } from './toast.svelte';

let threads: Thread[] = $state([]);

export function getThreads(): Thread[] {
  return threads;
}

export async function refreshThreads(): Promise<void> {
  try {
    threads = await ListThreads() as Thread[];
  } catch (err) {
    console.error('Failed to load threads:', err);
    addToast('error', 'Failed to load threads');
  }
}

export function prependThread(thread: Thread): void {
  threads = [thread, ...threads];
}

export function removeThread(id: string): void {
  threads = threads.filter((t) => t.id !== id);
  // Drop any live-status entry so the sidebar doesn't keep painting a
  // dot for a thread that no longer exists in the list.
  clearThreadStatus(id);
}

export function updateThreadTitle(id: string, title: string): void {
  threads = threads.map((t) => t.id === id ? { ...t, title } : t);
}

export function updateThreadModel(id: string, model: string): void {
  threads = threads.map((t) => t.id === id ? { ...t, model } : t);
}

export function replaceThread(thread: Thread): void {
  threads = threads.map((t) => t.id === thread.id ? thread : t);
}

/**
 * Returns the thread with the given id, or undefined if the sidebar doesn't
 * currently track it (e.g. archived parent not in the filtered view).
 */
export function getThreadById(id: string): Thread | undefined {
  return threads.find((t) => t.id === id);
}
