// Sidebar thread filter + multi-select state. Lives outside the sidebar
// component so the command palette can imperatively focus the search box or
// jump to a thread by index even when the sidebar isn't mounted.

import type { Thread } from '../types/models';

let query = $state('');
let includeArchived = $state(false);
let workspaceFilter = $state<string | null>(null);
let selected: Set<string> = $state(new Set());

export function getThreadFilterQuery(): string {
  return query;
}

export function setThreadFilterQuery(next: string): void {
  query = next;
}

export function getIncludeArchived(): boolean {
  return includeArchived;
}

export function setIncludeArchived(next: boolean): void {
  includeArchived = next;
}

export function getWorkspaceFilter(): string | null {
  return workspaceFilter;
}

export function setWorkspaceFilter(next: string | null): void {
  workspaceFilter = next;
}

export function getSelectedThreadIds(): Set<string> {
  return selected;
}

export function isThreadSelected(id: string): boolean {
  return selected.has(id);
}

export function toggleThreadSelection(id: string): void {
  const next = new Set(selected);
  if (next.has(id)) next.delete(id);
  else next.add(id);
  selected = next;
}

export function setThreadSelection(ids: Iterable<string>): void {
  selected = new Set(ids);
}

export function clearThreadSelection(): void {
  if (selected.size === 0) return;
  selected = new Set();
}

/**
 * Apply the current filter settings to a thread list. Pure fn so the sidebar
 * can `$derived` on its own `getThreads()` + this selector.
 */
export function filterThreads(threads: Thread[]): Thread[] {
  const q = query.trim().toLowerCase();
  const ws = workspaceFilter;
  return threads.filter((t) => {
    if (!includeArchived && t.archived) return false;
    if (ws && t.workspacePath !== ws) return false;
    if (q.length === 0) return true;
    const hay = `${t.title ?? ''} ${t.workspacePath ?? ''}`.toLowerCase();
    return hay.includes(q);
  });
}
